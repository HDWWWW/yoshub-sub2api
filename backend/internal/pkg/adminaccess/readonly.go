package adminaccess

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

type readOnlyContextKey struct{}

func WithReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyContextKey{}, true)
}

func IsReadOnly(ctx context.Context) bool {
	value, _ := ctx.Value(readOnlyContextKey{}).(bool)
	return value
}

// An exact allowlist, independent of HTTP naming and of scoped administrator keys.
// New endpoints (including GET endpoints with side effects) are denied by default.
var readOnlyGETRoutes = wordSet(`
/dashboard/snapshot-v2 /dashboard/stats /dashboard/realtime /dashboard/trend
/dashboard/models /dashboard/groups /dashboard/api-keys-trend /dashboard/users-trend
/dashboard/users-ranking /dashboard/user-breakdown
/users /users/:id /users/:id/usage /users/:id/balance-history
/users/:id/rpm-status /users/:id/platform-quotas /users/:id/attributes /user-attributes
/groups /groups/all /groups/usage-summary /groups/capacity-summary /groups/live-capability
/groups/:id /groups/:id/stats /groups/:id/rate-multipliers
/groups/:id/model-allowlist-candidates /groups/:id/composite-routes
/accounts /accounts/:id /accounts/:id/stats /accounts/:id/usage /accounts/:id/today-stats
/accounts/:id/temp-unschedulable /accounts/:id/scheduled-test-plans
/accounts/upstream-billing-rates /accounts/upstream-billing-probe/settings
/accounts/antigravity/default-model-mapping /scheduled-test-plans/:id/results
/proxies /proxies/all /proxies/:id /proxies/:id/stats /proxies/:id/accounts
/announcements /announcements/:id /announcements/:id/read-status
/redeem-codes /redeem-codes/stats /redeem-codes/:id
/promo-codes /promo-codes/:id /promo-codes/:id/usages
/subscriptions /subscriptions/:id /subscriptions/:id/progress
/groups/:id/subscriptions /users/:id/subscriptions
/usage /usage/stats /usage/search-users /usage/search-api-keys /usage/cleanup-tasks
/channels /channels/:id /channels/model-pricing
/channel-monitors /channel-monitors/:id /channel-monitors/:id/history
/channel-monitor-templates /channel-monitor-templates/:id /channel-monitor-templates/:id/monitors
/channel-monitor-v2/config /channel-monitor-v2/dimensions /channel-monitor-v2/snapshot
/channel-monitor-v2/models /channel-monitor-v2/matrix /channel-monitor-v2/errors /channel-monitor-v2/users
/affiliates/invites /affiliates/rebates /affiliates/transfers /affiliates/users
/affiliates/users/lookup /affiliates/users/:user_id/overview
/payment/dashboard /payment/config /payment/orders /payment/orders/:id /payment/plans /payment/providers
/ops/concurrency /ops/user-concurrency /ops/account-availability /ops/realtime-traffic
/ops/alert-rules /ops/alert-events /ops/alert-events/:id /ops/advanced-settings
/ops/email-notification/config /ops/runtime/alert /ops/runtime/logging /ops/settings/metric-thresholds
/ops/ws/qps /ops/errors /ops/errors/:id /ops/request-errors /ops/request-errors/:id
/ops/request-errors/:id/upstream-errors /ops/upstream-errors /ops/upstream-errors/:id
/ops/requests /ops/system-logs /ops/system-logs/health
/ops/ingress-rejections /ops/ingress-rejections/health /ops/auth-cache-invalidation/health
/ops/dashboard/snapshot-v2 /ops/dashboard/overview /ops/dashboard/throughput-trend
/ops/dashboard/latency-histogram /ops/dashboard/error-trend /ops/dashboard/error-distribution
/ops/dashboard/openai-token-stats /audit-logs /audit-logs/:id
/settings /system/version /system/check-updates /system/rollback-versions
/error-passthrough-rules /error-passthrough-rules/:id /tls-fingerprint-profiles /tls-fingerprint-profiles/:id
/compliance /plugins /plugins/:id
/backups /backups/:id /backups/schedule /backups/s3-config /backups/image-storage
/risk-control/config /risk-control/status /risk-control/logs /risk-control/logs/:id/input /risk-control/rule-filter/catalog
/prompt-audit/config /prompt-audit/runtime /prompt-audit/events /prompt-audit/events/:id
`)

