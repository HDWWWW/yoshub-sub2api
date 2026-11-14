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
	return nativeContractFramesWithCompletion(t, svc, account, false, payloads...)
}

func nativeContractFramesWithCompletion(t *testing.T, svc *OpenAIGatewayService, account *Account, closeAfterCompletion bool, payloads ...string) ([][]byte, error) {
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
		} else if closeAfterCompletion && gjson.GetBytes(frame, "type").String() == "response.completed" && turn == len(payloads) {
			_ = client.CloseNow()
			break
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

func TestSmartWSNativeLaterTurnRetriesCurrentPayloadOnSameConnection(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, tc := range []struct {
			name                        string
			retries, failures, attempts int
			accepted                    bool
			success                     bool
			status                      int
		}{
			{"disabled", 0, 1, 2, false, false, 429},
			{"one_retry_recovers", 1, 1, 3, false, true, 429},
			{"one_retry_exhausted", 1, 2, 3, false, false, 429},
			{"two_retries_recovers", 2, 2, 4, false, true, 429},
			{"accepted_never_replayed", 2, 1, 2, true, false, 429},
			{"503_recovers", 1, 1, 3, false, true, 503},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var connections atomic.Int32
				requests := make(chan []byte, 8)
				svc, account, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
					upstream, err := coderws.Accept(w, r, nil)
					if err != nil {
						return
					}
					defer upstream.CloseNow()
					connections.Add(1)
					for attempt := 0; ; attempt++ {
						_, request, err := upstream.Read(r.Context())
						if err != nil {
							return
						}
						requests <- append([]byte(nil), request...)
						frame := `{"type":"response.completed","response":{"id":"resp_first","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`
						if attempt > 0 && attempt <= tc.failures {
							if tc.accepted {
								_ = upstream.Write(r.Context(), coderws.MessageText, []byte(`{"type":"response.created","response":{"id":"resp_second","status":"in_progress"}}`))
							}
							frame = smartWSRateLimitFixture
							if tc.status == 503 {
								frame = `{"type":"error","status":503,"error":{"code":"server_error","type":"server_error","message":"temporarily unavailable"}}`
							}
						}
						if upstream.Write(r.Context(), coderws.MessageText, []byte(frame)) != nil {
							return
						}
					}
				})
				account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
				account.Extra["same_account_retry_count"] = tc.retries
				frames, _ := nativeContractFramesWithCompletion(t, svc, account, true,
					`{"type":"response.create","model":"gpt-5.1","input":"first"}`,
					`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_first","input":"second"}`)
				require.EqualValues(t, 1, connections.Load(), "continuation must retain the same upstream session")
				require.Len(t, requests, tc.attempts)
				first, second := <-requests, <-requests
				require.Contains(t, string(first), "first")
				require.Equal(t, "resp_first", gjson.GetBytes(second, "previous_response_id").String())
				for len(requests) > 0 {
					require.JSONEq(t, string(second), string(<-requests), "retry only the current rejected turn")
				}
				require.NotEmpty(t, frames)
				last := frames[len(frames)-1]
				if tc.success {
					require.Len(t, frames, 2, "internal failures must not reach the client")
					require.Equal(t, "response.completed", gjson.GetBytes(last, "type").String())
				} else {
					require.Equal(t, "rate_limit_exceeded", gjson.GetBytes(last, "error.code").String())
				}
			})
		}
	}
}
func TestSmartWSNativeLaterTurnCancelPreventsRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upstream := newStagedPassthroughConn()
	account := passthroughLifecycleAccount()
	account.Extra["same_account_retry_count"] = 2
	server, finished := startPassthroughLifecycleServer(t, ctx,
		newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream), account)
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer client.CloseNow()
	_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_first","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`)
	_, err := readPassthroughLifecycleFrame(t, client, time.Second)
	require.NoError(t, err)
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","previous_response_id":"resp_first","input":"second"}`)))
	_ = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	upstream.Send(smartWSRateLimitFixture)
	require.Eventually(t, func() bool { return len(upstream.frames) == 0 }, time.Second, time.Millisecond)
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.cancel"}`)))
	cancelFrame := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, "response.cancel", gjson.GetBytes(cancelFrame, "type").String())
	failure, err := readPassthroughLifecycleFrame(t, client, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "rate_limit_exceeded", gjson.GetBytes(failure, "error.code").String())
	_ = client.CloseNow()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("cancelled native retry did not finish")
	}
	require.Empty(t, upstream.writes, "response.cancel must prevent another response.create")
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
			const body = `{"error":{"code":"rate_limit_exceeded","message":"audit rejection; no generation accepted"}}`
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
			frames, err := nativeContractFrames(t, svc, account, `{"type":"response.create","model":"gpt-5.1","input":"audit"}`)
			if test.status == 429 {
				require.Empty(t, frames, "safe handshake rejection stays eligible for handler recovery")
			} else {
				require.NotEmpty(t, frames)
			}
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			t.Logf("credential=%s mode=%s status=%d same_account_retry=%t next_account_retry=%t body_bytes=%d", test.credential, mode, failover.StatusCode, failover.RetryableOnSameAccount, failover.ShouldRetryNextAccount(), len(failover.ResponseBody))
			assert.Equal(t, test.status == 429, failover.ShouldRetryNextAccount())
			assert.Equal(t, test.status == 429, failover.ShouldReportAccountScheduleFailure())
			assert.Equal(t, test.status == 429, failover.RetryableOnSameAccount)
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
