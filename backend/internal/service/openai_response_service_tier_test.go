package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponseServiceTierRewriteContract(t *testing.T) {
	for _, tc := range []struct {
		name, platform, accountType, requested, observed, want string
	}{
		{"oauth UF", PlatformOpenAI, AccountTypeOAuth, "ultrafast", "default", "ultrafast"},
		{"oauth fast", PlatformOpenAI, AccountTypeOAuth, "priority", "default", "priority"},
		{"setup token UF", PlatformOpenAI, AccountTypeSetupToken, "ultrafast", "default", "ultrafast"},
		{"API real downgrade", PlatformOpenAI, AccountTypeAPIKey, "ultrafast", "default", "default"},
		{"OAuth real priority downgrade", PlatformOpenAI, AccountTypeOAuth, "ultrafast", "priority", "priority"},
		{"OAuth real flex downgrade", PlatformOpenAI, AccountTypeOAuth, "ultrafast", "flex", "flex"},
		{"ordinary request", PlatformOpenAI, AccountTypeOAuth, "", "default", "default"},
		{"filtered request", PlatformOpenAI, AccountTypeOAuth, "default", "default", "default"},
		{"other provider", PlatformGrok, AccountTypeOAuth, "ultrafast", "default", "default"},
		{"unknown response", PlatformOpenAI, AccountTypeOAuth, "ultrafast", "unknown", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: tc.platform, Type: tc.accountType}
			for _, eventType := range []string{"", "response.created", "response.completed", "response.failed", "response.incomplete"} {
				body := fmt.Sprintf(`{"id":"resp_1","object":"response","model":"gpt-6.2-sol","service_tier":%q,"usage":{"input_tokens":23},"output":[{"text":"default ultrafast service_tier"}],"sequence_number":9007199254740993}`, tc.observed)
				path := "service_tier"
				if eventType != "" {
					body = fmt.Sprintf(`{"type":%q,"response":%s}`, eventType, body)
					path = "response.service_tier"
				}
				raw := []byte(body)
				got := rewriteOpenAIResponseServiceTier(raw, account, &tc.requested)
				require.Equal(t, tc.want, gjson.GetBytes(got, path).String())
				require.Equal(t, body, string(raw), "raw upstream evidence must not be mutated")
				require.Contains(t, string(got), "9007199254740993")
				require.Contains(t, string(got), "default ultrafast service_tier")
				if tc.observed == tc.want {
					require.Equal(t, body, string(got))
				}
			}
		})
	}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	tier := "ultrafast"
	for _, body := range []string{
		`{"type":"response.output_text.delta","delta":"service_tier default"}`,
		`{"object":"response","service_tier":null}`, `[DONE]`,
		`{"response":{"service_tier":"default"}}trailing`,
		`{"object":"response","model":"gpt-6.2-sol"}`,
	} {
		require.Equal(t, body, string(rewriteOpenAIResponseServiceTier([]byte(body), account, &tier)))
	}
}

func TestResponseServiceTierHTTPForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, shape := range []string{"sse", "json", "sse-to-json"} {
			for _, requested := range []string{"ultrafast", "priority", ""} {
				t.Run(fmt.Sprintf("passthrough=%t/%s/%s", passthrough, shape, requested), func(t *testing.T) {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
					response := `{"id":"resp_tier","object":"response","model":"gpt-6.2-sol","status":"completed","service_tier":"default","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":100,"output_tokens":10}}`
					contentType := "application/json"
					upstreamBody := response
					if shape != "json" {
						contentType = "text/event-stream"
						upstreamBody = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\ndata: [DONE]\n\n"
					}
					upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200,
						Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(upstreamBody))}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector()}
					account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
						Credentials: map[string]any{"access_token": "test", "chatgpt_account_id": "test"},
						Extra:       map[string]any{"openai_passthrough": passthrough, "openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff},
						Status:      StatusActive, Schedulable: true, RateMultiplier: f64p(1)}
					tierField := ""
					if requested != "" {
						tierField = fmt.Sprintf(`,"service_tier":%q`, requested)
					}
					body := []byte(fmt.Sprintf(`{"model":"gpt-6.2-sol","stream":%t,"instructions":"test","input":"hello"%s}`, shape == "sse", tierField))
					result, err := svc.Forward(context.Background(), c, account, body)
					require.NoError(t, err)
					require.Equal(t, requested, optionalStringValue(result.ServiceTier))
					require.Equal(t, "default", result.UpstreamResponseServiceTier)
					want := requested
					if want == "" {
						want = "default"
					}
					wire := rec.Body.Bytes()
					path := "service_tier"
					if shape == "sse" {
						_, wire, _ = extractOpenAISSETerminalEvent(rec.Body.String())
						path = "response.service_tier"
					}
					require.Equal(t, want, gjson.GetBytes(wire, path).String(), rec.Body.String())
					// An API-key downstream must now settle the same tier as King.
					king := ResolveOpenAIServiceTierBilling(account, requested, result.UpstreamResponseServiceTier)
					main := ResolveOpenAIServiceTierBilling(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, requested, want)
					require.Equal(t, king.Billing, main.Billing)
				})
			}
		}
	}
}