// These POSTs only read statistics. In particular, active probes, quota resets,
// token refresh, previews that contact upstreams and exports are not included.
var readOnlyPOSTRoutes = wordSet(`
/dashboard/users-usage /dashboard/api-keys-usage
/accounts/usage/batch /accounts/today-stats/batch /accounts/lifetime-stats/batch
/user-attributes/batch
`)

func wordSet(words string) map[string]bool {
	result := make(map[string]bool)
	for _, word := range strings.Fields(words) {
		result[word] = true
	}
	return result
}

func ReadOnlyRouteAllowed(method, path string) bool {
	const prefix = "/api/v1/admin"
	if !strings.HasPrefix(path, prefix+"/") {
		return false
	}
	path = strings.TrimPrefix(path, prefix)
	switch method {
	case "GET":
		return readOnlyGETRoutes[path]
	case "POST":
		return readOnlyPOSTRoutes[path]
	default:
		return false
	}
}

var readOnlySettingsFields = wordSet(`
site_name site_subtitle site_logo api_base_url contact_info doc_url frontend_url
compact_home_enabled table_default_page_size table_page_size_options
registration_enabled email_verify_enabled registration_email_suffix_whitelist
promo_code_enabled password_reset_enabled invitation_code_enabled
default_balance default_concurrency default_user_rpm_limit default_subscriptions default_platform_quotas
ops_monitoring_enabled ops_realtime_monitoring_enabled ops_query_mode_default
backend_mode_enabled totp_enabled passkey_enabled session_binding_enabled step_up_enabled audit_log_retention_days
turnstile_enabled tencent_captcha_enabled aliyun_captcha_enabled
linuxdo_connect_enabled dingtalk_connect_enabled wechat_connect_enabled oidc_connect_enabled github_oauth_enabled google_oauth_enabled
login_agreement_enabled login_agreement_mode login_agreement_updated_at login_agreement_documents
smtp_host smtp_port smtp_username smtp_from_email smtp_from_name smtp_use_tls
enable_model_fallback fallback_model_anthropic fallback_model_openai fallback_model_gemini fallback_model_antigravity
enable_fingerprint_unification enable_metadata_passthrough enable_identity_patch
min_claude_code_version max_claude_code_version web_search_emulation_enabled
channel_monitor_enabled channel_monitor_mode channel_monitor_default_interval_seconds
available_channels_enabled subscription_enabled purchase_subscription_enabled purchase_subscription_url
model_plaza_enabled model_plaza_require_auth plugin_management_enabled risk_control_enabled affiliate_enabled
balance_low_notify_enabled balance_low_notify_threshold subscription_expiry_notify_enabled account_quota_notify_enabled
payment_enabled payment_min_amount payment_max_amount payment_daily_limit
payment_order_timeout_minutes payment_max_pending_orders payment_enabled_types payment_balance_disabled
payment_balance_recharge_multiplier payment_subscription_usd_to_cny_rate payment_recharge_fee_rate payment_load_balance_strategy
`)

var readOnlyCredentialFields = wordSet(`
email plan_type account_type subscription_expires_at
subscription_active_until expires_at tier_id tier_name
`)

// Account metadata is extensible; explicitly retain only native display data.
// Otherwise a future refill/IP option could silently become readable.
var readOnlyExtraFields = wordSet(`
email plan_type privacy_mode subscription_expires_at subscription_active_until
model_rate_limits claude_usage codex_usage gemini_usage antigravity_usage
usage_5h usage_7d usage_updated_at upstream_billing_rate
upstream_billing_rate_checked_at
`)

