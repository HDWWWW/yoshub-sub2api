package repository

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const auditCompleted = `{"type":"response.completed","response":{"id":"resp_audit_redirect","status":"completed","model":"gpt-5.1","output":[],"usage":{"input_tokens":17,"output_tokens":3}}}`

func auditTransportService(t *testing.T, upstreamURL string) (*service.OpenAIGatewayService, *service.Account) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.AllowPrivateHosts = true
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 2
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 2
	upstream := NewHTTPUpstream(cfg)
	svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
		nil, nil, nil, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	t.Cleanup(svc.CloseOpenAIWSPool)
	account := &service.Account{ID: 71, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"base_url": upstreamURL, "api_key": "synthetic-audit-only"},
		Extra:       map[string]any{"openai_apikey_responses_websockets_v2_mode": "ctx_pool", "openai_responses_mode": "force_responses"}}
	return svc, account
}

func auditTransportContext(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx := context.WithValue(t.Context(), ctxkey.Group, &service.Group{ID: 7, Platform: service.PlatformOpenAI, Status: "active", Hydrated: true, SmartWSMode: "enabled"})
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	c.Set("api_key", &service.APIKey{ID: 73})
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportHTTP)
	return c, rec
}

func TestSmartWSHTTPTransportRedirectDoesNotReplay(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			name := "http_fallback"
			if native {
				name = "native_bridge"
			}
			t.Run(name+"/"+http.StatusText(status), func(t *testing.T) {
				var sourceCalls, targetCalls, handshakes atomic.Int64
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targetCalls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+auditCompleted+"\n\n")
				}))
				defer target.Close()
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						handshakes.Add(1)
						w.WriteHeader(http.StatusUpgradeRequired)
						return
					}
					sourceCalls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Location", target.URL+"/v1/responses")
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"code":"audit_redirect","message":"request already reached upstream"}}`)
				}))
				defer source.Close()
				svc, account := auditTransportService(t, source.URL)
				body := `{"model":"gpt-5.1","stream":true,"input":"synthetic audit"}`
				if !native {
					c, rec := auditTransportContext(t, body)
					result, err := svc.Forward(c.Request.Context(), c, account, []byte(body))
					t.Logf("source_posts=%d redirected_posts=%d handshakes=%d downstream_status=%d result_present=%t error=%v", sourceCalls.Load(), targetCalls.Load(), handshakes.Load(), rec.Code, result != nil, err)
					require.Error(t, err, "a redirect must be reported without dispatching the request again")
				} else {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					finished := make(chan error, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, err := coderws.Accept(w, r, nil)
						if err != nil {
							finished <- err
							return
						}
						defer conn.CloseNow()
						_, first, err := conn.Read(ctx)
						if err != nil {
							finished <- err
							return
						}
						c, _ := auditTransportContext(t, body)
						c.Request = r.WithContext(ctx)
						finished <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "synthetic-audit-only", first, nil)
					}))
					defer server.Close()
					client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
					require.NoError(t, err)
					defer client.CloseNow()
					require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","input":"synthetic audit"}`)))
					_, frame, readErr := client.Read(ctx)
					var event map[string]any
					_ = json.Unmarshal(frame, &event)
					t.Logf("source_posts=%d redirected_posts=%d handshakes=%d first_event=%v read_error=%v", sourceCalls.Load(), targetCalls.Load(), handshakes.Load(), event["type"], readErr)
					_ = client.CloseNow()
					cancel()
					select {
					case <-finished:
					case <-time.After(time.Second):
						t.Fatal("native bridge did not stop")
					}
					require.Equal(t, "error", event["type"])
				}
				require.EqualValues(t, 1, sourceCalls.Load())
				require.Zero(t, targetCalls.Load(), "following a redirect dispatches the same generation a second time")
			})
		}
	}
}

func TestSmartWSHTTPFallbackFailureBillsObservedUsageOnce(t *testing.T) {
	var dispatches atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(426)
			return
		}
		dispatches.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","response":{"id":"resp_failed_billed","status":"failed","model":"gpt-5.1","usage":{"input_tokens":17,"output_tokens":3},"error":{"code":"audit_declined","message":"preserve explicit failure"}}}`+"\n\n")
	}))
	defer upstream.Close()
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.AllowPrivateHosts, cfg.Security.URLAllowlist.AllowInsecureHTTP = true, true
	cfg.Gateway.OpenAIWS.Enabled, cfg.Gateway.OpenAIWS.APIKeyEnabled = true, true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2, cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true, true
	usage, billing := &lifecycleUsageLogRepo{}, &lifecycleBillingRepo{}
	svc := service.NewOpenAIGatewayService(nil, usage, billing, nil, nil, nil, nil, cfg, nil, nil,
		service.NewBillingService(cfg, nil), nil, nil, NewHTTPUpstream(cfg), &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	defer svc.CloseOpenAIWSPool()
	account := &service.Account{ID: 71, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"base_url": upstream.URL, "api_key": "synthetic-only"},
		Extra:       map[string]any{"openai_apikey_responses_websockets_v2_mode": "ctx_pool", "openai_responses_mode": "force_responses"}}
	body := `{"model":"gpt-5.1","stream":true,"input":"synthetic usage check"}`
	c, rec := auditTransportContext(t, body)
	result, err := svc.Forward(c.Request.Context(), c, account, []byte(body))
	require.Error(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "audit_declined")
	require.NotContains(t, rec.Body.String(), "smart_ws_dispatch_uncertain")
	require.False(t, result.OpenAIWSMode)
	require.NoError(t, svc.RecordUsage(c.Request.Context(), &service.OpenAIRecordUsageInput{
		Result: result, APIKey: &service.APIKey{ID: 73}, User: &service.User{ID: 3}, Account: account}))
	require.EqualValues(t, 1, dispatches.Load())
	require.Len(t, billing.commands, 1)
	require.Len(t, usage.logs, 1)
	require.Equal(t, 17, usage.logs[0].InputTokens)
	require.Equal(t, 3, usage.logs[0].OutputTokens)
	require.Greater(t, usage.logs[0].ActualCost, 0.0)
}
