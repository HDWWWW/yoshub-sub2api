//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type smartWSCloseAfterDoneWriter struct {
	gin.ResponseWriter
	buffer *httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *smartWSCloseAfterDoneWriter) Flush() {
	w.ResponseWriter.Flush()
	if strings.Contains(w.buffer.Body.String(), "data: [DONE]") {
		w.cancel()
	}
}

func TestSmartWS_ChatCompletedBeforeClientCloseCountsSuccess(t *testing.T) {
	s, account, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		for _, event := range []string{forcedWSCreated, `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`, forcedWSCompleted} {
			if conn.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
				return
			}
		}
		_, _, _ = conn.ReadMessage()
	})
	c, rec := forcedWSContext(t, "/v1/chat/completions", 1, "close-after-done")
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	c.Writer = &smartWSCloseAfterDoneWriter{ResponseWriter: c.Writer, buffer: rec, cancel: cancel}
	result, err := s.ForwardAsChatCompletions(ctx, c, account,
		[]byte(`{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`), "", "")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.False(t, result.ClientDisconnect)
	require.Equal(t, 7, result.Usage.OutputTokens)
	snapshot := s.getOpenAIWSConnPool().SmartWSSnapshot()
	require.EqualValues(t, 1, snapshot.Counters["succeeded"])
	require.Zero(t, snapshot.Counters["canceled"])
}

func TestSmartWS_ConvertedJSONPreservesDownstreamUsage(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "normal"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			s := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
			account := &Account{ID: 71, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			body := []byte("data: " + forcedWSCompleted + "\n\n")
			upstream := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}}
			c, rec := forcedWSContext(t, "/v1/responses", 1, "json-conversion")
			if passthrough {
				_, err := s.handlePassthroughSSEToJSON(upstream, c, account, body, "gpt-5.1", "gpt-5.1")
				require.NoError(t, err)
			} else {
				_, err := s.handleSSEToJSON(upstream, c, account, body, "gpt-5.1", "gpt-5.1")
				require.NoError(t, err)
			}
			downstream, _ := forcedWSContext(t, "/v1/responses", 2, "json-downstream")
			response := rec.Result()
			defer response.Body.Close()
			result, err := s.handleNonStreamingResponse(downstream.Request.Context(), response, downstream, account, "gpt-5.1", "gpt-5.1")
			require.NoError(t, err)
			require.Equal(t, 12, result.usage.InputTokens, "the next proxy must parse JSON usage")
			require.Equal(t, 7, result.usage.OutputTokens)
			require.Equal(t, 3, result.usage.CacheReadInputTokens)
			require.Contains(t, response.Header.Get("Content-Type"), "application/json")
		})
	}
}

func TestSmartWS_FirstFailedEventRetainsUsageAndError(t *testing.T) {
	for _, chat := range []bool{false, true} {
		name := "responses"
		if chat {
			name = "chat"
		}
		t.Run(name, func(t *testing.T) {
			payload := `{"type":"response.failed","response":{"id":"resp_failed_usage","model":"gpt-5.1","service_tier":"priority","usage":{"input_tokens":17,"output_tokens":3},"error":{"code":"server_error","message":"failed after processing","status":500}}}`
			s, a, httpTransport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, _, _ = conn.ReadMessage()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
				_, _, _ = conn.ReadMessage()
			})
			c, rec := forcedWSContext(t, "/v1/responses", 1, "failed-usage")
			var result *OpenAIForwardResult
			var err error
			if chat {
				result, err = s.ForwardAsChatCompletions(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`), "", "")
			} else {
				result, err = s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"reasoning":{"effort":"high"},"input":"hello"}`))
			}
			require.Error(t, err)
			require.NotNil(t, result)
			require.Equal(t, 17, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.Equal(t, "resp_failed_usage", result.RequestID)
			require.Equal(t, "priority", result.UpstreamResponseServiceTier)
			require.NotNil(t, result.ReasoningEffort)
			require.Equal(t, "high", *result.ReasoningEffort)
			require.JSONEq(t, payload, rec.Body.String())
			require.Zero(t, httpTransport.callCount)
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, NextAccountStop, failure.NextAccountAction)
			require.EqualValues(t, 1, s.getOpenAIWSConnPool().SmartWSSnapshot().Counters["failed"])
		})
	}
}

