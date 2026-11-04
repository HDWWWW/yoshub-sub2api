//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountRefillFingerprintModesPersistAndDeliver(t *testing.T) {
	for _, mode := range []string{"off", "device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			s, store, provider, accounts := refillHarness(t)
			config := refillTestConfig()
			config.Delivery.FingerprintMode = mode
			require.NoError(t, s.Configure(ctx, config, "", 1))
			view, err := s.View(ctx)
			require.NoError(t, err)
			require.Equal(t, mode, view.Config.Delivery.FingerprintMode)

			require.NoError(t, s.run(ctx, false))
			require.Len(t, provider.submitted, 1)
			require.Equal(t, mode, provider.submitted[0].DeliveryConfig.FingerprintMode)
			provider.status = "succeeded"
			for range 2 {
				refillDue(t, store)
				require.NoError(t, s.run(ctx, false))
			}
			require.Equal(t, 1, accounts.creates)
			require.Len(t, accounts.accounts, 1)
			require.Equal(t, mode, accounts.accounts[0].Extra[codexFingerprintModeExtraKey])
		})
	}
}

func TestAccountRefillFingerprintModesRejectUnknownValues(t *testing.T) {
	for _, mode := range []string{"", "unknown", "DEVICE", "device_session"} {
		t.Run(mode, func(t *testing.T) {
			config := refillTestConfig()
			config.Delivery.FingerprintMode = mode
			require.Error(t, config.Validate())
		})
	}
}
