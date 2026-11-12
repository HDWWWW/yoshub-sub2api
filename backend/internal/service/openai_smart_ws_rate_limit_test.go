//go:build unit

package service

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const smartWSRateLimitFixture = `{"type":"error","error":{"code":"rate_limit_exceeded","type":"usage_limit_reached","message":"The usage limit has been reached"}}`

func TestSmartWSRateLimitRecoveryRequiresUnacceptedRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name   string
			frames []string
			retry  bool
			usage  int
		}{
			{"rejected", []string{smartWSRateLimitFixture}, true, 0},
			{"control_then_rejected", []string{`{"type":"codex.rate_limits","rate_limits":{}}`, smartWSRateLimitFixture}, true, 0},
			{"accepted_then_rejected", []string{forcedWSCreated, smartWSRateLimitFixture}, false, 0},
			{"failed_with_usage", []string{`{"type":"response.failed","response":{"id":"resp_used","status":"failed","usage":{"input_tokens":9,"output_tokens":2},"error":{"code":"rate_limit_exceeded"}}}`}, false, 9},
			{"bare_error_then_usage", []string{smartWSRateLimitFixture, `{"type":"response.failed","response":{"id":"resp_used","status":"failed","usage":{"input_tokens":9,"output_tokens":2},"error":{"code":"rate_limit_exceeded"}}}`}, false, 9},
			{"message_only", []string{`{"type":"error","error":{"code":"server_error","message":"rate limit exceeded after execution"}}`}, false, 0},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				var attempts atomic.Int32
				svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
					conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer func() { _ = conn.Close() }()
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					frames := tc.frames
					if attempts.Add(1) > 1 {
						frames = []string{forcedWSCreated, forcedWSCompleted}
					}
					for _, payload := range frames {
						if conn.WriteMessage(websocket.TextMessage, []byte(payload)) != nil {
							return
						}
					}
				})
				c, rec := forcedWSContext(t, "/v1/responses", 1, "rate-limit-recovery")
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello"}`, stream))
				result, err := svc.Forward(c.Request.Context(), c, account, body)
				var failure *UpstreamFailoverError
				require.ErrorAs(t, err, &failure)
				require.Equal(t, tc.retry, failure.ShouldRetryNextAccount())
				require.EqualValues(t, 1, attempts.Load(), "the transport must not retry on its own")
				require.Zero(t, transport.callCount)
				if tc.usage > 0 {
					require.NotNil(t, result)
					require.Equal(t, tc.usage, result.Usage.InputTokens)
				}
				if tc.retry {
					require.NotContains(t, rec.Body.String(), `"type":"error"`)
					// Reuse the request context like the handler does. The failed
					// attempt's state and usage must not poison the successful one.
					result, err = svc.Forward(c.Request.Context(), c, account, body)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, 7, result.Usage.OutputTokens)
					require.EqualValues(t, 2, attempts.Load())
					require.False(t, strings.Contains(rec.Body.String(), "smart_ws_dispatch_uncertain"))
				}
			})
		}
	}
}

func TestSmartWSNativeRateLimitTailWithUsageNeverRetries(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			svc, account, _ := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(smartWSRateLimitFixture))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.failed","response":{"id":"resp_used","status":"failed","usage":{"input_tokens":9,"output_tokens":2},"error":{"code":"rate_limit_exceeded"}}}`))
			})
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			frames, err := nativeContractFrames(t, svc, account, `{"type":"response.create","model":"gpt-5.1","input":"hello"}`)
			require.Len(t, frames, 2)
			require.Contains(t, string(frames[1]), `"input_tokens":9`)
			if failure, ok := err.(*UpstreamFailoverError); ok {
				require.False(t, failure.ShouldRetryNextAccount())
			}
		})
	}
}

// Rejections may use a retryable status/type or follow an apparently harmless
// quota error. None of these envelopes may reopen either retry path.
func TestSmartWSPolicyRejectionNeverRetries(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio, "content_policy_violation", "content_filter"} {
		policy := fmt.Sprintf(`{"type":"error","error":{"code":%q,"type":"rate_limit_error","status":429,"message":"blocked by upstream policy"}}`, code)
		failed := fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":{"code":%q,"status":429,"message":"blocked by upstream policy"}}}`, code)
		wrapped := fmt.Sprintf(`{"type":"error","error":{"code":"rate_limit_exceeded"},"response":{"error":{"code":%q,"message":"blocked by upstream policy"}}}`, code)
		for _, tc := range []struct {
			name   string
			frames []string
		}{
			{"direct", []string{policy}},
			{"wrapped", []string{wrapped}},
			{"failed_tail", []string{smartWSRateLimitFixture, failed}},
			{"metadata_then_failed_tail", []string{`{"type":"codex.rate_limits","rate_limits":{}}`, smartWSRateLimitFixture, failed}},
			{"intermediate_error", []string{smartWSRateLimitFixture, policy, `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded"},"usage":{"input_tokens":11,"output_tokens":3}}}`}},
		} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", code, tc.name, stream), func(t *testing.T) {
					var attempts atomic.Int32
					svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
						attempts.Add(1)
						conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer func() { _ = conn.Close() }()
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
						for _, frame := range tc.frames {
							if conn.WriteMessage(websocket.TextMessage, []byte(frame)) != nil {
								return
							}
						}
					})
					c, rec := forcedWSContext(t, "/v1/responses", 1, "policy-no-retry")
					_, err := svc.Forward(c.Request.Context(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","input":"hello","stream":%t}`, stream)))
					var failure *UpstreamFailoverError
					require.Error(t, err)
					if errors.As(err, &failure) {
						require.False(t, failure.ShouldRetryNextAccount())
						require.False(t, failure.RetryableOnSameAccount)
					}
					require.EqualValues(t, 1, attempts.Load())
					require.Zero(t, transport.callCount)
					require.Contains(t, rec.Body.String(), code)
					if rec.Code >= 400 {
						require.Equal(t, code, gjson.Get(rec.Body.String(), "error.code").String(), "NewAPI must see the exact policy in the standard HTTP error object")
					}
					require.NotContains(t, rec.Body.String(), `"code":"rate_limit_exceeded"`, "do not expose a superseded rate-limit error before the policy refusal")
					if code == OpenAIPolicyCyber || code == OpenAIPolicyBio {
						mark := GetOpsCyberPolicy(c)
						require.NotNil(t, mark)
						require.Equal(t, code, mark.Code)
						require.Contains(t, rec.Body.String(), code)
						if tc.name == "intermediate_error" {
							require.Equal(t, 11, mark.UpstreamInTok)
							require.Equal(t, 3, mark.UpstreamOutTok)
						}
					}
				})
			}
		}
	}
}
