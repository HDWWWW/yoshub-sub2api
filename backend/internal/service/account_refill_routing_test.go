//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the group worker with a 1208 wallet that actually quotes Pro20x.
func reverseRefillHarness(t *testing.T) (*AccountRefillService, *refillTestStore, *muzeTestProvider, *failureRefillProvider, *muzeTestAdmin) {
	t.Helper()
	s, store, muze, accounts := fallbackHarness(t)
	wallet := &failureRefillProvider{manualRefillProvider: &manualRefillProvider{s.provider.(*refillTestProvider), 500}, states: map[string]string{}}
	s.provider = wallet
	store.mutate(t, func(v *accountRefillState) {
		v.Config.PrimaryProvider = refillProviderMuze
		v.Config.Plan = "pro20x"
		v.Config.MuzeFallback.FailureThreshold = 1
		v.EncryptedToken = "1208-cipher"
		v.ProviderRetryAt = time.Time{}
		v.NextCheckAt = time.Time{}
	})
	return s, store, muze, wallet, accounts
}

func TestRefillMuzePrimaryRejectionRoutesOnceTo1208(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, _ := reverseRefillHarness(t)
	muze.orderError = &muzeHTTPError{remote: &refillProviderError{code: 400}, code: "insufficient_stock", message: "stock", rejection: true}
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, muze.orders)
	require.Empty(t, wallet.orders)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, wallet.orders, 1)
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	backup, primary := v.Jobs[0], v.Jobs[1]
	require.Empty(t, backup.Provider)
	require.Equal(t, "pro20x", backup.Plan)
	require.Equal(t, "1208自动备用补号", backup.Reason)
	require.Equal(t, primary.ID, backup.FallbackForJobID)
	require.Equal(t, backup.ID, primary.MuzeFallbackJobID)
	usage := v.Usage[backup.CreatedAt.In(refillTimezone).Format("2006-01-02")]
	require.Equal(t, 2, usage.Count)
	require.Equal(t, 60.0, usage.Points)
	for i := 0; i < 3; i++ {
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, wallet.orders, 1)
	require.Equal(t, 1, muze.orders)
}

func TestRefillMuzePrimaryRoleSwapLeavesAcceptedOrderAtMuze(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, accounts := reverseRefillHarness(t)
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, muze.orders)
	require.Empty(t, wallet.orders)
	v, err := s.View(ctx)
	require.NoError(t, err)
	id := v.Jobs[0].ID
	c := v.Config
	c.PrimaryProvider = "1208"
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	pushMuzeFixture(muze, accounts)
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "1208", v.Config.PrimaryProvider)
	require.Equal(t, id, v.Jobs[0].ID)
	require.Equal(t, "imported", v.Jobs[0].Status)
	require.Equal(t, 1, accounts.adopt)
	require.Equal(t, 1, muze.orders)
	require.Empty(t, wallet.orders)
}

func TestRefillMuzePrimaryStallRoutesOnceTo1208(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, _ := reverseRefillHarness(t)
	require.NoError(t, s.run(ctx, false))
	store.mutate(t, func(v *accountRefillState) {
		v.Jobs[0].CreatedAt = time.Now().Add(-6 * time.Minute)
		v.Muze.RetryAt = time.Now().Add(time.Hour)
	})
	require.NoError(t, s.run(ctx, false))
	require.Len(t, wallet.orders, 1)
	require.Equal(t, 1, muze.orders)
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, v.Jobs[1].ID, v.Jobs[0].FallbackForJobID)
	require.True(t, v.Jobs[1].pending())
	require.NoError(t, s.run(ctx, false))
	require.Len(t, wallet.orders, 1)
}

func TestRefillMuzeShortageThresholdSurvivesTicksAndFailedBackup(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, _ := reverseRefillHarness(t)
	muze.available = 0
	store.mutate(t, func(v *accountRefillState) { v.Config.MuzeFallback.FailureThreshold = 2 })
	require.NoError(t, s.run(ctx, false))
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Empty(t, wallet.orders)
	store.mutate(t, func(v *accountRefillState) {
		require.Equal(t, 1, v.MuzeFallback.Shortage.Checks)
		v.MuzeFallback.Shortage.LastAt = time.Now().Add(-time.Minute)
		v.NextCheckAt = time.Time{}
	})
	require.NoError(t, s.run(ctx, false))
	require.Len(t, wallet.orders, 1)
	require.Zero(t, muze.orders)
	// A second worker sees the same receipt both while pending and after definitive failure.
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	wallet.states[wallet.orders[0].ID] = "failed"
	for i := 0; i < 3; i++ {
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
	}
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.NotNil(t, v.MuzeFallback.Shortage)
	require.Equal(t, wallet.orders[0].ID, v.MuzeFallback.Shortage.BackupJobID)
	require.Len(t, wallet.orders, 1)
	muze.available = 2
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, muze.orders)
	require.Len(t, wallet.orders, 1)
}

