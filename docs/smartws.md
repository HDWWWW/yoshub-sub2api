# SmartWS

SmartWS changes the upstream transport for eligible Responses requests. Clients keep the existing HTTP JSON/SSE interface. Native WebSocket ingress remains a separate path. This checkout runs one Go application with an embedded Vue frontend; scheduling, forwarding, and usage recording are internal services, not separate Core, Relay, or Worker deployments.

## Routing and controls

`internal/server/routes/gateway.go` registers HTTP POST and WebSocket GET `/v1/responses`. Both retain the existing authentication, group permissions, concurrency controls, and `OpenAIGatewayHandler` scheduling. HTTP forwarding selects its account first, builds its normal upstream request, then `doOpenAIUpstream` optionally calls `doOpenAIHTTPOverWS`. Existing response handlers still format SSE/JSON, and `RecordUsage` retains billing ownership. Converted Chat Completions uses the same adapter when its selected upstream is Responses.

The admin group field `smart_ws_mode` accepts `enabled`, `disabled`, and `inherit`. `enabled` opts HTTP requests into SmartWS; both other values retain HTTP upstream. Set it through the group editor or `PUT /api/v1/admin/groups/{id}`. Changes invalidate the existing API-key authorization cache. Migration 254 is additive and defaults existing groups to `inherit`; rollback binaries ignore the new column.

Global WS enablement, credential-type enablement, account/global force-HTTP, and plugin routing retain precedence. Compact, Messages, Excel OAuth, and plugin-dispatched requests are excluded. A custom API-key endpoint also needs the account's existing explicit WS opt-in; OpenAI compatibility alone is insufficient. Group SmartWS controls HTTP adaptation, while native WS continues to use the existing account ingress settings.

## Protocol and dispatch

