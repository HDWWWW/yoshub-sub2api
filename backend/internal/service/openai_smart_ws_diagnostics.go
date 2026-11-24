package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const smartWSHandshakeLogComponent = "service.openai_ws_handshake"

// The key is random, pool-local and never persisted. Fingerprints can correlate
// attempts in one process without publishing hashes suitable for guessing
// low-entropy session/header values offline.
type smartWSHandshakeDiagnostics struct {
	once         sync.Once
	key          [32]byte
	keyReady     bool
	successes    atomic.Uint64
	afterFailure atomic.Bool
}

func (d *smartWSHandshakeDiagnostics) fingerprint(value any) string {
	d.once.Do(func() { _, err := rand.Read(d.key[:]); d.keyReady = err == nil })
	if !d.keyReady {
		return ""
	}
	mac := hmac.New(sha256.New, d.key[:])
	if json.NewEncoder(mac).Encode(value) != nil {
		return ""
	}
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

type smartWSHeaderDiagnostic struct {
	Present     bool   `json:"present"`
	Values      int    `json:"values"`
	Bytes       int    `json:"bytes"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

var smartWSDiagnosticRequestHeaders = []string{
	"authorization", "chatgpt-account-id", "openai-project", "openai-organization",
	"user-agent", "originator", "version", "origin", "accept", "accept-language", "content-type",
	"openai-beta", "x-codex-beta-features", "x-codex-routing-hint", "x-openai-fedramp",
	"session_id", "conversation_id", "thread-id", "x-codex-installation-id", "x-codex-window-id",
	"x-codex-turn-state", "x-codex-turn-metadata", responsesLiteHeaderKey,
}

func (d *smartWSHandshakeDiagnostics) headerSummary(headers http.Header, names []string) map[string]smartWSHeaderDiagnostic {
	out := make(map[string]smartWSHeaderDiagnostic, len(names))
	for _, name := range names {
		values := headers.Values(name)
		entry := smartWSHeaderDiagnostic{Present: len(values) > 0, Values: len(values)}
		for _, value := range values {
			entry.Bytes += len(value)
		}
		if entry.Present {
			entry.Fingerprint = d.fingerprint([]any{name, values})
		}
		out[name] = entry
	}
	return out
}

var smartWSCFRayPattern = regexp.MustCompile(`^[0-9a-fA-F]{16}-[A-Z0-9]{3}$`)
var smartWSRequestIDPattern = regexp.MustCompile(`^(?:[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|req_[0-9a-fA-F]{16,64})$`)

func smartWSDiagnosticErrorLabel(value string) string {
	switch value {
	case "invalid_api_key", "invalid_authentication", "authentication_error", "permission_error", "permission_denied",
		"forbidden", "unauthorized", "access_denied", "account_deactivated", "account_suspended", "account_disabled",
		"token_expired", "invalid_token", "workspace_deactivated", "organization_deactivated",
		"rate_limit_exceeded", "rate_limit_error", "insufficient_quota", "server_error", "upstream_error",
		"invalid_request_error", "model_not_found", "unsupported_country_region_territory":
		return value
	case "":
		return ""
	default:
		// Error fields are untrusted and can echo arbitrary request contents.
		return "unrecognized"
	}
}

func (d *smartWSHandshakeDiagnostics) responseSummary(headers http.Header, body []byte) map[string]any {
	out := map[string]any{
		"headers":             d.headerSummary(headers, []string{"server", "via", "cf-ray", "cf-mitigated", "x-request-id", "x-openai-request-id", "retry-after", "content-type", "content-length"}),
		"body_observed_bytes": len(body),
		"body_fingerprint":    d.fingerprint(body),
	}
	server := strings.ToLower(strings.TrimSpace(headers.Get("Server")))
	switch server {
	case "cloudflare", "nginx", "envoy", "openresty":
		out["server"] = server
	case "":
	default:
		out["server"] = "other"
	}
	if ray := headers.Get("Cf-Ray"); smartWSCFRayPattern.MatchString(ray) {
		out["cf_ray"] = ray
	}
	if strings.EqualFold(headers.Get("Cf-Mitigated"), "challenge") {
		out["cf_mitigated"] = "challenge"
	}
	for _, name := range []string{"X-Request-Id", "X-OpenAI-Request-Id"} {
		if id := headers.Get(name); smartWSRequestIDPattern.MatchString(id) {
			out[strings.ToLower(name)] = id
		}
	}
	if value := strings.TrimSpace(headers.Get("Retry-After")); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
			out["retry_after_seconds"] = seconds
		} else if deadline, err := http.ParseTime(value); err == nil {
			out["retry_after_at"] = deadline.UTC().Format(time.RFC3339)
		}
	}
	mediaType, _, _ := mime.ParseMediaType(headers.Get("Content-Type"))
	switch mediaType {
	case "application/json", "text/html", "text/plain":
		out["content_type"] = mediaType
	}
	trimmed := strings.TrimSpace(string(body))
	kind := "other"
	if trimmed == "" {
		kind = "empty"
	} else if json.Valid(body) {
		kind = "json"
		var payload struct {
			Error struct {
				Code string `json:"code"`
				Type string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &payload) == nil {
			out["error_code"] = smartWSDiagnosticErrorLabel(payload.Error.Code)
			out["error_type"] = smartWSDiagnosticErrorLabel(payload.Error.Type)
		}
	} else if lower := strings.ToLower(trimmed); strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") {
		kind = "html"
	}
	out["body_kind"] = kind
	// This is a content marker, not an assertion about who generated a refusal.
	out["challenge_marker"] = strings.Contains(strings.ToLower(trimmed), "cf-chl-")
	return out
}

func (p *openAIWSConnPool) observeSmartWSHandshake(ctx context.Context, req openAIWSAcquireRequest, hasConn bool, status int, responseHeaders http.Header, dialErr error, elapsed time.Duration) {
	d := &p.handshakeDiagnostics
	succeeded := dialErr == nil && hasConn
	if succeeded {
		n := d.successes.Add(1)
		recoverySample := d.afterFailure.Swap(false)
		if n != 1 && n%64 != 0 && !recoverySample {
			return
		}
		status = http.StatusSwitchingProtocols
	} else {
		d.afterFailure.Store(true)
	}
	var body []byte
	var handshakeErr *openAIWSHandshakeError
	if errors.As(dialErr, &handshakeErr) && handshakeErr != nil {
		body = handshakeErr.Body
	}
	if status >= 400 && p.onHandshakeStatus != nil {
		p.onHandshakeStatus(ctx, req.Account, status, body)
	}
	result, reason := "failed", "transport_error"
	if succeeded {
		result, reason = "connected", "upgrade_accepted"
	} else if status >= 400 {
		reason = classifySmartWSHandshake(status, body).category
	} else if errors.Is(dialErr, context.Canceled) {
		reason = "canceled"
	} else if errors.Is(dialErr, context.DeadlineExceeded) {
		reason = "timeout"
	}
	accountID, proxyID := int64(0), int64(0)
	if req.Account != nil {
		accountID = req.Account.ID
		if req.Account.ProxyID != nil {
			proxyID = *req.Account.ProxyID
		}
	}
	origin := "native_or_background"
	if strings.HasPrefix(req.IsolationKey, "http:") {
		origin = "http_adapter"
	}
	endpoint := "custom"
	if target, err := url.Parse(req.WSURL); err == nil {
		switch target.Hostname() {
		case "chatgpt.com":
			endpoint = "chatgpt_codex"
		case "api.openai.com":
			endpoint = "openai_platform"
		}
	}
	p.gate.mu.Lock()
	conns, dialing, queued := p.gate.conns, p.gate.dialing, p.gate.queued
	p.gate.mu.Unlock()
	fields := []zap.Field{
		zap.String("component", smartWSHandshakeLogComponent), zap.String("stage", "ws_handshake"),
		zap.String("result", result), zap.String("reason", reason), zap.String("dispatch_state", "not_sent"),
		zap.String("origin", origin), zap.String("endpoint", endpoint), zap.Int("upstream_status_code", status),
		zap.Int64("account_id", accountID), zap.Int64("proxy_id", proxyID), zap.Bool("proxy_enabled", req.ProxyURL != ""),
		zap.Int64("handshake_ms", elapsed.Milliseconds()), zap.Bool("connection_reused", false),
		zap.Int("pool_reserved_connections", conns), zap.Int("pool_handshaking", dialing), zap.Int("pool_acquiring", queued),
		zap.String("fingerprint_scope", d.fingerprint("scope")),
		zap.String("target_fingerprint", d.fingerprint([]string{req.WSURL, req.ProxyURL})),
		zap.String("request_headers_fingerprint", d.fingerprint(req.Headers)),
		zap.Any("request_header_summary", d.headerSummary(req.Headers, smartWSDiagnosticRequestHeaders)),
		zap.Any("response_summary", d.responseSummary(responseHeaders, body)),
	}
	log := logger.FromContext(ctx)
	if succeeded {
		log.Info("smart_ws.handshake", fields...)
	} else {
		log.Warn("smart_ws.handshake", fields...)
	}
}
