//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountRefillNamespaceSettingUsesOrderSnapshot(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name    string
		setting *bool
	}{
		{name: "legacy default"},
		{name: "enabled", setting: &enabled},
		{name: "disabled", setting: &disabled},
	} {
		for _, mode := range []string{"automatic", "manual"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := context.Background()
				s, store, provider, accounts := refillHarness(t)
				config := refillTestConfig()
				config.Delivery.OpenAIResponsesFlattenNamespaces = tc.setting
				if mode == "manual" {
					config.Enabled, config.MonitorEnabled = false, false
					s.provider = &manualRefillProvider{provider, 500}
				}
				require.NoError(t, s.Configure(ctx, config, "", 1))
				if mode == "manual" {
					request := manualRequest()
					request.Revision = 2
					require.NoError(t, s.Manual(ctx, request))
				}
				require.NoError(t, s.run(ctx, false))
				require.Len(t, provider.submitted, 1)
				require.Equal(t, tc.setting, provider.submitted[0].DeliveryConfig.OpenAIResponsesFlattenNamespaces)

				// Changing the rule while delivery is pending must not rewrite the order.
				view, err := s.View(ctx)
				require.NoError(t, err)
				expected := tc.setting != nil && *tc.setting
				changed := !expected
				config = view.Config
				config.Enabled = false
				config.Delivery.OpenAIResponsesFlattenNamespaces = &changed
				require.NoError(t, s.Configure(ctx, config, "", view.Revision))
				s = &AccountRefillService{store: store, provider: s.provider, encryptor: s.encryptor, accounts: s.accounts, admin: s.admin, groups: s.groups, loads: s.loads}
				provider.status = "succeeded"
				for range 2 {
					refillDue(t, store)
					require.NoError(t, s.run(ctx, false))
				}
				require.Equal(t, 1, accounts.creates)
				require.Len(t, accounts.accounts, 1)
				require.Equal(t, expected, accounts.accounts[0].IsOpenAIResponsesFlattenNamespacesEnabled())
				if !expected {
					require.NotContains(t, accounts.accounts[0].Extra, "openai_responses_flatten_namespaces")
				}
				require.Len(t, provider.submitted, 1)
			})
		}
	}
}

func TestAccountRefillNamespaceSettingPersistsAndPreservesLegacySaves(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := refillHarness(t)
	view, err := s.View(ctx)
	require.NoError(t, err)
	require.Nil(t, view.Config.Delivery.OpenAIResponsesFlattenNamespaces)
	for _, enabled := range []bool{true, false} {
		config := view.Config
		config.Delivery.OpenAIResponsesFlattenNamespaces = &enabled
		require.NoError(t, s.Configure(ctx, config, "", view.Revision))
		view, err = s.View(ctx)
		require.NoError(t, err)
		require.Equal(t, &enabled, view.Config.Delivery.OpenAIResponsesFlattenNamespaces)

		config = view.Config
		config.Delivery.OpenAIResponsesFlattenNamespaces = nil
		config.CooldownSeconds++
		require.NoError(t, s.Configure(ctx, config, "", view.Revision))
		view, err = s.View(ctx)
		require.NoError(t, err)
		require.Equal(t, &enabled, view.Config.Delivery.OpenAIResponsesFlattenNamespaces)
	}
}

func TestAccountRefillNamespaceSettingIsScopedToGroup(t *testing.T) {
	ctx := context.Background()
	s, base, _, _ := refillHarness(t)
	s.store = &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	config := refillTestConfig()
	enabled := true
	config.Delivery.OpenAIResponsesFlattenNamespaces = &enabled
	require.NoError(t, s.Configure(ctx, config, "", 1))
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	view, err := child.View(ctx)
	require.NoError(t, err)
	require.Nil(t, view.Config.Delivery.OpenAIResponsesFlattenNamespaces)
	disabled := false
	view.Config.Delivery.OpenAIResponsesFlattenNamespaces = &disabled
	require.NoError(t, child.Configure(ctx, view.Config, "", view.Revision))
	view, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, &enabled, view.Config.Delivery.OpenAIResponsesFlattenNamespaces)
}
