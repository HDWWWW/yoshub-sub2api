//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/smartws"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type forcedWSTestDialer struct {
	url      string
	delegate openAIWSClientDialer
}

func (d forcedWSTestDialer) Dial(ctx context.Context, _ string, headers http.Header, proxy string) (openAIWSClientConn, int, http.Header, error) {
	return d.delegate.Dial(ctx, d.url, headers, proxy)
}

func forcedWSService(t *testing.T, handler http.HandlerFunc) (*OpenAIGatewayService, *Account, *httpUpstreamSequenceRecorder) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 4
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 4
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 2
	cfg.Gateway.OpenAIWS.SmartWSReadTimeoutSeconds = 2
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 2
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(forcedWSTestDialer{url: "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses", delegate: newDefaultOpenAIWSClientDialer()})
	t.Cleanup(pool.Close)
	httpUpstream := &httpUpstreamSequenceRecorder{}
	s := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool, httpUpstream: httpUpstream, toolCorrector: NewCodexToolCorrector()}
	s.openaiSmartPool = smartws.NewPool(func(ctx context.Context, d *websocket.Dialer, _ string, h http.Header) (*websocket.Conn, *http.Response, error) {
		return d.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", h)
	})
	t.Cleanup(s.openaiSmartPool.Close)
	a := &Account{ID: 71, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 4,
		Credentials: map[string]any{"api_key": "test-token", "base_url": server.URL},
		Extra:       map[string]any{"openai_apikey_responses_websockets_v2_mode": "ctx_pool", "openai_responses_mode": "force_responses"},
	}
	return s, a, httpUpstream
}

func forcedWSContext(t *testing.T, path string, key int64, thread string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, &Group{ID: 7, Platform: PlatformOpenAI, Status: "active", Hydrated: true, SmartWSMode: "enabled"}))
	c.Request.Header.Set("thread-id", thread)
	c.Set("api_key", &APIKey{ID: key})
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c, rec
}

var forcedWSUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

const forcedWSCreated = `{"type":"response.created","response":{"id":"resp_force","model":"gpt-5.1","status":"in_progress"}}`
const forcedWSCompleted = `{"type":"response.completed","response":{"id":"resp_force","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":12,"output_tokens":7,"input_tokens_details":{"cached_tokens":3}}}}`

func TestForcedWSResponsesAndChatPreserveFormatsAndUsage(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("chat=%v/stream=%v", chat, stream), func(t *testing.T) {
				requests := make(chan []byte, 1)
				s, a, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
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
					for _, event := range []string{forcedWSCreated, `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`, forcedWSCompleted} {
						if conn.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
							return
						}
					}
					_, _, _ = conn.ReadMessage()
				})
				path := "/v1/responses"
				body := fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"input":"hello"}`, stream)
				if chat {
					path = "/v1/chat/completions"
					body = fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`, stream)
				}
				c, rec := forcedWSContext(t, path, 1, "thread")
				var result *OpenAIForwardResult
				var err error
				if chat {
					result, err = s.ForwardAsChatCompletions(c.Request.Context(), c, a, []byte(body), "", "")
				} else {
					result, err = s.Forward(c.Request.Context(), c, a, []byte(body))
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.OpenAIWSMode)
				require.Equal(t, stream, result.Stream)
				require.Equal(t, 12, result.Usage.InputTokens)
				require.Equal(t, 7, result.Usage.OutputTokens)
				require.Equal(t, 3, result.Usage.CacheReadInputTokens)
				require.Zero(t, upstream.callCount)
				payload := <-requests
				require.Equal(t, "response.create", gjson.GetBytes(payload, "type").String())
				require.False(t, gjson.GetBytes(payload, "stream").Exists())
				require.Contains(t, rec.Body.String(), "hello")
				if stream {
					require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
					if chat {
						require.Contains(t, rec.Body.String(), "chat.completion.chunk")
					}
				} else if chat {
					require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
					require.Equal(t, "hello", gjson.GetBytes(rec.Body.Bytes(), "choices.0.message.content").String())
				} else {
					require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
					require.Equal(t, "resp_force", gjson.GetBytes(rec.Body.Bytes(), "id").String())
				}
			})
		}
	}
}

