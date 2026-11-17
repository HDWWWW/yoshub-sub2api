package service

import (
	"bytes"
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

func TestOpenAICodexSystemRoleCompatibilityPreservesRequest(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		for _, previousResponseID := range []string{"", "resp_previous"} {
			t.Run(accountType+"/"+previousResponseID, func(t *testing.T) {
				body := []byte(`{"type":"response.create","model":"gpt-5.1","instructions":"keep existing instructions","marker":"\u0061","number":900719925474099312345,"previous_response_id":"` + previousResponseID + `","input":[` +
					`{"type":"message","id":"msg_user","role":"user","content":[{"type":"input_text","text":"before system"}]},` +
					`{"type":"message","id":"msg_system","role":"system","content":[{"type":"input_text","text":"system text"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}],"future":{"number":900719925474099312345}},` +
					`{"role":"system","content":"second system"},` +
					`{"type":"message","id":"msg_dev","role":"developer","content":"existing developer"},` +
					`{"type":"reasoning","id":"rs_history","summary":[],"encrypted_content":"opaque-encrypted-history"},` +
					`{"type":"function_call","id":"fc_history","call_id":"call_history","name":"check","arguments":"{\"number\":900719925474099312345}"},` +
					`{"type":"function_call_output","call_id":"call_history","output":"done"},` +
					`{"type":"future_item","role":"system","content":"not a message"}]}`)
				original := bytes.Clone(body)
				account := &Account{Platform: PlatformOpenAI, Type: accountType}
				normalized, changed, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, account, openAIResponsesCompatibilityOptions{})
				require.NoError(t, err)
				require.True(t, changed)
				want := strings.Replace(string(body), `"role":"system"`, `"role":"developer"`, 2)
				require.Equal(t, want, string(normalized), "only message roles should change, including a system message after user input")
				require.Equal(t, original, body, "do not mutate the caller's buffer")
				again, changed, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(normalized, account, openAIResponsesCompatibilityOptions{})
				require.NoError(t, err)
				require.False(t, changed)
				require.Equal(t, normalized, again)
			})
		}
	}
}

func TestOpenAICodexSystemRoleCompatibilityAccountAndMessageGuards(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","input":[{"role":"system","content":"rules"}]}`)
	for _, tt := range []struct {
		name    string
		account *Account
		want    bool
	}{
		{"oauth", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, true},
		{"setup_token", &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}, true},
		{"agent_identity", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity}}, true},
		{"api_key", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, false},
		{"anthropic_oauth", &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, false},
		{"grok_setup_token", &Account{Platform: PlatformGrok, Type: AccountTypeSetupToken}, false},
		{"implicit_platform", &Account{Type: AccountTypeOAuth}, false},
		{"nil", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			normalized, changed, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, tt.account, openAIResponsesCompatibilityOptions{})
			require.NoError(t, err)
			require.Equal(t, tt.want, changed)
			if tt.want {
				require.Equal(t, "developer", gjson.GetBytes(normalized, "input.0.role").String())
				require.False(t, gjson.GetBytes(normalized, "instructions").Exists(), "do not synthesize instructions")
			} else {
				require.Equal(t, body, normalized)
			}
		})
	}
	for _, input := range []string{
		`"plain input"`, `null`, `[]`, `{"role":"system","content":"not an input array"}`,
		`[{"type":"future_item","role":"system"},{"type":null,"role":"system"},{"type":"","role":"system"},{"type":"message","role":"System"},{"role":42},{"role":"user","content":"system"},null]`,
	} {
		t.Run(input, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.1","input":` + input + `}`)
			normalized, changed, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, openAIResponsesCompatibilityOptions{})
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, body, normalized)
		})
	}
}

