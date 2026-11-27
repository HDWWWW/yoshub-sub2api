//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func riskFallbackHarness() (*AccountRiskService, *riskTestStore, *riskTestAccounts, time.Time) {
	s, store, accounts, _, now := riskHarness()
	store.state.Rules[7].Config.APIFallback = &AccountRiskAPIFallbackConfig{Enabled: true, AccountID: 99}
	store.state.Rules[7].Config.ProbeEnabled = false
	accounts.pool = []Account{{ID: 99, Name: "API backup", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, GroupIDs: []int64{7}, Concurrency: 100}}
	return s, store, accounts, now
}

func TestRiskAPIFallbackRecoveryAndRestart(t *testing.T) {
	s, store, accounts, now := riskFallbackHarness()
	ctx := context.Background()
	require.NoError(t, s.reconcileAPIFallbacks(ctx, now))
	require.True(t, accounts.pool[0].Schedulable)
	require.Equal(t, "active", store.state.Rules[7].APIFallback.State)
	require.Zero(t, store.state.Rules[7].APIFallback.Healthy)
	require.NotEmpty(t, store.state.APIFallbackLeases[99].Token)
	state, err := store.Read(ctx)
	require.NoError(t, err)
	store.state = state
	restarted := &AccountRiskService{store: store, accounts: accounts, groups: s.groups}
	newAccount := refillTestAccount(now.Add(time.Hour))
	newAccount.GroupIDs = []int64{7}
	newAccount.Schedulable = false
	accounts.pool = append(accounts.pool, newAccount)
	require.NoError(t, restarted.reconcileAPIFallbacks(ctx, now.Add(time.Second)))
	require.True(t, accounts.pool[0].Schedulable, "an unusable new account must not end fallback")
	accounts.pool[1].Schedulable = true
	require.NoError(t, restarted.reconcileAPIFallbacks(ctx, now.Add(2*time.Second)))
	require.False(t, accounts.pool[0].Schedulable)
	require.NotContains(t, accounts.pool[0].Extra, AccountRiskAPIFallbackExtraKey)
	require.Empty(t, store.state.APIFallbackLeases)
	require.Equal(t, "standby", store.state.Rules[7].APIFallback.State)
	require.Equal(t, 1, store.state.Rules[7].APIFallback.Healthy)
}

func TestRiskAPIFallbackSharedGroups(t *testing.T) {
	s, store, accounts, now := riskFallbackHarness()
	ctx := context.Background()
	c := store.state.Rules[7].Config
	store.state.Rules[8] = &AccountRiskRule{GroupID: 8, Config: c}
	accounts.pool[0].GroupIDs = []int64{7, 8}
	require.NoError(t, s.reconcileAPIFallbacks(ctx, now))
	newAccount := refillTestAccount(now.Add(time.Hour))
	newAccount.GroupIDs = []int64{7}
	accounts.pool = append(accounts.pool, newAccount)
	require.NoError(t, s.reconcileAPIFallbacks(ctx, now.Add(time.Second)))
	require.True(t, accounts.pool[0].Schedulable, "group 8 still needs the shared backup")
	accounts.pool[1].GroupIDs = []int64{7, 8}
	require.NoError(t, s.reconcileAPIFallbacks(ctx, now.Add(2*time.Second)))
	require.False(t, accounts.pool[0].Schedulable)
}

func TestRiskAPIFallbackRespectsManualPauseAndUnavailableAccount(t *testing.T) {
	s, store, accounts, now := riskFallbackHarness()
	ctx := context.Background()
	require.NoError(t, s.reconcileAPIFallbacks(ctx, now))
	accounts.pool[0].Schedulable = false
	accounts.pool[0].Status = StatusError
	require.NoError(t, s.reconcileAPIFallbacks(ctx, now.Add(time.Second)))
	accounts.pool[0].Status = StatusActive
	require.NoError(t, s.reconcileAPIFallbacks(ctx, now.Add(2*time.Second)))
	require.False(t, accounts.pool[0].Schedulable)
	require.True(t, store.state.APIFallbackLeases[99].Paused)
	store.state.Rules[7].Config.Enabled = false
	require.NoError(t, s.reconcileAPIFallbacks(ctx, now.Add(3*time.Second)))
	require.Empty(t, store.state.APIFallbackLeases)
}

