//go:build unit

package service

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

type retrySettingsRepo struct{ settingRepoStub }

func (r *retrySettingsRepo) Set(_ context.Context, k, v string) error { r.values[k] = v; return nil }
func TestDefaultAccountRetryPersistenceAndImport(t *testing.T) {
	ctx := context.Background()
	repo := &retrySettingsRepo{settingRepoStub{values: map[string]string{}}}
	svc := &SettingService{settingRepo: repo}
	count, err := svc.GetDefaultAccountRetryCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, count)
	for _, n := range []int{0, 3, 10} {
		require.NoError(t, svc.SetDefaultAccountRetryCount(ctx, n))
		fresh := &SettingService{settingRepo: repo}
		input := &CreateAccountInput{Type: AccountTypeOAuth, Extra: map[string]any{"keep": true}}
		require.NoError(t, fresh.applyDefaultAccountRetryCount(ctx, input))
		require.Equal(t, n, input.Extra["same_account_retry_count"])
		require.Equal(t, true, input.Extra["keep"])
	}
	for _, n := range []int{-1, 11} {
		require.Error(t, svc.SetDefaultAccountRetryCount(ctx, n))
	}
	explicit := &CreateAccountInput{Extra: map[string]any{"same_account_retry_count": 0}}
	require.NoError(t, svc.applyDefaultAccountRetryCount(ctx, explicit))
	require.Equal(t, 0, explicit.Extra["same_account_retry_count"])
	legacy := &CreateAccountInput{Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": 1}}
	require.NoError(t, svc.applyDefaultAccountRetryCount(ctx, legacy))
	require.Nil(t, legacy.Extra)
	repo.err = errors.New("unavailable")
	require.Error(t, svc.applyDefaultAccountRetryCount(ctx, &CreateAccountInput{}))
}
func TestRefillSameAccountRetriesSnapshot(t *testing.T) {
	for _, n := range []int{-1, 0, 3, 10} {
		s, store, provider, accounts := refillHarness(t)
		s.provider = &manualRefillProvider{provider, 500}
		store.mutate(t, func(v *accountRefillState) {
			v.Config.Enabled = false
			if n >= 0 {
				v.Config.Delivery.SameAccountRetryCount = &n
			}
		})
		require.NoError(t, s.Manual(context.Background(), manualRequest()))
		store.mutate(t, func(v *accountRefillState) { changed := 7; v.Config.Delivery.SameAccountRetryCount = &changed })
		require.NoError(t, s.run(context.Background(), false))
		provider.status = "succeeded"
		for i := 0; i < 2; i++ {
			refillDue(t, store)
			require.NoError(t, s.run(context.Background(), false))
		}
		require.Equal(t, 1, accounts.creates)
		expected := n
		if n < 0 {
			expected = 3
		}
		require.Equal(t, expected, accounts.accounts[0].GetPoolModeRetryCount())
	}
	c := refillTestConfig()
	for _, n := range []int{-1, 11} {
		c.Delivery.SameAccountRetryCount = &n
		require.Error(t, c.Validate())
	}
}
