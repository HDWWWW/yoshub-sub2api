//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillHealthDurationValidation(t *testing.T) {
	for _, minutes := range []int{-1, 0, 1, 100, 525600, 525601} {
		c := refillTestConfig()
		c.Delivery.ExpiryMode, c.Delivery.HealthDurationMinutes = "health_duration", minutes
		if minutes < 1 || minutes > 525600 {
			require.ErrorContains(t, c.Validate(), "健康时长")
		} else {
			require.NoError(t, c.Validate())
			c.Delivery.AutoPauseOnExpired = false
			require.ErrorContains(t, c.Validate(), "到期自动暂停")
		}
	}
}

func TestRefillHealthDurationSnapshotsAndImportRecovery(t *testing.T) {
	for _, entry := range []string{"manual", "automatic", "daily"} {
		t.Run(entry, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				time.Sleep(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Sub(time.Now()))
				ctx := context.Background()
				s, store, provider, accounts := refillHarness(t)
				store.mutate(t, func(v *accountRefillState) {
					v.Config.Delivery.ExpiryMode, v.Config.Delivery.HealthDurationMinutes = "health_duration", 100
					v.Config.ImportExpiryGraceMinutes = 60
				})
				switch entry {
				case "manual":
					r := manualRequest()
					r.Plan = "pro5x"
					require.NoError(t, s.Manual(ctx, r))
				case "daily":
					seedDailyRefill(t, store, time.Now().Truncate(time.Minute), 1)
				}
				require.NoError(t, s.run(ctx, false))
				state, err := decodeAccountRefillState(store.raw)
				require.NoError(t, err)
				require.Len(t, state.Jobs, 1)
				require.Equal(t, 100, state.Jobs[0].DeliveryConfig.HealthDurationMinutes)
				store.mutate(t, func(v *accountRefillState) {
					v.Config.Delivery.HealthDurationMinutes = 200
					v.Config.Enabled = false
				})
				provider.status = "succeeded"
				accounts.failAfterCreate = true
				started := time.Now()
				for i := 0; i < 5 && len(accounts.accounts) == 0; i++ {
					refillDue(t, store)
					require.NoError(t, s.run(ctx, false))
				}
				require.Len(t, accounts.accounts, 1)
				a := &accounts.accounts[0]
				deadline := a.ExpiresAt.Unix()
				require.Equal(t, started.Add(100*time.Minute).Unix(), deadline, "use the order snapshot without retention")
				require.True(t, a.AutoPauseOnExpired)
				require.Equal(t, true, a.Extra[AccountExpiryManualExtraKey])
				source, origin := a.SourceExpiresAt()
				require.Equal(t, "supplier_api", origin)
				require.Equal(t, started.Add(time.Hour).Unix(), source.Unix(), "keep the supplier lease separate")
				time.Sleep(5 * time.Minute)
				accounts.failAfterCreate = false
				state, err = decodeAccountRefillState(store.raw)
				require.NoError(t, err)
				job := state.Jobs[0]
				require.NoError(t, s.importDelivery(ctx, "fixture", &job))
				require.Equal(t, 1, accounts.creates)
				require.Equal(t, deadline, a.ExpiresAt.Unix(), "recovery must not restart the timer")
				ApplyAccountExpiryExtra(a, map[string]any{AccountSourceExpiryExtraKey: time.Now().Add(3 * time.Hour).Unix()})
				require.Equal(t, deadline, a.ExpiresAt.Unix(), "lease sync must not move the deadline")
				require.True(t, a.isSchedulableAt(time.Unix(deadline-1, 0)))
				require.False(t, a.isSchedulableAt(time.Unix(deadline, 0)))
			})
		})
	}
}

func TestRefillHealthDurationRejectsExpiredSupplierDelivery(t *testing.T) {
	s, _, p, _ := refillHarness(t)
	d, err := p.Delivery(context.Background(), "fixture", "supplier-1")
	require.NoError(t, err)
	lease := time.Now().Add(-time.Minute).Unix()
	d.Accounts[0].ExpiresAt = &lease
	j := &AccountRefillJob{DeliveryConfig: AccountRefillDeliveryConfig{ExpiryMode: "health_duration", HealthDurationMinutes: 100, ProxyMode: "direct"}}
	_, err = s.refillDeliveryInput(context.Background(), j, d)
	require.ErrorContains(t, err, "租期")
	in, err := s.buildRefillDeliveryInput(context.Background(), j, d, true)
	require.NoError(t, err)
	require.NotEqual(t, true, in.Extra[AccountExpiryManualExtraKey], "historical reconciliation must not create a new health period")
}

func TestRefill558669HealthDurationSurvivesCredentialVersionUpdate(t *testing.T) {
	s, store, p, a := harness558669(t)
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Delivery.ExpiryMode, v.Config.Delivery.HealthDurationMinutes = "health_duration", 100
	})
	reserve558669(t, s)
	p.ready = true
	require.NoError(t, s.run(context.Background(), false))
	deadline := a.accounts[0].ExpiresAt.Unix()
	p.version, p.entitlement = 2, "confirmed"
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, a.syncs)
	require.Equal(t, deadline, a.accounts[0].ExpiresAt.Unix())
	require.True(t, a.accounts[0].AutoPauseOnExpired)
	raw, err := json.Marshal(a.accounts[0])
	require.NoError(t, err)
	var reloaded Account
	require.NoError(t, json.Unmarshal(raw, &reloaded))
	require.False(t, reloaded.isSchedulableAt(time.Unix(deadline, 0)))
}

func TestRefillMuzeHealthDurationSurvivesLeaseSyncAndRetry(t *testing.T) {
	s, store, p, accounts := muzeHarness(t)
	s.accounts = &muzeExpiryAccounts{accounts.refillTestAccounts}
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Delivery.ExpiryMode, v.Config.Delivery.HealthDurationMinutes = "health_duration", 100
	})
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, muzeRequest()))
	require.NoError(t, s.run(ctx, false))
	pushMuzeFixture(p, accounts)
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	a := &accounts.accounts[0]
	deadline := a.ExpiresAt.Unix()
	for _, duration := range []time.Duration{3 * time.Hour, 30 * time.Minute} {
		p.cycle.RefundDueAt = time.Now().Add(duration).UTC().Format(time.RFC3339)
		require.NoError(t, s.Check(ctx))
		require.Equal(t, deadline, a.ExpiresAt.Unix())
	}
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.NoError(t, s.importMuzeDelivery(ctx, &state.Jobs[0], p.cycle, true))
	require.Equal(t, 1, accounts.adopt)
	require.Equal(t, deadline, a.ExpiresAt.Unix())
	require.False(t, a.isSchedulableAt(time.Unix(deadline, 0)))
}