func TestRefillPrimaryLocalReservationsDoNotTriggerFailover(t *testing.T) {
	ctx := context.Background()
	for _, provider := range []string{"1208", "muze"} {
		t.Run(provider, func(t *testing.T) {
			s, store, muze, wallet, _ := reverseRefillHarness(t)
			muze.available = 1
			v, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			v.Config.PrimaryProvider = provider
			v.Catalog = &RefillCatalog{Available: 60, Items: []RefillQuote{{Service: "refill", Plan: "pro20x", Price: 60, Enabled: true}}}
			v.Jobs = []AccountRefillJob{{ID: "reserved", Provider: v.Config.automaticProvider(), Plan: "pro20x", Price: 60, Status: "submitting"}}
			v.MuzeFallback.Shortage = &refillSupplyShortage{Plan: "pro20x", FirstAt: time.Now().Add(-time.Hour), Checks: 2}
			_, _, backup, err := s.automaticQuote(ctx, &v, "pro20x", time.Now(), func() error { return nil })
			require.ErrorContains(t, err, "本地预占")
			require.False(t, backup)
			require.Nil(t, v.MuzeFallback.Shortage)
			require.Empty(t, wallet.orders)
			require.Zero(t, muze.orders)
		})
	}
}

func TestRefillMuzeDailyUsesSavedSupplierAfterSwitch(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, _ := reverseRefillHarness(t)
	due := time.Now().Truncate(time.Minute)
	seedDailyRefill(t, store, due, 1)
	require.NoError(t, s.run(ctx, false))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 1)
	j := v.Jobs[0]
	require.Equal(t, "muze", j.Provider)
	require.NotEmpty(t, j.DailyBatchID)
	require.NotEmpty(t, j.MuzeConnectionID)
	c := v.Config
	c.PrimaryProvider = "1208"
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, muze.orders)
	require.Empty(t, wallet.orders)
}

func TestRefillRetiredOrdersMigrateWithoutCredentialsOrReplay(t *testing.T) {
	ctx := context.Background()
	s, store, muze, _ := muzeHarness(t)
	var disk map[string]any
	require.NoError(t, json.Unmarshal([]byte(store.raw), &disk))
	config := disk["config"].(map[string]any)
	delete(config, "primary_provider")
	delete(config, "muze_fallback")
	config["fendudu_fallback"] = map[string]any{"enabled": true, "failure_threshold": 4, "timeout_minutes": 9}
	delete(disk, "muze")
	delete(disk, "muze_fallback")
	disk["fendudu"] = map[string]string{"encrypted_token": "retired-only-secret", "connection_id": "old-destination"}
	disk["jobs"] = []map[string]any{{"id": "old", "provider": "fendudu", "status": "processing", "supplier_id": "old-order", "supplier_expires_at": 1730000000}, {"id": "original", "status": "failed", "fendudu_fallback_job_id": "old"}}
	raw, err := json.Marshal(disk)
	require.NoError(t, err)
	store.raw = string(raw)
	view, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "1208", view.Config.PrimaryProvider)
	require.False(t, view.Muze.TokenConfigured)
	require.True(t, view.Config.MuzeFallback.Enabled)
	require.Equal(t, 4, view.Config.MuzeFallback.FailureThreshold)
	require.Error(t, s.Retry(ctx, "old"))
	require.NoError(t, s.DismissRejected(ctx, "old"))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, "dismissed", v.Jobs[0].Status)
	require.Equal(t, int64(1730000000), *v.Jobs[0].SupplierExpiresAt)
	require.Equal(t, "old", v.Jobs[1].MuzeFallbackJobID)
	require.NotContains(t, store.raw, "retired-only-secret")
	require.Zero(t, muze.orders)
}

// Distinguish the installation's ciphertext from an explicitly configured wallet.
type routeDefaultStore struct {
	*refillGroupedTestStore
	cipher string
}

func (s *routeDefaultStore) ReadMuzeDefaultToken(context.Context) (string, error) {
	return s.cipher, nil
}

type routeTestCipher struct{ refillTestEncryptor }

func (routeTestCipher) Decrypt(cipher string) (string, error) {
	switch cipher {
	case "default-wallet":
		return muzeFixtureToken, nil
	case "override-wallet":
		return "tk_" + strings.Repeat("b", 64), nil
	}
	return "", errors.New("unexpected ciphertext")
}