func TestOpenAICodexSystemRoleJSONModePreservesInputInstruction(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","instructions":null,"text":{"format":{"type":"json_object"}},"input":[{"role":"system","content":"Return only valid JSON."},{"role":"user","content":"Give one object."}]}`)
	normalized, changed, err := normalizeOpenAIResponsesCompatibilityBodyWithOptions(body, &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, openAIResponsesCompatibilityOptions{})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, strings.Replace(string(body), `"role":"system"`, `"role":"developer"`, 1), string(normalized), "JSON mode must retain its instruction in input and must not fill instructions")
}

func TestOpenAICodexSystemRoleHTTPBridgePreservesContinuationAndStops400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey} {
		for _, turn := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s/turn%d", accountType, turn), func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"invalid_parameter","message":"another field rejected"}}`)),
				}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 4901, Platform: PlatformOpenAI, Type: accountType, Concurrency: 1}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
				payload := []byte(`{"type":"response.create","generate":true,"model":"gpt-5.1","instructions":"keep instructions","previous_response_id":"resp_prev","sequence":900719925474099312345,"input":[{"role":"user","content":"before"},{"type":"message","id":"msg_system","role":"system","content":[{"type":"input_text","text":"rules"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]},{"type":"reasoning","id":"rs_prev","summary":[],"encrypted_content":"opaque"}]}`)
				var writes [][]byte
				result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "test-token", payload, len(payload), "gpt-5.1", "", "", "", "", turn, func(message []byte) error {
					writes = append(writes, bytes.Clone(message))
					return nil
				})
				require.Nil(t, result)
				requireStoppedSmartWSFailure(t, err)
				require.Len(t, upstream.requests, 1, "a 400 after normalization must never trigger a replay")
				wantRole := "developer"
				if accountType == AccountTypeAPIKey {
					wantRole = "system"
				}
				require.Equal(t, wantRole, gjson.GetBytes(upstream.lastBody, "input.1.role").String())
				for _, path := range []string{"instructions", "previous_response_id", "sequence", "input.0", "input.1.id", "input.1.content", "input.2"} {
					require.JSONEq(t, gjson.GetBytes(payload, path).Raw, gjson.GetBytes(upstream.lastBody, path).Raw, path)
				}
				require.Equal(t, "900719925474099312345", gjson.GetBytes(upstream.lastBody, "sequence").Raw)
				require.Len(t, writes, 1)
				require.EqualValues(t, http.StatusBadRequest, gjson.GetBytes(writes[0], "status").Int())
				require.Equal(t, "invalid_parameter", gjson.GetBytes(writes[0], "error.code").String())
			})
		}
	}
}

type codexSystemRoleStagedConn struct{ *stagedPassthroughConn }

func (c *codexSystemRoleStagedConn) WriteJSON(ctx context.Context, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.WriteFrame(ctx, coderws.MessageText, payload)
}

