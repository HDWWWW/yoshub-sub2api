//go:build unit

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

func TestResponsesLiteIngress_HTTPMetadataAndWSBridgeHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, transport := range []string{"http-managed", "http-passthrough", "ws-http"} {
		for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
			for _, parallel := range []string{"", `,"parallel_tool_calls":true`, `,"parallel_tool_calls":false`} {
				t.Run(transport+"/"+accountType+"/"+parallel, func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
					marker := `,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}`
					if transport == "ws-http" {
						c.Request.Header.Set(responsesLiteHeader, "true")
						marker = ""
					}
					body := []byte(`{"model":"gpt-6.1-sol","stream":true,"instructions":"test","input":"hi"` + marker + parallel + `}`)
					original := string(body)
					upstream := &httpUpstreamRecorder{resp: &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_lite\",\"model\":\"gpt-6.1-sol\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
					}}
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
					account := &Account{ID: 601, Platform: PlatformOpenAI, Type: accountType,
						Concurrency: 1, Status: StatusActive, Schedulable: true, RateMultiplier: f64p(1),
						Credentials: map[string]any{"api_key": "sk-test", "access_token": "oauth-token", "chatgpt_account_id": "test-account"},
						Extra:       map[string]any{"openai_passthrough": transport == "http-passthrough"},
					}
					if transport == "ws-http" {
						_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "test-token", body, len(body), "gpt-6.1-sol", "", "", "", "", 1, func([]byte) error { return nil })
						require.NoError(t, err)
					} else {
						_, err := svc.Forward(context.Background(), c, account, body)
						require.NoError(t, err)
					}
					require.NotNil(t, upstream.lastReq)
					require.Equal(t, gjson.False, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Type, string(upstream.lastBody))
					if transport == "ws-http" {
						require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
					}
					require.Equal(t, original, string(body), "normalization must not mutate the retry source")
				})
			}
		}
	}
}

func TestResponsesLiteIngress_WSHandshakeAppliesToEveryTurn(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModePassthrough, OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			testResponsesLiteHandshakeTurns(t, mode)
		})
	}
}

type liteIngressPoolConn struct{ *stagedPassthroughConn }

func (c *liteIngressPoolConn) WriteJSON(ctx context.Context, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, body)
}

func testResponsesLiteHandshakeTurns(t *testing.T, mode string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	controlCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := newStagedPassthroughConn()
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	svc := newPassthroughLifecycleService(cfg, upstream)
	account := passthroughLifecycleAccount()
	account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
	if mode == OpenAIWSIngressModeCtxPool {
		svc.openaiWSPool = newOpenAIWSConnPool(cfg)
		svc.openaiWSPool.setClientDialerForTest(&stagedPassthroughDialer{conn: &liteIngressPoolConn{upstream}})
		defer svc.openaiWSPool.Close()
	}
	server, serverErr := startPassthroughLifecycleServer(t, controlCtx, svc, account)
	defer server.Close()
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	conn, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: http.Header{responsesLiteHeader: []string{"true"}}})
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()
	for turn := 0; turn < 2; turn++ {
		require.NoError(t, conn.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":"hi","parallel_tool_calls":true}`)))
		body := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
		require.Equal(t, gjson.False, gjson.GetBytes(body, "parallel_tool_calls").Type, string(body))
		require.True(t, isOpenAIResponsesLiteWebSocketPayload(body), "project the handshake marker onto upstream frames")
		upstream.Send(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_lite_%d","model":"gpt-6.1-sol","usage":{"input_tokens":1,"output_tokens":1}}}`, turn))
		_, err = readPassthroughLifecycleFrame(t, conn, 3*time.Second)
		require.NoError(t, err)
	}
	require.NoError(t, conn.Close(coderws.StatusNormalClosure, "done"))
	select {
	case <-serverErr:
	case <-ctx.Done():
		t.Fatal("Lite websocket relay did not finish")
	}
}

func TestResponsesLiteIngress_FrameMarkerDoesNotEnableLaterOrdinaryRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	account := passthroughLifecycleAccount()
	lite := []byte(`{"type":"response.create","parallel_tool_calls":true,"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	normalized, changed, err := normalizeOpenAIResponsesLiteWebSocketRequest(c, lite, account)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, gjson.False, gjson.GetBytes(normalized, "parallel_tool_calls").Type)
	for _, header := range []string{"", "false"} {
		c.Request.Header.Set(responsesLiteHeader, header)
		body := []byte(`{"type":"response.create","parallel_tool_calls":true,"tools":[{"type":"function","name":"lookup"}],"client_metadata":{"keep":"value"}}`)
		normalized, changed, err = normalizeOpenAIResponsesLiteWebSocketRequest(c, body, account)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, normalized)
	}
	c.Request.Header.Set(responsesLiteHeader, "true")
	body := []byte(`{"type":"response.create","parallel_tool_calls":true,"sequence":900719925474099312345,"client_metadata":{"keep":"value"}}`)
	normalized, changed, err = normalizeOpenAIResponsesLiteWebSocketRequest(c, body, account)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "900719925474099312345", gjson.GetBytes(normalized, "sequence").Raw)
	require.Equal(t, "value", gjson.GetBytes(normalized, "client_metadata.keep").String())
	require.True(t, isOpenAIResponsesLiteWebSocketPayload(normalized))
	require.Equal(t, gjson.False, gjson.GetBytes(normalized, "parallel_tool_calls").Type)
	for _, metadata := range []string{`"opaque"`, `[]`, `42`} {
		invalid := []byte(`{"type":"response.create","client_metadata":` + metadata + `}`)
		out, changed, err := normalizeOpenAIResponsesLiteWebSocketRequest(c, invalid, account)
		require.ErrorContains(t, err, "client_metadata to be an object")
		require.False(t, changed)
		require.Equal(t, invalid, out)
	}
	account.Platform = PlatformGrok
	normalized, changed, err = normalizeOpenAIResponsesLiteWebSocketRequest(c, body, account)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, normalized)
}