func TestSmartWS_HTTPFallbackBusinessErrorDoesNotReplay(t *testing.T) {
	s, a, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
	payload := `{"error":{"code":"invalid_encrypted_content","message":"do not rewrite or resend"}}`
	transport.responses = []*http.Response{{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}}
	c, rec := forcedWSContext(t, "/v1/responses", 1, "fallback-error")
	_, err := s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"input":[{"type":"reasoning","encrypted_content":"keep-me"}]}`))
	require.Error(t, err)
	require.Equal(t, 1, transport.callCount)
	require.JSONEq(t, payload, rec.Body.String())
	require.Contains(t, string(transport.bodies[0]), "keep-me")
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.False(t, failure.ShouldRetryNextAccount())
}

func TestSmartWS_BareErrorAndTailRetainUsageAndResponseIdentity(t *testing.T) {
	for _, scenario := range []string{"no_tail_usage", "cyber_policy", "zero_tail_usage", "unrelated_tail"} {
		t.Run(scenario, func(t *testing.T) {
			code, tailID, tailUsage := "server_error", "resp_tail", ""
			if scenario == "cyber_policy" {
				code = scenario
			}
			if scenario == "zero_tail_usage" {
				tailUsage = `,"usage":{"input_tokens":0,"output_tokens":0}`
			}
			if scenario == "unrelated_tail" {
				tailID = "resp_other"
				tailUsage = `,"usage":{"input_tokens":999,"output_tokens":999}`
			}
			bare := `{"type":"error","status":400,"response_id":"resp_tail","error":{"code":"` + code + `","message":"failed"},"usage":{"input_tokens":17,"output_tokens":3}}`
			tail := `{"type":"response.failed","response":{"id":"` + tailID + `","status":"failed","error":{"code":"` + code + `","message":"failed"}` + tailUsage + `}}`
			s, a, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, _, _ = conn.ReadMessage()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(bare))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(tail))
				_, _, _ = conn.ReadMessage()
			})
			c, rec := forcedWSContext(t, "/v1/responses", 1, "usage-in-error")
			result, err := s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
			require.Error(t, err)
			require.NotNil(t, result)
			require.Equal(t, 17, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.JSONEq(t, bare, rec.Body.String())
			require.Equal(t, "resp_tail", result.RequestID)
			if scenario == "unrelated_tail" {
				require.Equal(t, "error", result.UpstreamTerminalEvent)
			} else {
				require.Equal(t, "response.failed", result.UpstreamTerminalEvent)
			}
			require.Zero(t, transport.callCount)
			if code == "cyber_policy" {
				mark := GetOpsCyberPolicy(c)
				require.NotNil(t, mark)
				require.Equal(t, 17, mark.UpstreamInTok)
				require.Equal(t, 3, mark.UpstreamOutTok)
			}
		})
	}
}

// Regression contract: an uncertain dispatch must never be automatically replayed.
func TestSmartWS_DispatchedDisconnectNeverReplays(t *testing.T) {
	for _, chat := range []bool{false, true} {
		name := "responses"
		if chat {
			name = "chat"
		}
		t.Run(name, func(t *testing.T) {
			var received atomic.Int32
			s, a, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err == nil {
					received.Add(1)
				}
				// The upstream definitely received the request, then disconnected
				// without acknowledging whether generation or tools were started.
			})
			c, rec := forcedWSContext(t, "/v1/responses", 1, "audit-thread")
			var err error
			if chat {
				_, err = s.ForwardAsChatCompletions(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`), "", "")
			} else {
				_, err = s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
			}
			require.Error(t, err)
			require.EqualValues(t, 1, received.Load())
			var failover *UpstreamFailoverError
			require.True(t, errors.As(err, &failover))
			require.False(t, failover.ShouldRetryNextAccount())
			require.Empty(t, rec.Body.String())
			t.Logf("CONFIRMED: upstream_received=1, next_account_retry_allowed=false, downstream_bytes=0, error_type=%T", err)
		})
	}
}

