//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetPoolModeRetryCount(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected int
	}{
		{
			name: "default_when_not_pool_mode",
			account: &Account{
				Type:        AccountTypeAPIKey,
				Platform:    PlatformOpenAI,
				Credentials: map[string]any{},
			},
			expected: defaultPoolModeRetryCount,
		},
		{
			name: "default_when_missing_retry_count",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode": true,
				},
			},
			expected: defaultPoolModeRetryCount,
		},
		{
			name: "supports_float64_from_json_credentials",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": float64(5),
				},
			},
			expected: 5,
		},
		{
			name: "supports_json_number",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": json.Number("4"),
				},
			},
			expected: 4,
		},
		{
			name: "supports_string_value",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": "2",
				},
			},
			expected: 2,
		},
		{
			name: "negative_value_is_clamped_to_zero",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": -1,
				},
			},
			expected: 0,
		},
		{
			name: "oversized_value_is_clamped_to_max",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": 99,
				},
			},
			expected: maxPoolModeRetryCount,
		},
		{
			name: "invalid_value_falls_back_to_default",
			account: &Account{
				Type:     AccountTypeAPIKey,
				Platform: PlatformOpenAI,
				Credentials: map[string]any{
					"pool_mode":             true,
					"pool_mode_retry_count": "oops",
				},
			},
			expected: defaultPoolModeRetryCount,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, tt.account.GetPoolModeRetryCount())
		})
	}
}

func TestSameAccountRetryCountForAllAccountTypes(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey, AccountTypeBedrock, "setup-token", "service_account", "upstream"} {
		t.Run(accountType, func(t *testing.T) {
			account := &Account{Type: accountType, Platform: PlatformOpenAI}
			require.Equal(t, 1, account.GetPoolModeRetryCount())
			for _, count := range []int{0, 1, 4, 10} {
				account.Extra = map[string]any{"same_account_retry_count": float64(count)}
				require.Equal(t, count, account.GetPoolModeRetryCount())
			}
			account.Extra["same_account_retry_count"] = "oops"
			require.Equal(t, 1, account.GetPoolModeRetryCount())
			account.Extra["same_account_retry_count"] = float64(99)
			require.Equal(t, 10, account.GetPoolModeRetryCount())
		})
	}
	account := &Account{Type: AccountTypeAPIKey, Credentials: map[string]any{"pool_mode": true, "pool_mode_retry_count": 3}}
	require.Equal(t, 3, account.GetPoolModeRetryCount(), "preserve explicit legacy configuration")
	account.Extra = map[string]any{"same_account_retry_count": 0}
	require.Zero(t, account.GetPoolModeRetryCount(), "new account setting takes precedence even when zero")
}