var hiddenReadOnlyFields = wordSet(`
key api_keys access_token refresh_token id_token token password password_hash secret
client_secret signing_key private_key totp_secret_encrypted
credentials_status authorization cookie cookies request_headers response_headers
request_body response_body raw_request raw_response raw_payload
smart_ws_mode force_openai_fast force_openai_ultrafast filter_openai_service_tier
free_openai_fast reverse_account_priority
managed_ipv6 managed_ipv6_state privacy_ipv6 proxy_binding_limits binding_limit binding_limits
refill scheduling_metadata manual_priority lease_started_at lease_expires_at
custom_menu_items config_schema secret_access_key access_key_id
upstream_errors upstream_error_detail upstream_response_body error_body
webhook_url key_ipv6 api_key_prefix credential_masked validation_url
`)

// Redact after mapping, before serialization, for every permitted JSON response,
// including nested accounts/groups and batch/list variants. Copy through JSON so
// no cached service objects or full administrator responses are modified.
func RedactReadOnlyResponse(path string, data any) (any, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if path == "/api/v1/admin/settings" {
		value = retainReadOnlyFields(value, readOnlySettingsFields)
	}
	if path == "/api/v1/admin/payment/config" {
		value = retainReadOnlyFields(value, wordSet("enabled min_amount max_amount daily_limit order_timeout_minutes max_pending_orders enabled_payment_types balance_disabled balance_recharge_multiplier subscription_usd_to_cny_rate recharge_fee_rate load_balance_strategy product_name_prefix product_name_suffix help_image_url help_text"))
	}
	return redactReadOnlyValue(path, value), nil
}

func retainReadOnlyFields(value any, fields map[string]bool) any {
	object, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	for key := range object {
		if !fields[key] {
			delete(object, key)
		}
	}
	return object
}

var credentialText = regexp.MustCompile(`(?i)(?:sk-[a-z0-9_-]{16,}|bearer\s+[a-z0-9._~+/-]+=*|eyJ[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+)`)

func redactReadOnlyValue(path string, value any) any {
	switch object := value.(type) {
	case string:
		return credentialText.ReplaceAllString(logredact.RedactText(object, "api_key", "key", "authorization", "secret_access_key"), "[redacted]")
	case []any:
		for i := range object {
			object[i] = redactReadOnlyValue(path, object[i])
		}
	case map[string]any:
		for key, item := range object {
			lower := strings.ToLower(key)
			// Preserve name/id metadata used by native usage tables, never the key itself.
			if lower == "api_key" {
				if _, ok := item.(map[string]any); !ok {
					delete(object, key)
					continue
				}
				item = retainReadOnlyFields(item, wordSet("id name status group_id user_id"))
			}
			if lower == "config" && !strings.HasPrefix(path, "/api/v1/admin/channel-monitor-v2/") && path != "/api/v1/admin/backups/image-storage" {
				delete(object, key)
				continue
			}
			if hiddenReadOnlyFields[lower] || strings.Contains(lower, "smart_ws") ||
				strings.Contains(lower, "smartws") || strings.Contains(lower, "refill") ||
				strings.Contains(lower, "first_token_optimization") || strings.Contains(lower, "happy_first") ||
				strings.Contains(lower, "ovh_ipv6") || strings.Contains(lower, "auto_fast") ||
				strings.HasSuffix(lower, "_password") || strings.HasSuffix(lower, "_secret") ||
				strings.HasSuffix(lower, "_api_key") {
				delete(object, key)
				continue
			}
			if key == "credentials" {
				item = retainReadOnlyFields(item, readOnlyCredentialFields)
			}
			if key == "extra" {
				item = retainReadOnlyFields(item, readOnlyExtraFields)
			}
			object[key] = redactReadOnlyValue(path, item)
		}
	}
	return value
}