func TestForcedWSHappyFirstTokenPreservesRealTTFT(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, delta := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%v/delta=%v", passthrough, delta), func(t *testing.T) {
				c, rec := happyTestContext(t)
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, &Group{ID: 7, Platform: PlatformOpenAI, Status: "active", Hydrated: true, SmartWSMode: "enabled"}))
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				s, a, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
					conn, err := forcedWSUpgrader.Upgrade(w, r, http.Header{"X-Codex-Turn-State": {"private-state"}})
					if err != nil {
						return
					}
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					if conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated)) != nil {
						return
					}
					select {
					case <-rec.flushed:
					case <-time.After(2 * time.Second):
						return
					}
					time.Sleep(35 * time.Millisecond)
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCompleted))
					_, _, _ = conn.ReadMessage()
				})
				s.settingService = happyTestService(t, 0, delta).settingService
				a.Type = AccountTypeOAuth
				a.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
				a.Extra["openai_passthrough"] = passthrough
				result, err := s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
				require.NoError(t, err)
				require.True(t, result.OpenAIWSMode)
				require.Zero(t, upstream.callCount)
				want := openAIHappyFirstTokenEvent
				if delta {
					want = openAIHappyFirstTokenDeltaEvent
				}
				require.True(t, strings.HasPrefix(rec.Body.String(), want), rec.Body.String())
				require.Equal(t, 1, strings.Count(rec.Body.String(), `"gateway_status"`))
				require.NotNil(t, result.FirstTokenMs)
				require.GreaterOrEqual(t, *result.FirstTokenMs, 35)
				require.Equal(t, 7, result.Usage.OutputTokens)
				require.Empty(t, rec.Result().Header.Get("X-Codex-Turn-State"))
			})
		}
	}
}

func TestForcedWSChatWithHappySettingsPreservesChatProtocol(t *testing.T) {
	for _, delta := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("delta=%v/stream=%v", delta, stream), func(t *testing.T) {
				s, a, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
					conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated))
					time.Sleep(35 * time.Millisecond)
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
					_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCompleted))
					_, _, _ = conn.ReadMessage()
				})
				s.settingService = happyTestService(t, 0, delta).settingService
				a.Type = AccountTypeOAuth
				a.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
				c, rec := forcedWSContext(t, "/v1/chat/completions", 1, "thread")
				body := fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`, stream)
				result, err := s.ForwardAsChatCompletions(c.Request.Context(), c, a, []byte(body), "", "")
				require.NoError(t, err)
				require.True(t, result.OpenAIWSMode)
				require.Equal(t, 7, result.Usage.OutputTokens)
				require.Contains(t, rec.Body.String(), "hello")
				require.NotContains(t, rec.Body.String(), "gateway.status")
				require.NotContains(t, rec.Body.String(), "upstream_connected")
				if stream {
					require.NotNil(t, result.FirstTokenMs)
					// Existing Chat TTFT counts the first upstream event, including
					// response.created. It is separate from happy-first-token settings.
					require.Contains(t, rec.Body.String(), `"role":"assistant"`)
				} else {
					require.True(t, gjson.ValidBytes(rec.Body.Bytes()))
				}
			})
		}
	}
}

func TestForcedWSProgressTimeoutEvictsOnlyStalledLease(t *testing.T) {
	for _, phase := range []string{"headers", "created", "content"} {
		for _, chat := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chat=%v", phase, chat), func(t *testing.T) {
				var dials atomic.Int32
				closed := make(chan struct{})
				s, a, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
					conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					current := dials.Add(1)
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					if current > 1 {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCompleted))
					} else if phase != "headers" {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated))
						if phase == "content" {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
						}
					}
					_, _, _ = conn.ReadMessage()
					if current == 1 {
						close(closed)
					}
				})
				s.cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
				s.cfg.Gateway.OpenAIWS.SmartWSReadTimeoutSeconds = 1
				s.settingService = happyTestService(t, 0, true).settingService
				a.Type = AccountTypeOAuth
				a.Credentials = map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"}
				path := "/v1/responses"
				body := []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`)
				if chat {
					path = "/v1/chat/completions"
					body = []byte(`{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
				}
				forward := func(c *gin.Context) (*OpenAIForwardResult, error) {
					if chat {
						return s.ForwardAsChatCompletions(c.Request.Context(), c, a, body, "", "")
					}
					return s.Forward(c.Request.Context(), c, a, body)
				}
				c, rec := forcedWSContext(t, path, 1, "thread")
				started := time.Now()
				_, err := forward(c)
				require.Error(t, err)
				require.Less(t, time.Since(started), 2*time.Second)
				var failure *UpstreamFailoverError
				if !chat && phase != "content" {
					// Responses can retry a transport timeout before model output.
					// The failed dispatch is cleared before the HTTP handler retries.
					require.ErrorAs(t, err, &failure)
					require.True(t, failure.RetryableOnSameAccount)
					if phase == "headers" {
						require.Empty(t, rec.Body.String())
					} else {
						require.Contains(t, rec.Body.String(), "gateway.status.delta")
						require.NotContains(t, rec.Body.String(), smartWSUncertainCode)
					}
					require.False(t, smartWSHasDispatched(c))
				} else {
					if errors.As(err, &failure) {
						require.False(t, failure.ShouldRetryNextAccount())
					}
					require.True(t, smartWSHasDispatched(c))
				}
				if phase == "content" {
					require.Contains(t, rec.Body.String(), "hello")
				}

				select {
				case <-closed:
				case <-time.After(2 * time.Second):
					t.Fatal("stalled WS was not closed")
				}
				c2, _ := forcedWSContext(t, path, 1, "thread")
				result, err := forward(c2)
				require.NoError(t, err)
				require.Equal(t, 7, result.Usage.OutputTokens)
				require.Equal(t, int32(2), dials.Load(), "stalled lease must not be reused")
			})
		}
	}
}

func TestForcedWSReuseAndTenantThreadIsolation(t *testing.T) {
	var dials atomic.Int32
	s, a, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		dials.Add(1)
		defer conn.Close()
		for turn := 0; ; turn++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			if conn.WriteMessage(websocket.TextMessage, []byte(strings.ReplaceAll(forcedWSCompleted, "resp_force", fmt.Sprintf("resp_%d", turn)))) != nil {
				return
			}
		}
	})
	upstream.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_http_fallback","status":"completed","output":[],"usage":{"input_tokens":1}}`))}}
	for i, tc := range []struct {
		key    int64
		thread string
		want   int32
	}{{1, "a", 1}, {1, "a", 1}, {2, "a", 2}, {1, "b", 2}, {1, "a", 2}} {
		c, _ := forcedWSContext(t, "/v1/responses", tc.key, tc.thread)
		result, err := s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":false,"input":"hello"}`))
		require.NoError(t, err, "request %d", i)
		require.Equal(t, i != 3, result.OpenAIWSMode)
		require.Equal(t, tc.want, dials.Load())
	}
}

