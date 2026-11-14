//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestSmartWSGenerationTimeoutAndCloseDiagnostics(t *testing.T) {
	for _, mode := range []string{"before_output", "after_output", "close_1008"} {
		t.Run(mode, func(t *testing.T) {
			s, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				ws, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer ws.Close()
				if _, _, err = ws.ReadMessage(); err != nil {
					return
				}
				_ = ws.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated))
				if mode == "after_output" {
					_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"partial answer"}`))
				}
				if mode == "close_1008" {
					_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1008, "private-upstream-close-text"), time.Now().Add(time.Second))
				}
				_, _, _ = ws.ReadMessage()
			})
			s.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 900
			s.cfg.Gateway.OpenAIWS.IdleTimeoutSeconds = 300
			s.cfg.Gateway.OpenAIWS.SmartWSReadTimeoutSeconds = 1
			core, logs := observer.New(zap.InfoLevel)
			c, rec := forcedWSContext(t, "/v1/responses", 1, "timeout-diagnostic")
			c.Request = c.Request.WithContext(logger.IntoContext(c.Request.Context(), zap.New(core)))
			started := time.Now()
			_, err := s.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"private-request-input"}`))
			require.Error(t, err)
			require.Less(t, time.Since(started), 5*time.Second)
			require.Zero(t, upstream.callCount)
			entries := logs.FilterMessage("smart_ws.stream_failure").All()
			require.Len(t, entries, 1)
			fields := entries[0].ContextMap()
			require.Equal(t, float64(1), fields["read_timeout_seconds"])
			require.Equal(t, mode == "before_output", fields["retry_before_output"])
			detail := c.GetString(OpsUpstreamErrorDetailKey)
			require.True(t, gjson.Valid(detail))
			if mode == "close_1008" {
				require.EqualValues(t, 1008, fields["close_code"])
				require.Equal(t, "connection_interrupted", fields["reason"])
			} else {
				require.Equal(t, "read_timeout", fields["reason"])
				require.Equal(t, "read_timeout", gjson.Get(detail, "reason").String())
			}
			if mode == "before_output" {
				var retry *UpstreamFailoverError
				require.ErrorAs(t, err, &retry)
				require.True(t, retry.RetryableOnSameAccount)
				require.Empty(t, rec.Body.String())
			} else {
				require.Contains(t, rec.Body.String(), smartWSUncertainCode)
			}
			encoded, marshalErr := json.Marshal(fields)
			require.NoError(t, marshalErr)
			for _, secret := range []string{"private-upstream-close-text", "private-request-input"} {
				require.NotContains(t, detail, secret)
				require.NotContains(t, string(encoded), secret)
				require.NotContains(t, rec.Body.String(), secret)
			}
		})
	}
}

func TestSmartWSHandshakeDiagnosticsHTTPFailurePreservesResponseAndSecrets(t *testing.T) {
	const upstreamBody = `{"error":{"code":"forbidden","type":"permission_error","message":"private-customer-input"}}`
	s, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("Cf-Ray", "a56a26360b697588-SEA")
		w.Header().Set("X-Request-Id", "085687b9-4cac-4e7c-a959-dc15613ded24")
		w.Header().Set("Via", "private-proxy-detail")
		w.Header().Set("Set-Cookie", "private-cookie")
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, upstreamBody)
	})
	core, logs := observer.New(zap.InfoLevel)
	c, _ := forcedWSContext(t, "/v1/responses", 1, "private-thread")
	ctx := logger.IntoContext(c.Request.Context(), zap.New(core).With(zap.String("request_id", "req-diagnostic")))
	c.Request = c.Request.WithContext(ctx)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{"model":"test-model","input":"private-request-input","stream":true}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer private-access-token")
	req.Header.Set("Session_id", "private-session")
	req.Header.Set("X-Codex-Turn-State", "private-turn-state")
	before := req.Header.Clone()
	resp, err := s.doOpenAIHTTPOverWS(c, req, "", account)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, upstreamBody, string(body))
	require.Equal(t, "30", resp.Header.Get("Retry-After"))
	require.Equal(t, before, req.Header)
	require.Zero(t, upstream.callCount, "diagnostics must not replay or fall back")
	entries := logs.FilterMessage("smart_ws.handshake").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	require.Equal(t, zap.WarnLevel, entries[0].Level)
	require.Equal(t, "req-diagnostic", fields["request_id"])
	require.Equal(t, "http_adapter", fields["origin"])
	require.Equal(t, "not_sent", fields["dispatch_state"])
	require.Equal(t, "ws_forbidden", fields["reason"])
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	for _, secret := range []string{"private-access-token", "private-session", "private-turn-state", "private-request-input", "private-customer-input", "private-proxy-detail", "private-cookie"} {
		require.NotContains(t, string(encoded), secret)
	}
	require.Contains(t, string(encoded), "a56a26360b697588-SEA")
	require.Contains(t, string(encoded), `"error_code":"forbidden"`)
	require.Contains(t, string(encoded), `"retry_after_seconds":30`)
	require.True(t, (&OpsSystemLogSink{}).shouldIndex(&logger.LogEvent{Level: "warn", Message: entries[0].Message, Fields: fields}))
}

