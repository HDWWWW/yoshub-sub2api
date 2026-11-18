//go:build unit

package service

import (
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// Two real HTTP requests on one physical upstream WS. Assert what reaches the
// formatter, the usage collector, and the handler's retry decision separately.
func TestIndependentSmartWSReusedTurnOwnership(t *testing.T) {
	const prior = `{"type":"response.completed","response":{"id":"resp_prior","status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":5}}}`
	const clean = `{"type":"keepalive"}`
	const stale = `{"type":"keepalive","response_id":"resp_prior","usage":{"input_tokens":11,"output_tokens":5}}`
	const staleFailed = `{"type":"response.failed","response":{"id":"resp_prior","usage":{"input_tokens":11},"error":{"code":"server_error"}}}`
	const ambiguous = `{"type":"keepalive","usage":{"input_tokens":99}}`
	const created = `{"type":"response.created","response":{"id":"resp_new","status":"in_progress"}}`
	const complete = `{"type":"response.completed","response":{"id":"resp_new","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2}}}`
	const partial = `{"type":"keepalive","response_id":"resp_new","usage":{"input_tokens":3,"output_tokens":2}}`
	const out = `{"type":"response.output_text.delta","response_id":"resp_new","delta":"work"}`
	for _, tc := range []struct {
		name           string
		frames         []string
		success, retry bool
		in, out        int
	}{
		{"previous_usage_then_success", []string{stale, clean, created, complete}, true, false, 3, 2},
		{"previous_id_then_429", []string{`{"type":"keepalive","response_id":"resp_prior"}`, smartWSRateLimitFixture}, false, true, 0, 0},
		{"previous_usage_then_429", []string{stale, smartWSRateLimitFixture}, false, true, 0, 0},
		{"previous_usage_then_disconnect", []string{stale}, false, true, 0, 0},
		{"previous_usage_in_failure_tail", []string{smartWSRateLimitFixture, stale, staleFailed}, false, true, 0, 0},
		{"terminal_then_clean_tail", []string{`{"type":"error","status":400,"error":{"code":"invalid_value","message":"bad field"}}`, clean}, false, false, 0, 0},
		{"unknown_usage_before_created", []string{ambiguous}, false, false, 0, 0},
		{"unknown_work_before_created", []string{`{"type":"keepalive","item":{"type":"function_call","name":"test"}}`}, false, false, 0, 0},
		{"unknown_tagged_control_before_created", []string{`{"type":"keepalive","response_id":"resp_unconfirmed","response":{"usage":{"input_tokens":99}}}`}, false, false, 0, 0},
		{"unknown_usage_in_failure_tail", []string{smartWSRateLimitFixture, ambiguous}, false, false, 0, 0},
		{"unknown_failed_tail_without_id", []string{smartWSRateLimitFixture, `{"type":"response.failed","response":{"usage":{"input_tokens":99}}}`}, false, false, 0, 0},
		{"anonymous_usage_after_identified_error", []string{created, smartWSRateLimitFixture, `{"type":"response.failed","response":{"usage":{"input_tokens":99}}}`}, false, false, 0, 0},
		{"keep_current_usage_after_stale", []string{created, stale, partial}, false, false, 3, 2},
		{"current_work_in_tagged_control", []string{created, `{"type":"keepalive","response_id":"resp_new","item":{"type":"function_call","name":"test"}}`}, false, false, 0, 0},
		{"current_usage_in_failure_tail", []string{created, smartWSRateLimitFixture, stale, `{"type":"response.failed","response":{"id":"resp_new","status":"failed","usage":{"input_tokens":3,"output_tokens":2}}}`}, false, false, 3, 2},
		{"current_output_then_drop", []string{created, out, stale}, false, false, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sockets, requests atomic.Int32
			s, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				ws, err := forcedWSUpgrader.Upgrade(w, r, nil)
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
						frames = []string{prior}
					}
					for _, f := range frames {
						if ws.WriteMessage(websocket.TextMessage, []byte(f)) != nil {
							return
						}
					}
				}
			})
			for turn := 0; turn < 2; turn++ {
				c, rec := forcedWSContext(t, "/v1/responses", 1, "ownership")
				c.Request.Header.Set("session_id", "ownership")
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"prompt_cache_key":"ownership","input":"turn %d"}`, turn == 1, turn))
				r, err := s.Forward(c.Request.Context(), c, account, body)
				if turn == 0 {
					require.NoError(t, err)
					require.NotNil(t, r)
					require.Equal(t, 11, r.Usage.InputTokens)
					continue
				}
				require.True(t, c.GetBool(OpsOpenAIWSConnReusedKey))
				require.EqualValues(t, 1, sockets.Load())
				require.EqualValues(t, 2, requests.Load())
				require.Zero(t, transport.callCount)
				if tc.success {
					require.NoError(t, err)
					require.Contains(t, rec.Body.String(), clean)
				} else {
					require.Error(t, err)
					var failure *UpstreamFailoverError
					if errors.As(err, &failure) {
						require.Equal(t, tc.retry, failure.ShouldRetryNextAccount())
						require.Equal(t, tc.retry, failure.RetryableOnSameAccount)
					} else {
						require.False(t, tc.retry)
						require.True(t, c.Writer.Written(), "only a committed stream may surface a read error without a retry decision")
						require.False(t, smartWSCanRetryHTTPBeforeOutput(c))
					}
				}
				require.NotContains(t, rec.Body.String(), "resp_prior")
				if tc.name == "terminal_then_clean_tail" {
					require.Contains(t, rec.Body.String(), "bad field")
					require.False(t, smartWSOutcomeUncertain(c), "a tail heartbeat must not reopen a known terminal failure")
				}
				var in, out int
				if r != nil {
					in, out = r.Usage.InputTokens, r.Usage.OutputTokens
				}
				require.Equal(t, tc.in, in)
				require.Equal(t, tc.out, out)
			}
		})
	}
}
