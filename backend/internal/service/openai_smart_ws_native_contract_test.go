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

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Exercise the public native boundary and read every delivered frame, including
// the final error. The same client connection is retained between turns.
func nativeContractFrames(t *testing.T, svc *OpenAIGatewayService, account *Account, payloads ...string) ([][]byte, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	finished := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
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
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.WithContext(ctx)
		c.Set("api_key", &APIKey{ID: 73})
		finished <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "test-token", first, nil)
	}))
	defer server.Close()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(payloads[0])))
	var frames [][]byte
	turn := 1
	for {
		_, frame, readErr := client.Read(ctx)
		if readErr != nil {
			break
		}
		frames = append(frames, frame)
		if gjson.GetBytes(frame, "type").String() == "response.completed" && turn < len(payloads) {
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(payloads[turn])))
			turn++
		}
	}
	select {
	case err := <-finished:
		return frames, err
	case <-ctx.Done():
		t.Fatal("native contract deadline")
		return frames, ctx.Err()
	}
}
func reauditNativeRun(t *testing.T, svc *OpenAIGatewayService, account *Account, payload string) error {
	t.Helper()
	frames, err := nativeContractFrames(t, svc, account, payload)
	require.NotEmpty(t, frames, "native failure must reach the client")
	require.Equal(t, "error", gjson.GetBytes(frames[len(frames)-1], "type").String())
	return err
}

func TestSmartWSNativeContractNativeHandshakePreservesTerminalError(t *testing.T) {
	for _, test := range []struct {
		mode, credential string
		status           int
	}{
		{OpenAIWSIngressModeCtxPool, AccountTypeAPIKey, 429}, {OpenAIWSIngressModePassthrough, AccountTypeAPIKey, 429},
		{OpenAIWSIngressModeCtxPool, AccountTypeOAuth, 429}, {OpenAIWSIngressModePassthrough, AccountTypeOAuth, 429},
		{OpenAIWSIngressModeCtxPool, AccountTypeAPIKey, 403}, {OpenAIWSIngressModePassthrough, AccountTypeAPIKey, 403},
		{OpenAIWSIngressModeCtxPool, AccountTypeOAuth, 403}, {OpenAIWSIngressModePassthrough, AccountTypeOAuth, 403},
	} {
		t.Run(test.credential+"/"+test.mode, func(t *testing.T) {
			mode := test.mode
			const body = `{"error":{"code":"audit_explicit_rejection","message":"audit rejection; no generation accepted"}}`
			svc, account, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, body)
			})
			// Force both credential types to the local synthetic upstream. No
			// fixture credential may be sent to an official upstream endpoint.
			localURL := "ws" + strings.TrimPrefix(account.GetOpenAIBaseURL(), "http") + "/v1/responses"
			account.Type = test.credential
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
			svc.openaiWSPassthroughDialer = forcedWSTestDialer{url: localURL, delegate: newDefaultOpenAIWSClientDialer()}
			err := reauditNativeRun(t, svc, account, `{"type":"response.create","model":"gpt-5.1","input":"audit"}`)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			t.Logf("credential=%s mode=%s status=%d same_account_retry=%t next_account_retry=%t body_bytes=%d", test.credential, mode, failover.StatusCode, failover.RetryableOnSameAccount, failover.ShouldRetryNextAccount(), len(failover.ResponseBody))
			assert.False(t, failover.ShouldRetryNextAccount(), "an explicit upstream rejection must be delivered without hidden account retry")
			assert.Equal(t, test.status == 429, failover.ShouldReportAccountScheduleFailure())
			assert.False(t, failover.RetryableOnSameAccount)
			assert.JSONEq(t, body, string(failover.ResponseBody), "the explicit upstream error must be retained")
		})
	}
}

func TestSmartWSNativeContractNativeLargeRequestBridgeNeverReplaysAfterDispatch(t *testing.T) {
	svc, account, _ := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("bridge unexpectedly dialed WS") })
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	// Same selection branch as a production request above 15 MiB; small fixture.
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	upstream := &httpUpstreamRecorder{err: io.ErrUnexpectedEOF}
	svc.httpUpstream = upstream
	err := reauditNativeRun(t, svc, account, `{"type":"response.create","model":"gpt-5.1","input":"accepted then disconnected"}`)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1, "fixture must reach the HTTP upstream dispatch boundary")
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) {
		t.Logf("upstream_dispatches=%d next_account_retry=%t same_account_retry=%t", len(upstream.requests), failover.ShouldRetryNextAccount(), failover.RetryableOnSameAccount)
		assert.False(t, failover.ShouldRetryNextAccount(), "EOF after upstream dispatch must never be classified as safe account failover")
		assert.False(t, failover.RetryableOnSameAccount)
	}
}

func TestSmartWSNativeContractNativeBridgePreservesRejectedEncryptedReasoning(t *testing.T) {
	svc, account, _ := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("bridge unexpectedly dialed WS") })
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_encrypted_content","message":"The encrypted content could not be verified"}}`))},
		{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"server_error","message":"audit sentinel: second dispatch must not occur"}}`))},
	}}
	svc.httpUpstream = upstream
	err := reauditNativeRun(t, svc, account, `{"type":"response.create","model":"gpt-5.1","input":[{"type":"reasoning","encrypted_content":"audit-preserve-cipher"},{"role":"user","content":"audit"}]}`)
	require.Error(t, err)
	t.Logf("upstream_dispatches=%d", len(upstream.requests))
	assert.Len(t, upstream.requests, 1, "upstream rejection must not trigger stripping encrypted reasoning and resending")
	if len(upstream.bodies) > 1 {
		t.Logf("second_request_retains_encrypted_content=%t", strings.Contains(string(upstream.bodies[1]), "audit-preserve-cipher"))
	}
}

func TestSmartWSNativeContractNativeBridgeDoesNotResendAfterExplicitFieldError(t *testing.T) {
	svc, account, _ := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("bridge unexpectedly dialed WS") })
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_parameter","param":"prompt_cache_breakpoint","message":"model does not support prompt_cache_breakpoint"}}`))},
		{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"server_error","message":"audit sentinel: second dispatch must not occur"}}`))},
	}}
	svc.httpUpstream = upstream
	err := reauditNativeRun(t, svc, account, `{"type":"response.create","model":"gpt-5.1","prompt_cache_breakpoint":1,"input":"preserve explicit upstream error"}`)
	require.Error(t, err)
	t.Logf("upstream_dispatches=%d", len(upstream.requests))
	assert.Len(t, upstream.requests, 1, "an explicit upstream validation error must not trigger automatic body mutation and resend")
}

func TestSmartWSNativeContractUncertainSecondTurn(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			var dispatches atomic.Int32
			svc, account, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				upstream, err := coderws.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer upstream.CloseNow()
				for turn := 0; turn < 2; turn++ {
					_, _, err = upstream.Read(r.Context())
					if err != nil {
						return
					}
					dispatches.Add(1)
					if turn == 0 {
						_ = upstream.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_first","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`))
					}
				}
			})
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			frames, err := nativeContractFrames(t, svc, account,
				`{"type":"response.create","model":"gpt-5.1","input":"first"}`,
				`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_first","input":"second"}`)
			require.Error(t, err)
			require.EqualValues(t, 2, dispatches.Load(), "no turn may be replayed")
			require.Len(t, frames, 2)
			require.Equal(t, "response.completed", gjson.GetBytes(frames[0], "type").String())
			require.Equal(t, smartWSUncertainCode, gjson.GetBytes(frames[1], "error.code").String())
		})
	}
}
