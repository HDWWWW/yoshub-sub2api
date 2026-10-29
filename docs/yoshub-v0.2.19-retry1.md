# Yoshub v0.2.19: account retry controls

Based on the official `v0.2.19` release, following production `v0.2.18`.

- Account creation and editing always expose **Same-Account Retries**, including OAuth accounts. The field sits above concurrency/priority settings. The range is 0–10; the default is one additional retry, excluding the initial request.
- The account setting is stored as `extra.same_account_retry_count`, included in scheduler metadata, and used by failover handlers. An explicit value of zero disables these retries. Existing pool-mode `credentials.pool_mode_retry_count` values remain the fallback until the account is edited.
- OpenAI OAuth transient 429 deadlines are an additional time limit and no longer bypass the account retry count.
- Gemini retries and Antigravity smart/capacity retries also read account settings. Gemini's global default is one; an explicitly configured global Gemini limit remains the fallback for legacy accounts without an account override.
- Protocol compatibility repair requests and cross-account switching are separate mechanisms. This setting does not impose a total upstream-request cap across all recovery layers.
- GitHub workflows are manual-only. Build and publication run locally, and deployment pins the private GHCR digest.

Official release highlights: scoped administrator API keys; opt-in billing by actual returned image count; GPT-6.1 Terra; Gemini model-list/cache/retry fixes; shorter cooldowns for frequency-related Coding Plan 429s; Grok model diagnostics; DeepSeek billing/role fixes; rejection of oversized multipart uploads; pagination overflow fixes; model refresh request deduplication; and updated `golang.org/x/net`.

Official startup migrations 247 and 248 add `scoped_admin_api_keys` and `groups.image_count_billing`. Existing accounts and billing settings are not bulk modified. Back up PostgreSQL and deployment configuration before replacement; recreate only the Sub2API service with `--no-deps`.