func TestForcedWSErrorsBeforeAcceptanceKeepStatus(t *testing.T) {
	for _, handshake := range []bool{false, true} {
		for _, status := range []int{400, 401, 403, 429, 503} {
			t.Run(fmt.Sprintf("handshake=%v/status=%d", handshake, status), func(t *testing.T) {
				s, a, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
					payload := fmt.Sprintf(`{"type":"error","status":%d,"error":{"message":"upstream rejected","type":"server_error"}}`, status)
					if handshake {
						w.WriteHeader(status)
						_, _ = io.WriteString(w, payload)
						return
					}
					conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					_, _, _ = conn.ReadMessage()
					_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
					_, _, _ = conn.ReadMessage()
				})
				c, rec := forcedWSContext(t, "/v1/responses", 1, "a")
				req, err := s.buildUpstreamRequest(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`), "token", true, "", false)
				require.NoError(t, err)
				resp, err := s.doOpenAIUpstream(req, "", a, c)
				require.NoError(t, err)
				defer resp.Body.Close()
				require.Equal(t, status, resp.StatusCode)
				require.True(t, isOpenAIHTTPWSResponse(resp))
				require.Empty(t, rec.Body.String())
				require.Zero(t, upstream.callCount)
			})
		}
	}
}

func TestForcedWSCancelClosesConnectionAndStopsRead(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			started := make(chan struct{})
			closed := make(chan struct{})
			s, a, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				if accepted {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated))
				}
				close(started)
				_, _, _ = conn.ReadMessage()
				close(closed)
			})
			c, _ := forcedWSContext(t, "/v1/responses", 1, "a")
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			req, err := s.buildUpstreamRequest(ctx, c, a, []byte(`{"model":"gpt-5.1","stream":true}`), "token", true, "", false)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				resp, err := s.doOpenAIUpstream(req, "", a, c)
				if resp != nil {
					_, err = io.ReadAll(resp.Body)
					_ = resp.Body.Close()
				}
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("request not sent")
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(3 * time.Second):
				t.Fatal("read did not stop")
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream connection not closed")
			}
		})
	}
}

func TestSmartWSConfiguredFrameLimitNeverReplays(t *testing.T) {
	var dispatched atomic.Int32
	s, account, httpTransport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		dispatched.Add(1)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated))
		_ = conn.WriteJSON(map[string]string{"type": "response.output_text.delta", "delta": strings.Repeat("x", 4096)})
		_, _, _ = conn.ReadMessage()
	})
	s.cfg.Gateway.OpenAIWS.MaxUpstreamFrameBytes = 2048
	c, _ := forcedWSContext(t, "/v1/responses", 1, "frame-limit")
	_, err := s.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
	require.Error(t, err)
	require.EqualValues(t, 1, dispatched.Load())
	require.Zero(t, httpTransport.callCount)
	var retry *UpstreamFailoverError
	if errors.As(err, &retry) {
		require.False(t, retry.ShouldRetryNextAccount())
	}
	require.EqualValues(t, 1, s.getOpenAIWSConnPool().SmartWSSnapshot().Counters["dispatch_uncertain"])
}

func TestForcedWSDecisionAndValidation(t *testing.T) {
	s, a, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected network request") })
	for _, mode := range []string{"off", "http_bridge", "ctx_pool", "passthrough"} {
		a.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
		expected := OpenAIUpstreamTransportHTTPSSE
		if mode == "ctx_pool" || mode == "passthrough" {
			expected = OpenAIUpstreamTransportResponsesWebsocketV2
		}
		require.Equal(t, expected, s.smartWSEligibleAccount(a).Transport)
		require.Equal(t, mode, a.Extra["openai_apikey_responses_websockets_v2_mode"])
	}
	a.Extra["openai_ws_force_http"] = true
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, s.smartWSEligibleAccount(a).Transport)
	delete(a.Extra, "openai_ws_force_http")
	s.cfg.Gateway.OpenAIWS.ForceHTTP = true
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, s.smartWSEligibleAccount(a).Transport)
	s.cfg.Gateway.OpenAIWS.ForceHTTP = false
	s.cfg.Gateway.OpenAIWS.Enabled = false
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, s.smartWSEligibleAccount(a).Transport)
	s.cfg.Gateway.OpenAIWS.Enabled = true
	a.Type = AccountTypeOAuth
	s.pluginManager = &PluginManager{}
	s.pluginManager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, unavailable: "test unavailable"})
	require.Equal(t, "openai_oauth_plugin", s.smartWSEligibleAccount(a).Reason)
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, s.smartWSEligibleAccount(a).Transport)
	s.pluginManager = nil
	a.Extra["openai_ws_force_http"] = true
	require.Equal(t, OpenAIUpstreamTransportHTTPSSE, s.smartWSEligibleAccount(a).Transport)
}

// SmartWS leaves provider-managed HTTP history on HTTP; native WS still owns
// its existing previous_response_id continuation implementation.
func TestSmartWSHTTPHistoryStaysHTTP(t *testing.T) {
	s, a, upstream := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("continuation must not dial WS") })
	body := `{"model":"gpt-5.1","stream":false,"previous_response_id":"resp_http","input":[{"type":"function_call_output","call_id":"call_a","output":"ok"}]}`
	upstream.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_next","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`))}}
	c, _ := forcedWSContext(t, "/v1/responses", 1, "tools")
	result, err := s.Forward(c.Request.Context(), c, a, []byte(body))
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	require.Equal(t, 1, upstream.callCount)
	require.Equal(t, "resp_http", gjson.GetBytes(upstream.bodies[0], "previous_response_id").String())
	require.Equal(t, "call_a", gjson.GetBytes(upstream.bodies[0], "input.0.call_id").String())
}

