package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewAccountQuotaPauseDefaults(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	ApplyNewAccountQuotaPauseDefaults(account)
	require.Equal(t, 0.99, account.Extra["auto_pause_5h_threshold"])
	require.Equal(t, 0.99, account.Extra["auto_pause_7d_threshold"])

	source := map[string]any{"auto_pause_5h_threshold": 0.8, "auto_pause_7d_disabled": true, "unrelated": "retained"}
	account.Extra = source
	ApplyNewAccountQuotaPauseDefaults(account)
	require.Equal(t, 0.8, account.Extra["auto_pause_5h_threshold"])
	require.Equal(t, 0.99, account.Extra["auto_pause_7d_threshold"])
	require.Equal(t, true, account.Extra["auto_pause_7d_disabled"])
	require.Equal(t, "retained", account.Extra["unrelated"])
	require.NotContains(t, source, "auto_pause_7d_threshold", "do not mutate imported source maps")

	account = &Account{Platform: PlatformAnthropic}
	ApplyNewAccountQuotaPauseDefaults(account)
	require.Nil(t, account.Extra)
}

func TestNewAccountQuotaPauseAt99AndReset(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			ApplyNewAccountQuotaPauseDefaults(account)
			account.Extra["codex_usage_updated_at"] = time.Now().Format(time.RFC3339)
			account.Extra["codex_"+window+"_used_percent"] = 98.9
			account.Extra["codex_"+window+"_reset_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
			paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
			require.False(t, paused)
			account.Extra["codex_"+window+"_used_percent"] = 99.0
			paused, decision := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
			require.True(t, paused)
			require.Equal(t, window, decision.window)
			account.Extra["codex_"+window+"_reset_at"] = time.Now().Add(-time.Minute).Format(time.RFC3339)
			paused, _ = shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
			require.False(t, paused, "window reset automatically makes the account eligible again")
		})
	}
}