func TestRefillMuzeDefaultCredentialAndGroupOverride(t *testing.T) {
	s, base, _, _ := muzeHarness(t)
	group := &refillTestStore{raw: base.raw}
	group.mutate(t, func(v *accountRefillState) {
		v.Config.GroupID = 3
		v.Muze = muzeRefillState{EncryptedToken: "override-wallet", ConnectionID: "own"}
	})
	base.mutate(t, func(v *accountRefillState) { v.Muze = muzeRefillState{} })
	root := &routeDefaultStore{&refillGroupedTestStore{base, map[int64]*refillTestStore{3: group}}, "default-wallet"}
	s.store = root
	s.encryptor = routeTestCipher{}
	ctx := context.Background()
	for _, tc := range []struct {
		id             int64
		cipher, idWant string
	}{{0, "default-wallet", ""}, {3, "override-wallet", "own"}, {4, "default-wallet", ""}} {
		svc := s
		if tc.id != 0 {
			svc = s.groupService(tc.id, root)
		}
		raw, err := svc.store.Read(ctx)
		require.NoError(t, err)
		state, err := svc.decodeRefillWithMuze(ctx, raw)
		require.NoError(t, err)
		require.Equal(t, tc.cipher, state.Muze.EncryptedToken)
		require.Equal(t, tc.idWant, state.Muze.ConnectionID)
		view, err := svc.View(ctx)
		require.NoError(t, err)
		require.True(t, view.Muze.TokenConfigured)
		encoded, err := json.Marshal(view)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), tc.cipher)
		require.NotContains(t, string(encoded), muzeFixtureToken)
	}
}

type muzePushLookupTestRepo struct {
	*refillTestAccounts
	groups  []int64
	subject string
	since   time.Time
	rows    []*Account
}

func (r *muzePushLookupTestRepo) FindRefillPushCandidates(_ context.Context, groups []int64, subject string, since time.Time) ([]*Account, error) {
	r.groups, r.subject, r.since = groups, subject, since
	return r.rows, nil
}

func TestRefillMuzePushWithoutSub2IDUsesUniqueLocalIdentity(t *testing.T) {
	ctx := context.Background()
	s, store, muze, accounts := muzeHarness(t)
	repo := &muzePushLookupTestRepo{refillTestAccounts: accounts.refillTestAccounts}
	s.accounts = repo
	require.NoError(t, s.Manual(ctx, muzeRequest()))
	require.NoError(t, s.run(ctx, false))
	pushMuzeFixture(muze, accounts)
	muze.cycle.AccountID = ""
	muze.cycle.Subject = "upstream-only"
	account := &accounts.accounts[0]
	account.Credentials["chatgpt_account_id"] = muze.cycle.Subject
	account.Status = "inactive"
	account.Schedulable = false
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	j := v.Jobs[0]
	require.Equal(t, "processing", j.Status)
	require.Equal(t, j.DeliveryGroupIDs, repo.groups)
	require.Equal(t, muze.cycle.Subject, repo.subject)
	require.True(t, repo.since.Equal(j.SubmissionStartedAt.Add(-30*time.Second)))
	repo.rows = []*Account{account, account}
	_, err = s.muzePushedAccount(ctx, &j, muze.cycle)
	require.ErrorContains(t, err, "多个本地账号")
	wrong := *account
	wrong.Credentials = map[string]any{"chatgpt_account_id": muze.cycle.Subject, "email": "unrelated@example.test"}
	repo.rows = []*Account{&wrong}
	_, err = s.muzePushedAccount(ctx, &j, muze.cycle)
	require.ErrorContains(t, err, "身份不匹配")
	repo.rows = []*Account{account}
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, "imported", v.Jobs[0].Status)
	require.Equal(t, account.ID, v.Jobs[0].AccountID)
	require.Equal(t, 1, accounts.adopt)
	require.Equal(t, 1, muze.orders)
}

func TestRefill1208UnavailableRoutesToMuzeInPrepaidUnits(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, _ := reverseRefillHarness(t)
	wallet.available = 0
	store.mutate(t, func(v *accountRefillState) { v.Config.PrimaryProvider = "1208"; v.Config.Plan = "pro5x" })
	require.NoError(t, s.run(ctx, false))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 1)
	j := v.Jobs[0]
	require.Equal(t, "pro20x", j.Plan)
	require.True(t, j.SupplyFallback)
	require.Equal(t, "muze", j.Provider)
	require.Empty(t, wallet.orders)
	require.Equal(t, 1, muze.orders)
	require.Zero(t, v.Usage[j.CreatedAt.In(refillTimezone).Format("2006-01-02")].Points)
}