The [OpenAI WebSocket mode guide](https://developers.openai.com/api/docs/guides/websocket-mode) specifies `wss://api.openai.com/v1/responses` and `response.create`. API-key payloads omit HTTP-only `stream`/`background`. OAuth uses this repository's existing Codex endpoint `wss://chatgpt.com/backend-api/codex/responses` and Codex payload conventions; actual OAuth support must be validated against the intended account and deployment before enabling traffic. The adapter retains finalized upstream authentication/session/capability headers and adds the existing Responses WS beta only when none is already selected.

The complete finalized input, instructions, model, effort, tier, tools, tool results, encrypted reasoning, and continuation ID pass into the adapter. Existing account/group policy still applies before transport selection. SmartWS does not reduce context or reasoning effort. OpenAI native forwarding no longer removes a client continuation ID or synthesizes a replacement full-context request after a connection failure. It sends no extra probe generation.

Dispatch states are `NotSent → MayBeSent → Received → Terminal`. `MayBeSent` is set **before** attempting a write. Only a failure before dispatch may use HTTP fallback. Oversized/background requests, unsupported WS handshakes, and pre-dispatch network/capacity failures preserve the complete HTTP body. Handshake authentication, rate-limit, and other explicit upstream errors retain their status/body. No permanent negative capability cache is used.

After a write may have occurred, neither HTTP fallback, another account, nor same-account replay is permitted. A transport failure reports an unknown dispatch outcome. Explicit upstream errors remain errors; observed failed usage is retained for the existing accounting path. A bounded failed-event tail can collect authoritative usage after a bare error. A missing terminal usage event is never fabricated as billable success.

HTTP adaptation exposes an uncertain transport outcome as `smart_ws_dispatch_uncertain`: HTTP 502 before output, or a protocol error event after SSE output has started. The message states that the request may have executed and was not replayed. No successful completion or `[DONE]` is synthesized for this failure. Explicit upstream terminal errors retain their original payload instead.

Handshake provenance remains attached to errors through the HTTP formatter. An unstructured or request-scoped 403 is an endpoint rejection, not evidence that the account credential is invalid; it does not increment account 403 strikes, cool down shared groups, or disable the account. A 401 or structured credential/workspace revocation retains the existing authentication policy. Explicit 429/529 signals retain the existing rate/overload policy. The rejected request receives its status/body and is never replayed.

Native `ctx_pool` and `passthrough` ingress also deliver explicit handshake errors before closing, including the upstream error object, HTTP status, and `Retry-After` hint. An endpoint rejection does not lower the shared account's scheduling score. A real 401 still applies the existing credential protection; clearing an error alone does not re-enable a disabled account.

For native clients, a turn that loses its upstream connection before a terminal event receives `smart_ws_dispatch_uncertain`, including when earlier turns on the same connection completed. Dispatch and delivered-error state reset between turns. The large-request HTTP bridge uses the same no-replay rule: it retains continuation IDs and encrypted reasoning, returns validation errors without deleting fields and resending, and records observed failed usage. A bare error can collect at most eight matching tail frames within 200 milliseconds for authoritative failed usage; a later completion cannot overwrite that error.

An endpoint 403 or unsupported-WS status pauses HTTP-to-WS attempts for the same account, credential, destination, proxy, and capability/identity headers for 30 seconds by default. Later independent requests may choose HTTP before dispatch; cache hits do not extend the pause. Authentication changes do not inherit the old pause. Session IDs are excluded from this rejection cache only; connection reuse isolation remains unchanged. This bounded process-local cache does not affect native WS. A request carrying `previous_response_id` is rejected with `409 smart_ws_continuation_unavailable` when fallback would be needed, instead of silently moving connection-local context to HTTP.

## Connection bounds

Each lease is serial. Reuse requires matching account, authorization, destination, proxy, protocol/capability/session headers, downstream API key, and execution scope. The identity capacity bucket excludes proxy/session/account aliases so duplicating an upstream credential cannot gain extra capacity. Credentials and header values never appear in SmartWS metrics.

Limits are process-local. Multiple app replicas each have their own pool; size the aggregate accordingly. The existing account scheduler still governs business concurrency. Configuration uses `gateway.openai_ws.*` or `GATEWAY_OPENAI_WS_*` environment variables:

| Key | Purpose |
| --- | --- |
| `max_conns_global`, `max_conns_per_account`, `max_conns_per_identity` | Connection ceilings; account auto-pool scaling also applies |
| `max_queued_global`, `max_queued_per_identity`, `queue_limit_per_conn` | Bounded acquisition/lease queues |
| `max_concurrent_handshakes`, `max_concurrent_handshakes_per_identity` | Concurrent handshake ceilings |
| `handshakes_per_second`, `handshakes_per_second_per_identity` | Handshake rate limits |
| `idle_timeout_seconds`, `max_conn_age_seconds` | Idle reclamation and maximum age between turns |
| `max_request_bytes`, `max_upstream_frame_bytes` | HTTP-to-WS request limit and incoming upstream WS message limit |
| `handshake_rejection_cooldown_seconds`, `max_handshake_rejections` | Temporary endpoint pause (1–300 seconds, default 30) and bounded entries (1–65536, default 1024) |

Defaults are conservative guardrails, not measured server capacity. HTTP requests above the WS size bound retain their entire body and use HTTP before dispatch. Existing HTTP and native-client body/frame limits remain additional bounds. Cancellation interrupts queued acquisition and active I/O; damaged or incomplete leases are evicted. Age/idle cleanup never cuts an active generation; an aged native session rejects the next turn before dispatch. HTTP adaptation opens connections on demand and does not create idle prewarm connections.

## Observation and release

`GET /api/v1/admin/ops/smart-ws` requires the existing admin/monitoring access. The Ops dashboard shows connections, active leases, acquisition/handshake counts and limits, reuse, fallback, queue rejection, cancellation, upstream terminal errors, transport failures, and uncertain dispatch. Counters are per process and reset on restart. Latency retains at most 256 successfully delivered samples, separates new/reused connections, and measures actual content rather than metadata or gateway notifications. These server-side counters do not establish end-to-end client success rates.

Handshake 401 and 403 have separate counters. Endpoint pauses, subsequent requests during pauses, and rejected continuations are separate from upstream terminal generation errors. Error events include `stage=ws_handshake`, the classification reason, account and proxy attribution, and the upstream host/path; no credentials or request/header dumps are added. `handshake_auth` remains a legacy combined 401/403 counter for API compatibility, not a credential-failure diagnosis.

Validation must include isolated fault injection and actual upstream/entry checks. Compare fixed account, credential, model, reasoning effort, request, session, and concurrency; report HTTP, cold WS, and warm WS separately. Record client terminal outcomes, transport failures, upstream errors, and replay counts independently. Small samples establish protocol behavior, not site-wide capacity or latency improvement.

Release an immutable image through the discovered blue/green operator, with new groups initially using HTTP. Validate the standby before switching, enable a bounded canary, and drain old HTTP/WS connections naturally. Roll back transport with the group control and roll back the application with the operator's previous image. Do not restart PostgreSQL/Redis or force-stop streams. Verify the real proxy configuration supports Upgrade, streaming, and sufficient read timeouts; source templates alone are not evidence of the live configuration.
