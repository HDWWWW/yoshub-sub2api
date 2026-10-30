package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccountExpiryPriorityConfig(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled)
	t.Setenv("GATEWAY_SCHEDULING_ACCOUNT_EXPIRY_PRIORITY_ENABLED", "true")
	cfg, err = Load()
	require.NoError(t, err)
	require.True(t, cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled)
}