func TestForcedWSCompactAndDisabledStayHTTP(t *testing.T) {
	for _, path := range []string{"/v1/responses/compact", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			s, a, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must stay HTTP") })
			if path == "/v1/responses" {
				a.Extra["openai_ws_force_http"] = true
			}
			upstream.responses = []*http.Response{{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"resp_http","usage":{"input_tokens":1,"output_tokens":2}}`))}}
			c, _ := forcedWSContext(t, path, 1, "a")
			req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "http://unused"+path, strings.NewReader(`{"model":"gpt-5.1"}`))
			require.NoError(t, err)
			resp, err := s.doOpenAIUpstream(req, "", a, c)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.False(t, isOpenAIHTTPWSResponse(resp))
			require.Equal(t, 1, upstream.callCount)
		})
	}
}

func TestForcedWSBrokenStreamIsNotSuccessfulOrReused(t *testing.T) {
	for _, chat := range []bool{false, true} {
		for _, failure := range []string{"disconnect", "bad_json", "error"} {
			t.Run(fmt.Sprintf("chat=%v/%s", chat, failure), func(t *testing.T) {
				var dials atomic.Int32
				s, a, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
					conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					n := dials.Add(1)
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					if n > 1 {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCompleted))
						_, _, _ = conn.ReadMessage()
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated))
					switch failure {
					case "bad_json":
						_ = conn.WriteMessage(websocket.TextMessage, []byte("invalid"))
					case "error":
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"type":"server_error","message":"interrupted"}}`))
					}
				})
				for i := 0; i < 2; i++ {
					c, _ := forcedWSContext(t, "/v1/responses", 1, "a")
					var err error
					if chat {
						_, err = s.ForwardAsChatCompletions(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"hello"}]}`), "", "")
					} else {
						_, err = s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
					}
					if i == 0 {
						require.Error(t, err)
					} else {
						require.NoError(t, err)
					}
				}
				require.EqualValues(t, 2, dials.Load())
			})
		}
	}
}
