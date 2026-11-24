//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSmartWSNativeFirstFramePreservesContinuationWithoutStickyBinding(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModeCtxPool, service.OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				firstPayload: `{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_existing","input":[{"role":"user","content":"continue"}]}`,
				ingressMode:  mode,
				group: &service.Group{ID: 4201, Platform: service.PlatformOpenAI, Status: service.StatusActive,
					Hydrated: true, SmartWSMode: "disabled"},
			})
			require.Equal(t, "resp_existing", gjson.GetBytes(got.upstreamFirstPayload, "previous_response_id").String())
			require.Len(t, got.logs, 1)
		})
	}
}

func TestSmartWSHTTPHandlerRetriesBeforeOutput(t *testing.T) {
	for _, ingress := range []string{"responses", "chat", "messages"} {
		for _, httpStatus := range []int{0, -503, 429, 503, 507, 520} {
			// Messages currently uses the HTTP Responses bridge, without SmartWS.
			if ingress == "messages" && httpStatus <= 0 {
				continue
			}
			if ingress != "responses" && httpStatus != 0 && httpStatus != 503 {
				continue
			}
			for _, tc := range []struct {
				name     string
				retries  int
				failures int
				accounts []string
			}{
				{"same_account", 1, 1, []string{"Bearer local-first", "Bearer local-first"}},
				{"switch_account", 0, 1, []string{"Bearer local-first", "Bearer local-second"}},
				{"exhausted", 1, 4, []string{"Bearer local-first", "Bearer local-first", "Bearer local-second", "Bearer local-second"}},
			} {
				t.Run(fmt.Sprintf("%s/status=%d/%s", ingress, httpStatus, tc.name), func(t *testing.T) {
					gin.SetMode(gin.TestMode)
					var attempts atomic.Int32
					requests := make(chan string, 8)
					upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if httpStatus > 0 {
							if websocket.IsWebSocketUpgrade(r) {
								w.WriteHeader(http.StatusUpgradeRequired)
								return
							}
							requests <- r.Header.Get("Authorization")
							if attempts.Add(1) <= int32(tc.failures) {
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(httpStatus)
								_, _ = fmt.Fprint(w, `{"error":{"type":"upstream_error","code":"provider_busy","message":"temporary provider rejection"}}`)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = fmt.Fprint(w, "data: "+`{"type":"response.created","response":{"id":"successful_attempt","model":"gpt-5.1","status":"in_progress","output":[]}}`+"\n\n")
							_, _ = fmt.Fprint(w, "data: "+`{"type":"response.output_text.delta","item_id":"msg_success","output_index":0,"content_index":0,"delta":"success"}`+"\n\n")
							_, _ = fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"successful_attempt","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"success"}]}],"usage":{"input_tokens":12,"output_tokens":7}}}`+"\n\n")
							return
						}

						conn, err := upgrader.Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer conn.Close()
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
						requests <- r.Header.Get("Authorization")
						if attempts.Add(1) <= int32(tc.failures) {
							if httpStatus < 0 {
								_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"error","status":%d,"error":{"type":"upstream_error","code":"provider_busy","message":"temporary provider rejection"}}`, -httpStatus)))
								return
							}
							// Chat/Messages converters commit their opening event on response.created.
							// Exercise a truly uncommitted disconnect for those protocols.
							if ingress == "responses" {
								_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"failed_attempt","status":"in_progress","output":[],"usage":null}}`))
							}
							return
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"successful_attempt","model":"gpt-5.1","status":"in_progress","output":[]}}`))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","item_id":"msg_success","output_index":0,"content_index":0,"delta":"success"}`))
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"successful_attempt","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"success"}]}],"usage":{"input_tokens":12,"output_tokens":7}}}`))
					}))
					t.Cleanup(upstream.Close)
					cfg := &config.Config{RunMode: config.RunModeSimple}
					cfg.Default.RateMultiplier = 1
					cfg.Gateway.MaxAccountSwitches = 1
					cfg.Security.URLAllowlist.AllowInsecureHTTP = true
					cfg.Gateway.OpenAIWS.Enabled = true
					cfg.Gateway.OpenAIWS.APIKeyEnabled = true
					cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
					cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
					cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 2
					cfg.Gateway.OpenAIWS.SmartWSReadTimeoutSeconds = 2
					accounts := make([]service.Account, 0, 2)
					for i, key := range []string{"local-first", "local-second"} {
						accounts = append(accounts, service.Account{ID: 99910 + int64(i), Platform: service.PlatformOpenAI,
							Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: i + 1,
							Credentials: map[string]any{"api_key": key, "base_url": upstream.URL},
							Extra: map[string]any{"openai_responses_mode": "force_responses", "openai_apikey_responses_websockets_v2_mode": "ctx_pool",
								"same_account_retry_count": tc.retries}})
					}
					repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
					usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
					billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
					t.Cleanup(billing.Stop)
					gateway := service.NewOpenAIGatewayService(repo, usageRepo, nil, nil, nil, nil, nil, cfg, nil, nil,
						service.NewBillingService(cfg, nil), nil, billing, repository.NewHTTPUpstream(cfg), &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
					t.Cleanup(gateway.CloseOpenAIWSPool)
					h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
						service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
					group := &service.Group{ID: 99912, Platform: service.PlatformOpenAI, Status: service.StatusActive,
						Hydrated: true, RateMultiplier: 1, SmartWSMode: "enabled", AllowMessagesDispatch: true}
					ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), ctxkey.Group, group), 8*time.Second)
					defer cancel()
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					path, body := "/v1/responses", `{"model":"gpt-5.1","stream":true,"input":"hello"}`
					if ingress != "responses" {
						path = "/v1/chat/completions"
						if ingress == "messages" {
							path = "/v1/messages"
						}
						body = `{"model":"gpt-5.1","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
					}
					c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
					c.Request.Header.Set("Content-Type", "application/json")
					c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 99913, GroupID: &group.ID, Group: group,
						User: &service.User{ID: 99914, Status: service.StatusActive}})
					c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 99914})
					switch ingress {
					case "chat":
						h.ChatCompletions(c)
					case "messages":
						h.Messages(c)
					default:
						h.Responses(c)
					}
					require.EqualValues(t, len(tc.accounts), attempts.Load(), rec.Body.String())
					for _, account := range tc.accounts {
						require.Equal(t, account, <-requests)
					}
					require.NotContains(t, rec.Body.String(), "failed_attempt")
					require.NotContains(t, rec.Body.String(), "smart_ws_dispatch_uncertain")
					if tc.name == "exhausted" {
						expectedStatus := http.StatusBadGateway
						if httpStatus != 0 {
							expectedStatus = httpStatus
							if expectedStatus < 0 {
								expectedStatus = -expectedStatus
							}
							require.Equal(t, "provider_busy", gjson.Get(rec.Body.String(), "error.code").String())
						}
						require.Equal(t, expectedStatus, rec.Code)
						require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
						require.Empty(t, usageRepo.created)
						return
					}
					require.Equal(t, http.StatusOK, rec.Code)
					require.Contains(t, rec.Body.String(), "success")
					require.NotContains(t, rec.Body.String(), `"type":"error"`)
					if ingress == "responses" {
						require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.completed"`))
					}
					select {
					case usage := <-usageRepo.created:
						require.Equal(t, 12, usage.InputTokens)
						require.Equal(t, 7, usage.OutputTokens)
					case <-time.After(time.Second):
						t.Fatal("successful attempt usage was not recorded")
					}
					require.Empty(t, usageRepo.created, "only the successful attempt records usage")
				})
			}
		}
	}
}

