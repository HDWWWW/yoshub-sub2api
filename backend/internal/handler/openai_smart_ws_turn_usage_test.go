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

// With no usage worker pool the recorder runs inline. Exercise the actual
// submission boundary, not just the tokens returned by the forwarder.
func TestSmartWSUsageRecorderDoesNotSubmitPreviousTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const old = `{"type":"response.completed","response":{"id":"resp_old","status":"completed","usage":{"input_tokens":11,"output_tokens":5}}}`
	const late = `{"type":"keepalive","response_id":"resp_old","usage":{"input_tokens":11,"output_tokens":5}}`
	const done = `{"type":"response.completed","response":{"id":"resp_new","status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`
	const reject = `{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"local quota"}}`
	for _, tc := range []struct {
		name    string
		frames  []string
		success bool
	}{
		{"success", []string{late, done}, true}, {"quota", []string{late, reject}, false}, {"disconnect", []string{late}, false}, {"error_tail", []string{reject, late}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sockets, requests, httpCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !websocket.IsWebSocketUpgrade(r) {
					httpCalls.Add(1)
					w.WriteHeader(503)
					return
				}
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer ws.Close()
				sockets.Add(1)
				for turn := 0; turn < 2; turn++ {
					if _, _, err := ws.ReadMessage(); err != nil {
						return
					}
					requests.Add(1)
					frames := tc.frames
					if turn == 0 {
						frames = []string{old}
					}
					for _, f := range frames {
						if ws.WriteMessage(websocket.TextMessage, []byte(f)) != nil {
							return
						}
					}
				}
			}))
			defer srv.Close()
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.Enabled, cfg.Gateway.OpenAIWS.APIKeyEnabled, cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2, cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true, true, true, true
			cfg.Gateway.OpenAIWS.SmartWSReadTimeoutSeconds = 2
			a := service.Account{ID: 991, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "test", "base_url": srv.URL}, Extra: map[string]any{"openai_responses_mode": "force_responses", "openai_apikey_responses_websockets_v2_mode": "ctx_pool", "same_account_retry_count": 0}}
			sink := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 8)}
			bill := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			defer bill.Stop()
			gw := service.NewOpenAIGatewayService(&openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{a}}, sink, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), nil, bill, nil, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
			defer gw.CloseOpenAIWSPool()
			h := NewOpenAIGatewayHandler(gw, service.NewConcurrencyService(nil), bill, service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			group := &service.Group{ID: 992, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, RateMultiplier: 1, SmartWSMode: "enabled"}
			root, cancel := context.WithTimeout(context.WithValue(context.Background(), ctxkey.Group, group), 8*time.Second)
			defer cancel()
			for turn := 0; turn < 2; turn++ {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				ctx := context.WithValue(root, ctxkey.RequestID, fmt.Sprintf("usage-test-%d", turn))
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"gpt-5.1","stream":false,"prompt_cache_key":"test-usage","input":"turn %d"}`, turn))).WithContext(ctx)
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("session_id", "test-usage")
				c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 993, GroupID: &group.ID, Group: group, User: &service.User{ID: 994, Status: service.StatusActive}})
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 994})
				h.Responses(c)
				if turn == 0 || tc.success {
					require.Equal(t, 200, rec.Code)
					require.Len(t, sink.created, 1)
					row := <-sink.created
					if turn == 0 {
						require.Equal(t, 11, row.InputTokens)
						require.Equal(t, 5, row.OutputTokens)
					} else {
						require.Equal(t, 3, row.InputTokens)
						require.Equal(t, 2, row.OutputTokens)
					}
				} else {
					require.GreaterOrEqual(t, rec.Code, 400)
					require.Empty(t, sink.created, "a failed turn must not submit the first turn's usage again")
				}
			}
			require.EqualValues(t, 1, sockets.Load())
			require.EqualValues(t, 2, requests.Load())
			require.Zero(t, httpCalls.Load())
		})
	}
}