func TestSmartWSHandshakeDiagnosticsFingerprintScopeAndUntrustedValues(t *testing.T) {
	var first, second smartWSHandshakeDiagnostics
	value := http.Header{"Authorization": {"Bearer secret-token"}, "Session_id": {"private-session"}}
	before := value.Clone()
	fingerprint := first.fingerprint(value)
	require.Len(t, fingerprint, 32)
	require.Equal(t, fingerprint, first.fingerprint(value))
	require.NotEqual(t, fingerprint, second.fingerprint(value))
	value.Set("Session_id", "different-session")
	require.NotEqual(t, fingerprint, first.fingerprint(value))
	summary := first.headerSummary(before, smartWSDiagnosticRequestHeaders)
	require.True(t, summary["authorization"].Present)
	require.Equal(t, len("Bearer secret-token"), summary["authorization"].Bytes)
	require.False(t, summary["x-codex-turn-state"].Present)
	headers := http.Header{}
	for _, name := range []string{"Server", "Cf-Ray", "Cf-Mitigated", "X-Request-Id", "X-OpenAI-Request-Id", "Via", "Retry-After", "Content-Type"} {
		headers.Set(name, "Bearer secret-token private-session\r\nforged-log")
	}
	response := first.responseSummary(headers, []byte(`{"error":{"code":"secret-token","type":"private-session","message":"private-input"}}`))
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	for _, secret := range []string{"secret-token", "private-session", "private-input", "forged-log"} {
		require.NotContains(t, string(encoded), secret)
	}
	require.Equal(t, "unrecognized", response["error_code"])
	require.Equal(t, "json", response["body_kind"])
}

func TestSmartWSHandshakeDiagnosticsSamplingAndAttribution(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	pool := &openAIWSConnPool{}
	proxyID := int64(88)
	req := openAIWSAcquireRequest{Account: &Account{ID: 7, ProxyID: &proxyID}, WSURL: "wss://example.invalid/private-path?secret=query", ProxyURL: "http://private-user:private-password@proxy.invalid", Headers: http.Header{}}
	for i := 0; i < 128; i++ {
		pool.observeSmartWSHandshake(ctx, req, true, 0, nil, nil, time.Millisecond)
	}
	require.Len(t, logs.All(), 3, "first success and each 64th success only")
	pool.observeSmartWSHandshake(ctx, req, false, 0, nil, context.DeadlineExceeded, time.Second)
	pool.observeSmartWSHandshake(ctx, req, true, 0, nil, nil, time.Millisecond)
	require.Len(t, logs.All(), 5, "retain failure and next successful handshake")
	require.Equal(t, "timeout", logs.All()[3].ContextMap()["reason"])
	fields := logs.All()[4].ContextMap()
	require.Equal(t, int64(7), fields["account_id"])
	require.Equal(t, int64(88), fields["proxy_id"])
	require.Equal(t, "native_or_background", fields["origin"])
	require.Equal(t, "connected", fields["result"])
	require.True(t, (&OpsSystemLogSink{}).shouldIndex(&logger.LogEvent{Level: "info", Message: "smart_ws.handshake", Fields: fields}))
	require.False(t, (&OpsSystemLogSink{}).shouldIndex(&logger.LogEvent{Level: "info", Message: "unrelated", Fields: fields}))
	encoded, err := json.Marshal(fields)
	require.NoError(t, err)
	for _, secret := range []string{"private-user", "private-password", "private-path", "secret=query"} {
		require.NotContains(t, string(encoded), secret)
	}
}

func TestSmartWSHandshakeDiagnosticsConcurrent(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	ctx := logger.IntoContext(context.Background(), zap.New(core))
	pool := &openAIWSConnPool{}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.observeSmartWSHandshake(ctx, openAIWSAcquireRequest{}, false, 403, nil, &openAIWSHandshakeError{Err: errors.New("private-error")}, time.Millisecond)
		}()
	}
	wg.Wait()
	require.Len(t, logs.All(), 64)
	scope := logs.All()[0].ContextMap()["fingerprint_scope"]
	for _, entry := range logs.All() {
		require.Equal(t, scope, entry.ContextMap()["fingerprint_scope"])
	}
}
