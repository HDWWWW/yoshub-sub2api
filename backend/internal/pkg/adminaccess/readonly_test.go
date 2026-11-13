package adminaccess

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadOnlyRoutesDenyMutationsAndSecrets(t *testing.T) {
	for _, route := range []string{
		"/accounts", "/groups/:id", "/users/:id", "/settings",
		"/payment/orders/:id/refund", "/accounts/:id/schedulable", "/accounts/:id/force-fast",
	} {
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			require.False(t, ReadOnlyRouteAllowed(method, "/api/v1/admin"+route), method+" "+route)
		}
	}
	for _, route := range []string{
		"/accounts/refill", "/accounts/auto-fast", "/accounts/risk-detection", "/accounts/scheduling-preview",
		"/accounts/refill/jobs/:job_id/delivery", "/accounts/data", "/proxies/data",
		"/proxies/ovh-ipv6", "/proxies/ovh-ipv6/storage", "/ops/smart-ws",
		"/settings/happy-first-token", "/settings/group-first-token-optimization/:id",
		"/settings/admin-api-key", "/settings/admin-api-keys", "/users/:id/api-keys",
		"/groups/:id/api-keys", "/backups/:id/download-url", "/plugins/:id/config", "/accounts/:id/models",
		"/new-feature/unsafe-get",
	} {
		require.False(t, ReadOnlyRouteAllowed("GET", "/api/v1/admin"+route), route)
	}
	for _, route := range []string{"/accounts", "/accounts/:id", "/groups", "/groups/:id", "/users", "/ops/concurrency", "/payment/orders"} {
		require.True(t, ReadOnlyRouteAllowed("GET", "/api/v1/admin"+route), route)
	}
	require.True(t, ReadOnlyRouteAllowed("POST", "/api/v1/admin/accounts/today-stats/batch"))
	require.False(t, ReadOnlyRouteAllowed("GET", "/api/v1/administrator/accounts"))
	require.False(t, ReadOnlyRouteAllowed("GET", ""))
}

func TestReadOnlyResponsePreservesNativeConfigAndKeyNames(t *testing.T) {
	for _, path := range []string{"/api/v1/admin/channel-monitor-v2/snapshot", "/api/v1/admin/backups/image-storage"} {
		out, err := RedactReadOnlyResponse(path, map[string]any{
			"config":          map[string]any{"enabled": true, "secret_access_key": "storage-secret", "access_key_id": "credential-id"},
			"api_key":         map[string]any{"id": 3, "name": "Usage key name", "key": "gateway-secret"},
			"message":         "Authorization: Bearer sk-01234567890123456789",
			"upstream_errors": `[{"authorization":"gateway-secret"}]`,
		})
		require.NoError(t, err)
		raw, err := json.Marshal(out)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"enabled":true`)
		require.Contains(t, string(raw), "Usage key name")
		for _, secret := range []string{"storage-secret", "credential-id", "gateway-secret", "sk-01234567890123456789"} {
			require.NotContains(t, string(raw), secret)
		}
	}
}

func TestReadOnlyResponseRedactsNestedFieldsWithoutMutatingSource(t *testing.T) {
	original := map[string]any{
		"id": int64(9007199254740993), "name": "native account", "priority": 5,
		"credentials": map[string]any{"email": "account@example.com", "plan_type": "plus", "access_token": "secret", "base_url": "private"},
		"extra":       map[string]any{"privacy_mode": "training_disabled", "refill_supplier": "secret", "openai_force_fast": true, "future_private_field": true},
		"groups":      []any{map[string]any{"id": 2, "name": "group", "smart_ws_mode": "enabled", "force_openai_fast": true}},
		"proxy":       map[string]any{"name": "proxy", "password": "secret", "managed_ipv6": "secret"},
		"api_key":     "secret", "key": "secret", "smtp_password": "secret",
	}
	before, err := json.Marshal(original)
	require.NoError(t, err)
	out, err := RedactReadOnlyResponse("/api/v1/admin/accounts/:id", original)
	require.NoError(t, err)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "9007199254740993")
	require.Contains(t, string(encoded), "account@example.com")
	require.Contains(t, string(encoded), "native account")
	for _, hidden := range []string{"secret", "smart_ws", "force_openai_fast", "openai_force_fast", "future_private_field", "managed_ipv6"} {
		require.NotContains(t, string(encoded), hidden)
	}
	after, err := json.Marshal(original)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestReadOnlySettingsExposeOnlyDisplayFields(t *testing.T) {
	out, err := RedactReadOnlyResponse("/api/v1/admin/settings", map[string]any{
		"site_name": "Sub2", "ops_monitoring_enabled": true, "default_concurrency": 5,
		"admin_api_key": "secret", "unknown_future_credential": "secret", "custom_menu_items": []string{"secret"},
	})
	require.NoError(t, err)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "secret")
	require.Contains(t, string(raw), "Sub2")
	require.False(t, IsReadOnly(context.Background()))
	require.True(t, IsReadOnly(WithReadOnly(context.Background())))
}