func TestResponseServiceTierWSHTTPBridge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := `{"type":"response.completed","response":{"id":"resp_bridge","model":"gpt-6.2-sol","status":"completed","service_tier":"default","usage":{"input_tokens":100,"output_tokens":10}}}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + response + "\n\n"))}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1}
	body := []byte(`{"type":"response.create","model":"gpt-6.2-sol","service_tier":"ultrafast","input":"hello"}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	var wire []byte
	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "test", body, len(body),
		"gpt-6.2-sol", "", "", "", "", 1, func(message []byte) error { wire = append([]byte(nil), message...); return nil })
	require.NoError(t, err)
	require.Equal(t, "default", result.UpstreamResponseServiceTier)
	require.Equal(t, "ultrafast", gjson.GetBytes(wire, "response.service_tier").String())
}

func TestResponseServiceTierNativeWebSocketTurns(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) { testResponseServiceTierNativeWebSocketTurns(t, mode) })
	}
}

type serviceTierWSConn struct{ *stagedPassthroughConn }

func (c *serviceTierWSConn) WriteJSON(ctx context.Context, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, body)
}

func testResponseServiceTierNativeWebSocketTurns(t *testing.T, mode string) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	upstream := newStagedPassthroughConn()
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	account := passthroughLifecycleAccount()
	account.Type = AccountTypeOAuth
	account.Credentials = map[string]any{"access_token": "test", "chatgpt_account_id": "test"}
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
	svc := newPassthroughLifecycleService(cfg, upstream)
	if mode == OpenAIWSIngressModeCtxPool {
		cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
		cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
		pool := newOpenAIWSConnPool(cfg)
		pool.setClientDialerForTest(&stagedPassthroughDialer{conn: &serviceTierWSConn{upstream}})
		svc.openaiWSPool = pool
		defer pool.Close()
	}
	results := make(chan *OpenAIForwardResult, 4)
	server, serverErr := startPassthroughHookRecordingServer(t, ctx, svc, account,
		&OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) {
			if err == nil && result != nil {
				results <- result
			}
		}})
	defer server.Close()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	for i, tier := range []string{"ultrafast", "", "priority"} {
		field := ""
		if tier != "" {
			field = fmt.Sprintf(`,"service_tier":%q`, tier)
		}
		body := []byte(`{"type":"response.create","model":"gpt-6.2-sol","input":"hello"` + field + `}`)
		turnCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		require.NoError(t, client.Write(turnCtx, coderws.MessageText, body))
		forwarded := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
		require.Equal(t, tier, gjson.GetBytes(forwarded, "service_tier").String())
		upstream.Send(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","model":"gpt-6.2-sol","status":"completed","service_tier":"default","usage":{"input_tokens":100,"output_tokens":10}}}`, i))
		_, wire, err := client.Read(turnCtx)
		stop()
		require.NoError(t, err)
		want := tier
		if want == "" {
			want = "default"
		}
		require.Equal(t, want, gjson.GetBytes(wire, "response.service_tier").String())
		select {
		case result := <-results:
			require.Equal(t, tier, optionalStringValue(result.ServiceTier))
			require.Equal(t, "default", result.UpstreamResponseServiceTier)
		case <-time.After(3 * time.Second):
			t.Fatal("missing usage result")
		}
	}
	client.CloseNow()
	select {
	case <-serverErr:
	case <-time.After(5 * time.Second):
		t.Fatal("websocket did not finish")
	}
}