func TestRiskAPIFallbackDisablingAndSwitching(t *testing.T) {
	for _, change := range []string{"rule", "fallback", "target", "group removed", "group disabled"} {
		t.Run(change, func(t *testing.T) {
			s, store, accounts, now := riskFallbackHarness()
			ctx := context.Background()
			other := accounts.pool[0]
			other.ID = 100
			accounts.pool = append(accounts.pool, other)
			require.NoError(t, s.reconcileAPIFallbacks(ctx, now))
			switch change {
			case "rule":
				store.state.Rules[7].Config.Enabled = false
			case "fallback":
				store.state.Rules[7].Config.APIFallback.Enabled = false
			case "target":
				store.state.Rules[7].Config.APIFallback.AccountID = 100
			case "group removed":
				accounts.pool[0].GroupIDs = nil
			case "group disabled":
				s.groups.(*riskTestGroups).inactive = true
			}
			require.NoError(t, s.reconcileAPIFallbacks(ctx, now.Add(time.Second)))
			require.False(t, accounts.pool[0].Schedulable)
			require.Equal(t, change == "target", accounts.pool[1].Schedulable)
		})
	}
}

func TestRiskAPIFallbackHealthExcludesAPIShadowsAndUnavailableOAuth(t *testing.T) {
	for _, kind := range []string{"api", "shadow", "paused", "expired", "cooldown", "rate limited", "overloaded", "quota", "supplier expired", "wrong group"} {
		t.Run(kind, func(t *testing.T) {
			s, store, accounts, now := riskFallbackHarness()
			a := refillTestAccount(now.Add(time.Hour))
			a.GroupIDs = []int64{7}
			until, past := now.Add(time.Hour), now.Add(-time.Hour)
			switch kind {
			case "api":
				a.Type = AccountTypeAPIKey
			case "shadow":
				id := int64(5)
				a.ParentAccountID = &id
			case "paused":
				a.Schedulable = false
			case "expired":
				a.ExpiresAt = &past
				a.AutoPauseOnExpired = true
			case "cooldown":
				a.TempUnschedulableUntil = &until
			case "rate limited":
				a.RateLimitResetAt = &until
			case "overloaded":
				a.OverloadUntil = &until
			case "quota":
				a.Extra = map[string]any{"codex_7d_used_percent": 100.0, "codex_7d_reset_at": until.Format(time.RFC3339)}
			case "supplier expired":
				a.Extra = map[string]any{AccountSourceExpiryExtraKey: past.Unix()}
			case "wrong group":
				a.GroupIDs = []int64{8}
			}
			accounts.pool = append(accounts.pool, a)
			require.NoError(t, s.reconcileAPIFallbacks(context.Background(), now))
			require.True(t, accounts.pool[0].Schedulable)
			require.Zero(t, store.state.Rules[7].APIFallback.Healthy)
		})
	}
}

func TestRiskAPIFallbackNotifyOnlyDoesNotChangeHealth(t *testing.T) {
	s, store, accounts, now := riskFallbackHarness()
	a := refillTestAccount(now.Add(time.Hour))
	a.GroupIDs = []int64{7}
	accounts.pool = append(accounts.pool, a)
	store.state.Rules[7].Config.ProbeNotifyOnly = true
	store.state.Rules[7].Events = []AccountRiskEvent{{AccountID: a.ID, Action: "notify_only", Kind: "all_probe", Reason: "reply mismatch"}}
	require.NoError(t, s.reconcileAPIFallbacks(context.Background(), now))
	require.False(t, accounts.pool[0].Schedulable)
	require.Equal(t, 1, store.state.Rules[7].APIFallback.Healthy)
}

