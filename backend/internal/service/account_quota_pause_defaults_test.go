package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type importThresholdSettings struct {
	SettingRepository
	raw string
	err error
}

func (s importThresholdSettings) GetValue(context.Context, string) (string, error) {
	return s.raw, s.err
}

func TestNewAccountQuotaPauseDefaultsReadFailure(t *testing.T) {
	errRead := errors.New("settings unavailable")
	account := &Account{Platform: PlatformOpenAI}
	require.ErrorIs(t, ApplyNewAccountQuotaPauseDefaults(t.Context(), account, importThresholdSettings{err: errRead}), errRead)
	require.Nil(t, account.Credentials)
	for _, raw := range []string{"0", "101", "invalid"} {
		require.Error(t, ApplyNewAccountQuotaPauseDefaults(t.Context(), account, importThresholdSettings{raw: raw}))
	}
	account.Credentials = map[string]any{accountSchedulingThresholdCredentialKey: 100}
	require.NoError(t, ApplyNewAccountQuotaPauseDefaults(t.Context(), account, importThresholdSettings{err: errRead}))
}

func TestNewAccountQuotaPauseDefaults(t *testing.T) {
	for _, explicit := range []any{nil, 80, 100, "100"} {
		t.Run(stringValue(explicit), func(t *testing.T) {
			credentials := map[string]any{"unrelated": "retained"}
			if explicit != nil {
				credentials[accountSchedulingThresholdCredentialKey] = explicit
			}
			extra := map[string]any{"auto_pause_5h_threshold": 0.8, "auto_pause_7d_disabled": true}
			account := &Account{Platform: PlatformOpenAI, Credentials: credentials, Extra: extra}
			require.NoError(t, ApplyNewAccountQuotaPauseDefaults(t.Context(), account, importThresholdSettings{err: ErrSettingNotFound}))
			if explicit == nil {
				require.Equal(t, 99, account.Credentials[accountSchedulingThresholdCredentialKey])
				require.NotContains(t, credentials, accountSchedulingThresholdCredentialKey)
			} else {
				require.Equal(t, explicit, account.Credentials[accountSchedulingThresholdCredentialKey])
			}
			require.Equal(t, "retained", account.Credentials["unrelated"])
			require.Equal(t, extra, account.Extra)
			require.NotContains(t, account.Extra, "auto_pause_7d_threshold")
		})
	}
	account := &Account{Platform: PlatformAnthropic}
	require.NoError(t, ApplyNewAccountQuotaPauseDefaults(t.Context(), account, importThresholdSettings{err: ErrSettingNotFound}))
	require.Nil(t, account.Credentials)
	require.Nil(t, account.Extra)
}

func TestNewAccountQuotaPauseAt99(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			now := time.Now()
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
			require.NoError(t, ApplyNewAccountQuotaPauseDefaults(t.Context(), account, importThresholdSettings{err: ErrSettingNotFound}))
			account.Extra = map[string]any{
				"codex_usage_updated_at":            now.Format(time.RFC3339),
				"codex_" + window + "_used_percent": 98.9,
				"codex_" + window + "_reset_at":     now.Add(time.Hour).Format(time.RFC3339),
			}
			require.False(t, ShouldStopAccountAtSchedulingThreshold(account, now))
			account.Extra["codex_"+window+"_used_percent"] = 99.0
			require.True(t, ShouldStopAccountAtSchedulingThreshold(account, now))
			account.Credentials[accountSchedulingThresholdCredentialKey] = 100
			require.False(t, ShouldStopAccountAtSchedulingThreshold(account, now))
		})
	}
}
