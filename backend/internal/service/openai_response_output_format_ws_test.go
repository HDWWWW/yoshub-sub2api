package service

import (
	"context"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponseOutputFormatWebSocketLaterTurnValidation(t *testing.T) {
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
			first := `{"type":"response.create","model":"gpt-5.1","text":{"format":{"type":"json_object"}},"input":[{"role":"user","content":"hello"}]}`
			client := dialPassthroughLifecycleClientWithPayload(t, server, first)
			defer func() { _ = client.CloseNow() }()
			forwarded := requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
			require.Equal(t, "developer", gjson.GetBytes(forwarded, "input.0.role").Str)
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`)
			completed, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(completed, "type").Str)
			second := []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_first","input":[{"role":"user","content":"next"}],"text":{"format":{"type":"json_schema","strict":true,"schema":{"type":"object","properties":{"a":{"type":"string"}},"required":[],"additionalProperties":false}}}}`)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			require.NoError(t, client.Write(ctx, coderws.MessageText, second))
			// Read the close frame so the relay's close handshake can finish.
			_, _, readErr := client.Read(ctx)
			require.Error(t, readErr)
			select {
			case err := <-serverErr:
				var validation *openAIOutputFormatValidationError
				require.ErrorAs(t, err, &validation)
				require.Equal(t, "text.format.schema.required", validation.param)
				var closeErr *OpenAIWSClientCloseError
				require.ErrorAs(t, err, &closeErr)
				require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
			case <-ctx.Done():
				t.Fatal("invalid later turn did not terminate")
			}
			select {
			case payload := <-upstream.writes:
				t.Fatalf("invalid turn reached upstream: %s", payload)
			default:
			}
		})
	}
}