func TestSmartWS_EncryptedReasoningErrorPreservedWithoutResend(t *testing.T) {
	requests := make(chan []byte, 2)
	var calls atomic.Int32
	s, a, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		requests <- payload
		if calls.Add(1) == 1 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified"}}`))
		} else {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCompleted))
		}
		_, _, _ = conn.ReadMessage()
	})
	c, rec := forcedWSContext(t, "/v1/responses", 1, "audit-reasoning")
	_, err := s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"input":[{"type":"reasoning","encrypted_content":"audit-opaque"},{"type":"message","role":"user","content":"hello"}]}`))
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load())
	first := <-requests
	require.Contains(t, string(first), "audit-opaque")
	require.Equal(t, 400, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid_encrypted_content")
}

func TestSmartWS_PoolIsolatesDestinationAndAuth(t *testing.T) {
	for _, changed := range []string{"destination", "authorization", "project_header", "refreshed_authorization"} {
		t.Run(changed, func(t *testing.T) {
			makeServer := func(label string) *httptest.Server {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					for {
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
						if conn.WriteJSON(map[string]string{"type": "response.completed", "endpoint": label, "auth": r.Header.Get("Authorization"), "project": r.Header.Get("OpenAI-Project")}) != nil {
							return
						}
					}
				}))
				t.Cleanup(srv.Close)
				return srv
			}
			a, b := makeServer("A"), makeServer("B")
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			pool := newOpenAIWSConnPool(cfg)
			t.Cleanup(pool.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := openAIWSAcquireRequest{
				Account: &Account{ID: 71, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1},
				WSURL:   "ws" + strings.TrimPrefix(a.URL, "http") + "/v1/responses",
				Headers: make(http.Header), IsolationKey: "http:71:1:thread",
			}
			req.Headers.Set("Authorization", "Bearer audit-alpha")
			req.Headers.Set("OpenAI-Project", "audit-project-A")
			freshAuth := "Bearer audit-alpha"
			if changed == "refreshed_authorization" {
				req.HeadersFactory = func(_ context.Context, headers http.Header) (http.Header, error) {
					headers.Set("Authorization", freshAuth)
					return headers, nil
				}
			}
			first, err := pool.Acquire(ctx, req)
			require.NoError(t, err)
			id := first.ConnID()
			require.NoError(t, first.WriteJSONWithContextTimeout(ctx, map[string]string{"type": "response.create"}, time.Second))
			_, err = first.ReadMessageWithContextTimeout(ctx, time.Second)
			require.NoError(t, err)
			first.Release()
			switch changed {
			case "destination":
				req.WSURL = "ws" + strings.TrimPrefix(b.URL, "http") + "/v1/responses"
			case "authorization":
				req.Headers.Set("Authorization", "Bearer audit-beta")
			case "project_header":
				req.Headers.Set("OpenAI-Project", "audit-project-B")
			case "refreshed_authorization":
				freshAuth = "Bearer audit-beta"
			}
			second, err := pool.Acquire(ctx, req)
			require.NoError(t, err)
			defer second.Release()
			require.False(t, second.Reused())
			require.NotEqual(t, id, second.ConnID())
			require.NoError(t, second.WriteJSONWithContextTimeout(ctx, map[string]string{"type": "response.create"}, time.Second))
			message, err := second.ReadMessageWithContextTimeout(ctx, time.Second)
			require.NoError(t, err)
			wantEndpoint, wantAuth, wantProject := "A", "Bearer audit-alpha", "audit-project-A"
			switch changed {
			case "destination":
				wantEndpoint = "B"
			case "authorization", "refreshed_authorization":
				wantAuth = "Bearer audit-beta"
			case "project_header":
				wantProject = "audit-project-B"
			}
			require.Equal(t, wantEndpoint, gjson.GetBytes(message, "endpoint").String())
			require.Equal(t, wantAuth, gjson.GetBytes(message, "auth").String())
			require.Equal(t, wantProject, gjson.GetBytes(message, "project").String())
		})
	}
}
