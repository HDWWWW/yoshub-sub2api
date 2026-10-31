//go:build unit

package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"testing"
)

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
