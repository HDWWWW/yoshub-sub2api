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
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// Use the public handlers, a real local WebSocket and the normal usage writer:
// a service-only test cannot detect appended handler errors or dropped billing.
func TestSmartWSHandlerFailureDeliveredOnceAndUsageRecordedOnce(t *testing.T) {
	for _, code := range []string{"partial_failure", "cyber_policy"} {
		t.Run(code, func(t *testing.T) {
			testSmartWSHandlerFailureDeliveredOnceAndUsageRecordedOnce(t, code)
		})
	}
}

func testSmartWSHandlerFailureDeliveredOnceAndUsageRecordedOnce(t *testing.T, code string) {
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
						if !started || !stream {
							require.Equal(t, http.StatusInternalServerError, rec.Code)
							if paired {
								require.JSONEq(t, bare, rec.Body.String())
							} else {
								require.JSONEq(t, failure, rec.Body.String())
							}
						} else {
							require.Equal(t, 1, strings.Count(rec.Body.String(), "local test failure"), rec.Body.String())
							require.NotContains(t, rec.Body.String(), "All accounts")
						}
						if code == "cyber_policy" {
							mark := service.GetOpsCyberPolicy(c)
							require.NotNil(t, mark)
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
							if code == "cyber_policy" {
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