// Use the public handlers, a real local WebSocket and the normal usage writer:
// a service-only test cannot detect appended handler errors or dropped billing.
func TestSmartWSHandlerFailureDeliveredOnceAndUsageRecordedOnce(t *testing.T) {
	for _, code := range []string{"partial_failure", service.OpenAIPolicyCyber, service.OpenAIPolicyBio} {
		t.Run(code, func(t *testing.T) {
			testSmartWSHandlerFailureDeliveredOnceAndUsageRecordedOnce(t, code)
		})
	}
}

func testSmartWSHandlerFailureDeliveredOnceAndUsageRecordedOnce(t *testing.T, code string) {
	isPolicy := code == service.OpenAIPolicyCyber || code == service.OpenAIPolicyBio
	for _, chat := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, started := range []bool{false, true} {
				for _, paired := range []bool{false, true} {
					t.Run(fmt.Sprintf("chat=%v/stream=%v/started=%v/paired=%v", chat, stream, started, paired), func(t *testing.T) {
						gin.SetMode(gin.TestMode)
						var received atomic.Int32
						failure := fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_partial","model":"gpt-5.1","status":"failed","usage":{"input_tokens":17,"output_tokens":3},"error":{"type":"server_error","code":%q,"message":"local test failure","status":500}}}`, code)
						bare := fmt.Sprintf(`{"type":"error","status":500,"error":{"type":"server_error","code":%q,"message":"local test failure"}}`, code)
						upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							conn, err := upgrader.Upgrade(w, r, nil)
							if err != nil {
								return
							}
							defer conn.Close()
							if _, _, err = conn.ReadMessage(); err != nil {
								return
							}
							received.Add(1)
							if started {
								_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_partial","model":"gpt-5.1","status":"in_progress"}}`))
								_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`))
							}
							if paired {
								_ = conn.WriteMessage(websocket.TextMessage, []byte(bare))
							}
							_ = conn.WriteMessage(websocket.TextMessage, []byte(failure))
							_, _, _ = conn.ReadMessage()
						}))
						t.Cleanup(server.Close)
						cfg := &config.Config{RunMode: config.RunModeSimple}
						cfg.Default.RateMultiplier = 1
						cfg.Gateway.StreamKeepaliveInterval = 1
						cfg.Security.URLAllowlist.AllowInsecureHTTP = true
						cfg.Gateway.OpenAIWS.Enabled = true
						cfg.Gateway.OpenAIWS.APIKeyEnabled = true
						cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
						cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
						cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 2
						cfg.Gateway.OpenAIWS.SmartWSReadTimeoutSeconds = 2
						accounts := []service.Account{{ID: 99901, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
							Status: service.StatusActive, Schedulable: true, Concurrency: 1,
							Credentials: map[string]any{"api_key": "local-test", "base_url": server.URL},
							Extra:       map[string]any{"openai_responses_mode": "force_responses", "openai_apikey_responses_websockets_v2_mode": "ctx_pool"}}}
						repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
						usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
						billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
						t.Cleanup(billing.Stop)
						gateway := service.NewOpenAIGatewayService(repo, usageRepo, nil, nil, nil, nil, nil, cfg, nil, nil,
							service.NewBillingService(cfg, nil), nil, billing, nil, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
						t.Cleanup(gateway.CloseOpenAIWSPool)
						h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
							service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
						group := &service.Group{ID: 99902, Platform: service.PlatformOpenAI, Status: service.StatusActive,
							Hydrated: true, RateMultiplier: 1, SmartWSMode: "enabled"}
						path := "/v1/responses"
						body := fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"input":"hello"}`, stream)
						if chat {
							path = "/v1/chat/completions"
							body = fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"messages":[{"role":"user","content":"hello"}]}`, stream)
						}
						ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), ctxkey.Group, group), 5*time.Second)
						defer cancel()
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
						c.Request.Header.Set("Content-Type", "application/json")
						c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 99903, GroupID: &group.ID, Group: group,
							User: &service.User{ID: 99904, Status: service.StatusActive}})
						c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 99904})
						if chat {
							h.ChatCompletions(c)
						} else {
							h.Responses(c)
						}
						require.EqualValues(t, 1, received.Load(), rec.Body.String())
						require.Contains(t, rec.Body.String(), `"code":"`+code+`"`)
						if stream && !chat && !isPolicy {
							require.Equal(t, http.StatusOK, rec.Code)
							require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
							require.Equal(t, 1, strings.Count(rec.Body.String(), "local test failure"))
							for _, field := range []string{`"input_tokens":17`, `"output_tokens":3`} {
								require.Contains(t, rec.Body.String(), field)
							}
						} else if !started || !stream {
							wantStatus := http.StatusInternalServerError
							if isPolicy {
								wantStatus = http.StatusBadRequest
							}
							require.Equal(t, wantStatus, rec.Code)
							if paired {
								require.JSONEq(t, bare, rec.Body.String())
							} else if isPolicy {
								require.Equal(t, code, gjson.Get(rec.Body.String(), "error.code").String())
								require.JSONEq(t, gjson.Get(failure, "response").Raw, gjson.Get(rec.Body.String(), "response").Raw)
								require.Equal(t, "response.failed", gjson.Get(rec.Body.String(), "type").String())
							} else {
								require.JSONEq(t, failure, rec.Body.String())
							}
						} else {
							require.Equal(t, http.StatusOK, rec.Code)
							require.Equal(t, 1, strings.Count(rec.Body.String(), "local test failure"), rec.Body.String())
							require.NotContains(t, rec.Body.String(), "All accounts")
							if chat && !isPolicy {
								require.Contains(t, rec.Body.String(), `"status_code":500`, "conversion must retain the provider status")
							}
						}
						if isPolicy {
							mark := service.GetOpsCyberPolicy(c)
							require.NotNil(t, mark)
							require.Equal(t, code, mark.Code)
							require.Equal(t, 17, mark.UpstreamInTok)
							require.Equal(t, 3, mark.UpstreamOutTok)
							if chat && stream && started {
								require.Equal(t, 1, strings.Count(rec.Body.String(), "data: [DONE]"))
							}
						}
						select {
						case usage := <-usageRepo.created:
							require.Equal(t, 17, usage.InputTokens)
							require.Equal(t, 3, usage.OutputTokens)
							if isPolicy {
								require.Equal(t, service.RequestTypeCyberBlocked, usage.RequestType)
							}
						case <-time.After(time.Second):
							t.Fatal("partial usage was not recorded")
						}
						require.Empty(t, usageRepo.created, "the same failure must only settle once")
					})
				}
			}
		}
	}
}

func TestSmartWSAccountSlotRetriesRespectAccountBudget(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retries  int
		sequence []bool
		acquired bool
		attempts int
	}{
		{"disabled", 0, []bool{false, true}, false, 1},
		{"recovers", 1, []bool{false, true}, true, 2},
		{"exhausted", 1, []bool{false, false, true}, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := &helperConcurrencyCacheStub{accountSeq: tc.sequence}
			h := &OpenAIGatewayHandler{concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)}
			account := &service.Account{ID: 99123, Extra: map[string]any{"same_account_retry_count": tc.retries}}
			release, acquired, err := h.acquireOpenAIWSAccountSlot(context.Background(), account, 1)
			require.NoError(t, err)
			require.Equal(t, tc.acquired, acquired)
			require.Equal(t, tc.attempts, cache.accountAcquireCalls)
			if release != nil {
				release()
				require.Equal(t, 1, cache.accountReleaseCalls)
			}
		})
	}
}
