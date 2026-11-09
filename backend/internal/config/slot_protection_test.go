package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPreserveProcessSlotsDefaultAndOverride(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{{"", true}, {"true", true}, {"false", false}} {
		t.Run(tc.env, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("GATEWAY_SCHEDULING_PRESERVE_PROCESS_SLOTS", tc.env)
			c, err := Load()
			require.NoError(t, err)
			require.Equal(t, tc.want, c.Gateway.Scheduling.PreserveProcessSlots)
		})
	}
}
