//go:build unit

package admin

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRefillConfigPlanQuotaReplacementAndLegacyClient(t *testing.T) {
	current := service.DefaultAccountRefillConfig()
	current.PlanQuotaThresholds = map[string]service.AccountRefillQuotaThreshold{
		"pro5x": {MinRemaining5h: 15}, "pro20x": {MinRemaining5h: 10},
	}
	legacy, err := decodeAccountRefillConfig(current, json.RawMessage(`{"min_remaining_5h":7}`))
	require.NoError(t, err)
	require.Len(t, legacy.PlanQuotaThresholds, 2)
	require.Equal(t, 7.0, legacy.MinRemaining5h)
	updated, err := decodeAccountRefillConfig(current, json.RawMessage(`{"plan_quota_thresholds":{"pro20x":{"min_remaining_5h":9,"min_remaining_7d":8}}}`))
	require.NoError(t, err)
	require.Len(t, updated.PlanQuotaThresholds, 1)
	require.Equal(t, 9.0, updated.PlanQuotaThresholds["pro20x"].MinRemaining5h)
	require.Equal(t, 10.0, current.PlanQuotaThresholds["pro20x"].MinRemaining5h)
	cleared, err := decodeAccountRefillConfig(current, json.RawMessage(`{"plan_quota_thresholds":{}}`))
	require.NoError(t, err)
	require.Empty(t, cleared.PlanQuotaThresholds)
}

func TestExternalAccountImportExpiryScope(t *testing.T) {
	for _, auth := range []string{"jwt", "admin_api_key"} {
		t.Run(auth, func(t *testing.T) {
			expiry := int64(2000000000)
			req := DataImportRequest{Data: DataPayload{Accounts: []DataAccount{
				{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, ExpiresAt: &expiry, Credentials: map[string]any{"expires_at": "token-expiry"}},
				{Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth, ExpiresAt: &expiry},
				{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, ExpiresAt: &expiry},
			}}}
			applyExternalImportExpiry(&req, auth)
			expected := expiry
			if auth == "admin_api_key" {
				expected += 3600
				require.Equal(t, 3600, req.Data.Accounts[0].Extra[service.AccountImportExpiryGraceExtraKey])
			} else {
				require.Nil(t, req.Data.Accounts[0].Extra)
			}
			require.Equal(t, expected, *req.Data.Accounts[0].ExpiresAt)
			require.Equal(t, "token-expiry", req.Data.Accounts[0].Credentials["expires_at"])
			require.Equal(t, expiry, *req.Data.Accounts[1].ExpiresAt)
			require.Equal(t, expiry, *req.Data.Accounts[2].ExpiresAt)
			applyExternalImportExpiry(&req, auth)
			require.Equal(t, expected, *req.Data.Accounts[0].ExpiresAt)
		})
	}
}
