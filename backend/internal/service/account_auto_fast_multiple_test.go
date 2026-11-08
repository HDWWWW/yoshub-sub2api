//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func addAutoFastSecondAccount(store *autoFastTestStore, now time.Time) *Account {
	deadline := now.Add(2 * time.Hour)
	a := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		ExpiresAt: &deadline, GroupIDs: []int64{7}, Extra: map[string]any{}}
	store.accounts.pool[a.ID] = a
	return a
}

func TestAccountAutoFastMultipleAccountsTransitions(t *testing.T) {
	s, store, first, now := autoFastHarness()
	store.state.Rules[7].Config.RequireMultipleAccounts = true
	ctx := context.Background()
	require.NoError(t, s.tick(ctx, now))
	require.False(t, first.IsOpenAIForceFastEnabled())
	require.Equal(t, 1, store.state.Rules[7].EligibleAccounts)
	require.Equal(t, "insufficient_accounts", store.state.Rules[7].Accounts[0].Mode)

	second := addAutoFastSecondAccount(store, now) // Future stage and no usage snapshot still counts.
	require.NoError(t, s.tick(ctx, now.Add(10*time.Second)))
	require.Equal(t, 2, store.state.Rules[7].EligibleAccounts)
	require.True(t, first.IsOpenAIForceFastEnabled())
	require.False(t, second.IsOpenAIForceFastEnabled())

	second.Schedulable = false
	require.NoError(t, s.tick(ctx, now.Add(20*time.Second)))
	require.False(t, first.IsOpenAIForceFastEnabled())
	require.Equal(t, 1, store.state.Rules[7].EligibleAccounts)
	require.False(t, store.state.Managed[first.ID].Applied)

	second.Schedulable = true
	require.NoError(t, s.tick(ctx, now.Add(30*time.Second)))
	require.True(t, first.IsOpenAIForceFastEnabled())
	first.Extra["codex_7d_used_percent"] = 70.
	require.NoError(t, s.tick(ctx, now.Add(40*time.Second)))
	require.False(t, first.IsOpenAIForceFastEnabled(), "two accounts allow the normal rules, not unconditional Fast")

	require.True(t, first.Schedulable)
	require.Equal(t, "keep", first.Extra["unrelated"])
	require.Equal(t, map[string]any{"unchanged": "credential"}, first.Credentials)
}

func TestAccountAutoFastMultipleAccountsOverridesFinalUnknownAndOriginal(t *testing.T) {
	for _, mode := range []string{"final", "quota_unknown", "waiting", "no_deadline"} {
		t.Run(mode, func(t *testing.T) {
			s, store, first, now := autoFastHarness()
			store.state.Rules[7].Config.RequireMultipleAccounts = true
			first.Extra[OpenAIForceFastExtraKey] = true
			switch mode {
			case "final":
				at := now.Add(3 * time.Minute)
				first.ExpiresAt = &at
			case "quota_unknown":
				delete(first.Extra, "codex_usage_updated_at")
			case "waiting":
				at := now.Add(2 * time.Hour)
				first.ExpiresAt = &at
			case "no_deadline":
				first.ExpiresAt = nil
			}
			require.NoError(t, s.tick(context.Background(), now))
			require.False(t, first.IsOpenAIForceFastEnabled())
			require.True(t, store.state.Managed[first.ID].Original)
			require.False(t, store.state.Managed[first.ID].Applied)
			require.Equal(t, "insufficient_accounts", store.state.Rules[7].Accounts[0].Mode)
			// Subsequent ticks must not restore Original=true while only one remains.
			require.NoError(t, s.tick(context.Background(), now.Add(10*time.Second)))
			require.False(t, first.IsOpenAIForceFastEnabled())
			c := store.state.Rules[7].Config
			c.Enabled = false
			require.NoError(t, s.Configure(context.Background(), 7, 1, c))
			require.True(t, first.IsOpenAIForceFastEnabled(), "disabling the whole rule restores the original state")
		})
	}
}

func TestAccountAutoFastMultipleAccountsEligibility(t *testing.T) {
	for _, kind := range []string{"eligible", "no deadline", "stopped", "disabled", "expired", "reclaimed", "rate limited", "overloaded", "temporary", "quota threshold", "other group", "other platform", "cooldown elapsed"} {
		t.Run(kind, func(t *testing.T) {
			s, store, first, now := autoFastHarness()
			store.state.Rules[7].Config.RequireMultipleAccounts = true
			second := addAutoFastSecondAccount(store, now)
			future, past := now.Add(time.Minute), now.Add(-time.Minute)
			switch kind {
			case "no deadline":
				second.ExpiresAt = nil
			case "stopped":
				second.Schedulable = false
			case "disabled":
				second.Status = "disabled"
			case "expired":
				second.ExpiresAt = &now
			case "reclaimed":
				second.Extra[AccountSourceExpiryExtraKey] = now.Unix()
			case "rate limited":
				second.RateLimitResetAt = &future
			case "overloaded":
				second.OverloadUntil = &future
			case "temporary":
				second.TempUnschedulableUntil = &future
			case "quota threshold":
				second.Credentials = map[string]any{accountSchedulingThresholdCredentialKey: 80}
				second.Extra["codex_7d_used_percent"] = 90.
				second.Extra["codex_7d_reset_at"] = future.Format(time.RFC3339)
			case "other group":
				second.GroupIDs = []int64{8}
			case "other platform":
				second.Platform = PlatformAnthropic
			case "cooldown elapsed":
				second.RateLimitResetAt = &past
			}
			require.NoError(t, s.tick(context.Background(), now))
			want := kind == "eligible" || kind == "no deadline" || kind == "cooldown elapsed"
			require.Equal(t, want, first.IsOpenAIForceFastEnabled())
			count := 1
			if want {
				count = 2
			}
			require.Equal(t, count, store.state.Rules[7].EligibleAccounts)
		})
	}
}

func TestAccountAutoFastMultipleAccountsToggleAndCAS(t *testing.T) {
	s, store, first, now := autoFastHarness()
	ctx := context.Background()
	require.NoError(t, s.tick(ctx, now))
	require.True(t, first.IsOpenAIForceFastEnabled(), "legacy default still allows one account")
	c := store.state.Rules[7].Config
	c.RequireMultipleAccounts = true
	require.NoError(t, s.Configure(ctx, 7, 1, c))
	store.cas = false
	require.NoError(t, s.tick(ctx, now.Add(10*time.Second)))
	require.True(t, first.IsOpenAIForceFastEnabled(), "a failed write must not claim Fast was disabled")
	require.True(t, store.state.Rules[7].Accounts[0].Fast)
	store.cas = true
	require.NoError(t, s.tick(ctx, now.Add(20*time.Second)))
	require.False(t, first.IsOpenAIForceFastEnabled())
	c.RequireMultipleAccounts = false
	require.NoError(t, s.Configure(ctx, 7, 2, c))
	require.NoError(t, s.tick(ctx, now.Add(30*time.Second)))
	require.True(t, first.IsOpenAIForceFastEnabled())
}
