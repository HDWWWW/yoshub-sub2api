//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestInternalExpiryRankingPreservesNativeFieldsAndSharedCandidates(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	soon, later, past := now.Add(time.Hour), now.Add(2*time.Hour), now.Add(-time.Hour)
	accounts := []Account{
		{ID: 1, Priority: 30, ExpiresAt: &soon},
		{ID: 2, Priority: 1, ExpiresAt: &soon},
		{ID: 3, Priority: 0, ExpiresAt: &later},
		{ID: 4, Priority: 0}, {ID: 5, Priority: 10},
		{ID: 6, Priority: -1, ExpiresAt: &past},
	}
	before, err := json.Marshal(accounts)
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
	ranked := accountsWithExpiryPriority(accounts, cfg, now)
	for i, want := range []int{1, 1, 2, 3, 4, 5} {
		require.Equal(t, want, ranked[i].SchedulingPriority())
		require.Zero(t, accounts[i].schedulingPriority)
		require.Equal(t, accounts[i].Priority, ranked[i].Priority)
	}
	after, err := json.Marshal(ranked)
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after), "internal ranking must not appear in serialized accounts")
	cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = false
	manual := accountsWithExpiryPriority(accounts, cfg, now)
	require.Equal(t, 30, manual[0].SchedulingPriority())
	cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
	boundary := accountsWithExpiryPriority(accounts, cfg, soon)
	require.Less(t, boundary[2].SchedulingPriority(), boundary[0].SchedulingPriority())
}

func TestImportGraceControlsSchedulingWithoutChangingExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	expiry := now.Add(-30 * time.Minute)
	a := Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Schedulable: true, ExpiresAt: &expiry, AutoPauseOnExpired: true,
		Extra: map[string]any{AccountImportExpiryGraceExtraKey: 3600}}
	require.True(t, a.IsSchedulable())
	require.True(t, a.IsCredentialUsableForShadow())
	require.Equal(t, expiry.Add(time.Hour), *a.SchedulingExpiresAt())
	require.Equal(t, expiry, *a.ExpiresAt)
	require.True(t, a.isSchedulingExpired(expiry.Add(time.Hour)))
	a.AutoPauseOnExpired = false
	require.False(t, a.isSchedulingExpired(expiry.Add(2*time.Hour)))
	a.AutoPauseOnExpired = true
	for _, bad := range []any{nil, -1, 86401, 100000, "oops", "1e3", " 3600", "1.5", true} {
		a.Extra[AccountImportExpiryGraceExtraKey] = bad
		require.False(t, a.IsSchedulable(), "malformed grace %v", bad)
	}
	a.Extra[AccountImportExpiryGraceExtraKey] = 3600
	a.Platform = PlatformAnthropic
	require.False(t, a.IsSchedulable())
	a.Platform, a.Type = PlatformOpenAI, AccountTypeAPIKey
	require.False(t, a.IsSchedulable())
	a.ExpiresAt = nil
	require.True(t, a.IsSchedulable())
}

func TestNativeAccountUpdatePreservesPrivateGraceAndReplacesPublicExtra(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	repo := &updateAccountOveragesRepoStub{account: &Account{ID: 1, Platform: PlatformOpenAI,
		Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Priority: 99, ExpiresAt: &expiry,
		Extra: map[string]any{AccountImportExpiryGraceExtraKey: 3600, "old": true}}}
	priority := 7
	updated, err := (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), 1, &UpdateAccountInput{
		Priority: &priority, Extra: map[string]any{"supplier": "value", AccountImportExpiryGraceExtraKey: 0},
	})
	require.NoError(t, err)
	require.Equal(t, priority, updated.Priority)
	require.Equal(t, expiry, *updated.ExpiresAt)
	require.Equal(t, 3600, updated.Extra[AccountImportExpiryGraceExtraKey])
	require.NotContains(t, updated.Extra, "old")
	require.Equal(t, "value", NativeAccountExtra(updated.Extra)["supplier"])
	require.NotContains(t, NativeAccountExtra(updated.Extra), AccountImportExpiryGraceExtraKey)
	updated, err = (&adminServiceImpl{accountRepo: repo}).UpdateAccount(context.Background(), 1, &UpdateAccountInput{Extra: map[string]any{}})
	require.NoError(t, err)
	require.NotContains(t, NativeAccountExtra(updated.Extra), "supplier")
	require.Equal(t, 3600, updated.Extra[AccountImportExpiryGraceExtraKey])
}

func TestNativeExtraMergeCannotOverwritePrivateGrace(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{AccountImportExpiryGraceExtraKey: 3600}},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	require.NoError(t, svc.UpdateAccountExtra(context.Background(), 1, map[string]any{AccountImportExpiryGraceExtraKey: 0, "supplier": true}))
	require.Equal(t, 3600, repo.accounts[1].Extra[AccountImportExpiryGraceExtraKey])
	require.Equal(t, true, repo.accounts[1].Extra["supplier"])
}
