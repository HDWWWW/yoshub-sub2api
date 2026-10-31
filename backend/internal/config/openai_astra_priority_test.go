package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAIAstraPriorityConfig(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.Gateway.Scheduling.OpenAIAstraPriorityEnabled)
	t.Setenv("GATEWAY_SCHEDULING_OPENAI_ASTRA_PRIORITY_ENABLED", "false")
	cfg, err = Load()
	require.NoError(t, err)
	require.False(t, cfg.Gateway.Scheduling.OpenAIAstraPriorityEnabled)
}