func TestOpenAICodexSystemRoleWebSocketFirstAndSubsequentTurns(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			controlCtx, cancelControl := context.WithCancelCause(context.Background())
			defer cancelControl(context.Canceled)
			upstream := newStagedPassthroughConn()
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
			svc := newPassthroughLifecycleService(cfg, upstream)
			if mode == OpenAIWSIngressModeCtxPool {
				pool := newOpenAIWSConnPool(cfg)
				defer pool.Close()
				pool.setClientDialerForTest(&openAIWSSingleConnDialer{conn: &codexSystemRoleStagedConn{upstream}})
				svc.openaiWSPool = pool
			}
			account := passthroughLifecycleAccount()
			account.Type = AccountTypeOAuth
			account.Credentials = map[string]any{"access_token": "test-token"}
			account.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": mode}
			server, serverErr := startPassthroughLifecycleServer(t, controlCtx, svc, account)
			defer server.Close()
			firstPayload := `{"type":"response.create","model":"gpt-5.1","instructions":"keep instructions","input":[{"role":"user","content":"before"},{"type":"message","id":"msg_first","role":"system","content":"first rules"}]}`
			client := dialPassthroughLifecycleClientWithPayload(t, server, firstPayload)
			defer func() { _ = client.CloseNow() }()
			first := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
			require.Equal(t, "developer", gjson.GetBytes(first, "input.1.role").String())
			require.Equal(t, "msg_first", gjson.GetBytes(first, "input.1.id").String())
			require.Equal(t, "keep instructions", gjson.GetBytes(first, "instructions").String())
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
			completed, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(completed, "type").String())
			secondPayload := []byte(`{"type":"response.create","model":"gpt-5.1","instructions":"keep next instructions","previous_response_id":"resp_first","input":[{"role":"user","content":"next"},{"role":"system","content":"second rules"}]}`)
			writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
			err = client.Write(writeCtx, coderws.MessageText, secondPayload)
			cancelWrite()
			require.NoError(t, err)
			second := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
			require.Equal(t, "developer", gjson.GetBytes(second, "input.1.role").String())
			require.Equal(t, "second rules", gjson.GetBytes(second, "input.1.content").String())
			require.Equal(t, "resp_first", gjson.GetBytes(second, "previous_response_id").String())
			require.Equal(t, "keep next instructions", gjson.GetBytes(second, "instructions").String())
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_second","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
			completed, err = readPassthroughLifecycleFrame(t, client, 3*time.Second)
			require.NoError(t, err)
			require.Equal(t, "resp_second", gjson.GetBytes(completed, "response.id").String())
			require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
			select {
			case <-serverErr:
			case <-time.After(3 * time.Second):
				t.Fatal("websocket session did not exit")
			}
			select {
			case replay := <-upstream.writes:
				t.Fatalf("unexpected replay: %s", replay)
			default:
			}
		})
	}
}

func TestOpenAICodexSystemRoleNativeLargeBridgeStops400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			controlCtx, cancelControl := context.WithCancelCause(context.Background())
			defer cancelControl(context.Canceled)
			wsUpstream := newStagedPassthroughConn()
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
			// Exercise the same branch as an oversized production frame with a small fixture.
			cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
			svc := newPassthroughLifecycleService(cfg, wsUpstream)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{
				{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","code":"invalid_parameter","message":"another field rejected"}}`))},
				{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unexpected second dispatch"}}`))},
			}}
			svc.httpUpstream = upstream
			account := passthroughLifecycleAccount()
			account.Type = AccountTypeOAuth
			account.Credentials = map[string]any{"access_token": "test-token"}
			account.Extra = map[string]any{"openai_oauth_responses_websockets_v2_mode": mode}
			server, serverErr := startPassthroughLifecycleServer(t, controlCtx, svc, account)
			defer server.Close()
			payload := `{"type":"response.create","model":"gpt-5.1","instructions":"keep instructions","sequence":900719925474099312345,"input":[{"role":"user","content":"before"},{"role":"system","content":"system rules"}]}`
			client := dialPassthroughLifecycleClientWithPayload(t, server, payload)
			defer func() { _ = client.CloseNow() }()
			failure, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
			require.NoError(t, err)
			require.Equal(t, "invalid_parameter", gjson.GetBytes(failure, "error.code").String())
			require.EqualValues(t, http.StatusBadRequest, gjson.GetBytes(failure, "status").Int())
			select {
			case err := <-serverErr:
				requireStoppedSmartWSFailure(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("bridge did not stop after a 400")
			}
			require.Len(t, upstream.requests, 1)
			require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "input.1.role").String())
			require.Equal(t, "system rules", gjson.GetBytes(upstream.lastBody, "input.1.content").String())
			require.Equal(t, "keep instructions", gjson.GetBytes(upstream.lastBody, "instructions").String())
			require.Equal(t, "900719925474099312345", gjson.GetBytes(upstream.lastBody, "sequence").Raw)
			select {
			case request := <-wsUpstream.writes:
				t.Fatalf("large frame unexpectedly used WS: %s", request)
			default:
			}
		})
	}
}