func TestRiskAPIFallbackPreservesExistingEnabledAccount(t *testing.T) {
	s, store, accounts, now := riskFallbackHarness()
	accounts.pool[0].Schedulable = true
	require.NoError(t, s.reconcileAPIFallbacks(context.Background(), now))
	require.Empty(t, store.state.APIFallbackLeases)
	require.Equal(t, "manual", store.state.Rules[7].APIFallback.State)
	store.state.Rules[7].Config.Enabled = false
	require.NoError(t, s.reconcileAPIFallbacks(context.Background(), now))
	require.True(t, accounts.pool[0].Schedulable)
}

func TestRiskAPIFallbackFailuresAndConfiguration(t *testing.T) {
	s, store, accounts, now := riskFallbackHarness()
	ctx := context.Background()
	accounts.err = errors.New("offline")
	require.Error(t, s.reconcileAPIFallbacks(ctx, now))
	require.False(t, accounts.pool[0].Schedulable)
	accounts.err = nil
	store.cas = false
	require.ErrorIs(t, s.reconcileAPIFallbacks(ctx, now), ErrAccountRiskConflict)
	require.Empty(t, store.state.APIFallbackLeases)
	store.cas = true
	view, err := s.View(ctx, 7)
	require.NoError(t, err)
	require.Len(t, view.APIFallbackAccounts, 1)
	require.False(t, accounts.pool[0].Schedulable, "GET cannot enable fallback")
	c := store.state.Rules[7].Config
	c.APIFallback = nil
	require.NoError(t, s.Configure(ctx, 7, 1, c))
	require.True(t, store.state.Rules[7].Config.APIFallback.Enabled, "old clients preserve fallback")
	c.APIFallback = &AccountRiskAPIFallbackConfig{Enabled: true, AccountID: 123}
	require.Error(t, s.Configure(ctx, 7, 2, c))
	c.APIFallback.AccountID = 0
	require.Error(t, c.Validate())
}

func TestRiskAPIFallbackOnlyEditPreservesPendingCapacityRecovery(t *testing.T) {
	s, store, _, _ := riskFallbackHarness()
	store.state.CapacityPauses = map[int64]*AccountRiskCapacityPause{1: {AccountID: 1, GroupID: 7, Revision: 1, Token: "pending"}}
	c := store.state.Rules[7].Config
	c.APIFallback = &AccountRiskAPIFallbackConfig{Enabled: false, AccountID: 99}
	require.NoError(t, s.Configure(context.Background(), 7, 1, c))
	require.Equal(t, int64(2), store.state.CapacityPauses[1].Revision)
	require.Equal(t, "pending", store.state.CapacityPauses[1].Token)
}

func TestRiskAPIFallbackDoesNotOverrideTargetRestrictions(t *testing.T) {
	for _, kind := range []string{"oauth", "shadow", "error", "expired", "cooldown", "rate limited", "overload", "capacity pause", "wrong group"} {
		t.Run(kind, func(t *testing.T) {
			s, _, accounts, now := riskFallbackHarness()
			a := &accounts.pool[0]
			until, past := now.Add(time.Hour), now.Add(-time.Hour)
			switch kind {
			case "oauth":
				a.Type = AccountTypeOAuth
			case "shadow":
				id := int64(5)
				a.ParentAccountID = &id
			case "error":
				a.Status = StatusError
			case "expired":
				a.ExpiresAt = &past
				a.AutoPauseOnExpired = true
			case "cooldown":
				a.TempUnschedulableUntil = &until
			case "rate limited":
				a.RateLimitResetAt = &until
			case "overload":
				a.OverloadUntil = &until
			case "capacity pause":
				a.Extra = map[string]any{AccountRiskCapacityPauseExtraKey: "risk"}
			case "wrong group":
				a.GroupIDs = []int64{8}
			}
			require.NoError(t, s.reconcileAPIFallbacks(context.Background(), now))
			require.False(t, accounts.pool[0].Schedulable)
		})
	}
}
