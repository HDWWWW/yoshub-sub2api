//go:build unit

package service

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSmartWSFallbackTerminalClassification(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, state := range []string{"completed", "incomplete", "failed"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, state), func(t *testing.T) {
				svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
				terminal := fmt.Sprintf(`{"type":"response.%s","response":{"id":"resp_fallback_audit","model":"gpt-5.1","status":"%s","output":[],"usage":{"input_tokens":17,"output_tokens":3},"error":{"code":"audit_terminal","message":"preserve explicit outcome"}}}`, state, state)
				transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + terminal + "\n\n"))}}
				c, rec := forcedWSContext(t, "/v1/responses", 1, "fallback-terminal")
				result, err := svc.Forward(c.Request.Context(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"synthetic fallback audit"}`, stream)))
				snapshot := svc.getOpenAIWSConnPool().SmartWSSnapshot()
				t.Logf("state=%s stream=%t dispatches=%d result_present=%t error=%v counters=%v downstream=%s", state, stream, transport.callCount, result != nil, err, snapshot.Counters, rec.Body.String())
				require.Equal(t, 1, transport.callCount)
				require.NotNil(t, result)
				assert.Equal(t, 17, result.Usage.InputTokens)
				assert.Equal(t, 3, result.Usage.OutputTokens)
				assert.Zero(t, snapshot.Counters["dispatch_uncertain"])
				assert.Zero(t, snapshot.Counters["transport_error"])
				assert.NotContains(t, rec.Body.String(), "smart_ws_dispatch_uncertain")
				if state == "failed" {
					assert.Error(t, err)
					assert.EqualValues(t, 1, snapshot.Counters["upstream_terminal_error"])
					assert.Zero(t, snapshot.Counters["succeeded"])
					assert.Contains(t, rec.Body.String(), "audit_terminal")
				} else if state == "incomplete" {
					assert.EqualValues(t, 1, snapshot.Counters["incomplete"])
					assert.Zero(t, snapshot.Counters["succeeded"])
				} else {
					assert.NoError(t, err)
					assert.EqualValues(t, 1, snapshot.Counters["succeeded"])
				}
			})
		}
	}
}

func TestSmartWSFallbackPreservesJSONAndSplitSSEFailures(t *testing.T) {
	for _, kind := range []string{"json", "sse", "missing_content_type", "split_reads", "bare_tail", "unrelated_tail"} {
		t.Run(kind, func(t *testing.T) {
			svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
			failed := `{"type":"response.failed","response":{"id":"resp_failed_audit","status":"failed","model":"gpt-5.1","usage":{"input_tokens":17,"output_tokens":3},"error":{"code":"audit_failure","message":"keep original error"}}}`
			payload := "event: response.failed\r\ndata: " + failed + "\r\n\r\n"
			contentType := "text/event-stream"
			if kind == "json" {
				payload = `{"id":"resp_failed_audit","status":"failed","model":"gpt-5.1","usage":{"input_tokens":17,"output_tokens":3},"error":{"code":"audit_failure","message":"keep original error"}}`
				contentType = "application/json"
			}
			if kind == "missing_content_type" {
				contentType = ""
			}
			var source io.ReadCloser = io.NopCloser(strings.NewReader(payload))
			if kind == "split_reads" {
				source = io.NopCloser(iotest.OneByteReader(strings.NewReader(payload)))
			}
			if kind == "bare_tail" || kind == "unrelated_tail" {
				pipe, writer := io.Pipe()
				source = pipe
				t.Cleanup(func() { _ = pipe.Close(); _ = writer.Close() })
				go func() {
					defer writer.Close()
					_, _ = io.WriteString(writer, "data: "+`{"type":"error","response_id":"resp_failed_audit","usage":{"input_tokens":7,"output_tokens":1},"error":{"code":"audit_failure","message":"keep original error"}}`+"\n\n")
					time.Sleep(10 * time.Millisecond)
					if kind == "unrelated_tail" {
						failed = strings.ReplaceAll(failed, "resp_failed_audit", "resp_unrelated")
					}
					_, _ = io.WriteString(writer, "data: "+failed+"\n\n")
				}()
			}
			transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: source}}
			c, rec := forcedWSContext(t, "/v1/responses", 1, "fallback-errors")
			result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","stream":false,"input":"synthetic audit","reasoning":{"effort":"high"},"service_tier":"priority"}`))
			require.Error(t, err)
			require.NotNil(t, result)
			assert.False(t, result.OpenAIWSMode, "HTTP fallback retains HTTP billing attribution")
			input, output := 17, 3
			if kind == "unrelated_tail" {
				input, output = 7, 1
			}
			assert.Equal(t, input, result.Usage.InputTokens)
			assert.Equal(t, output, result.Usage.OutputTokens)
			assert.Equal(t, "high", *result.ReasoningEffort)
			assert.Equal(t, "priority", *result.ServiceTier)
			assert.Contains(t, rec.Body.String(), "audit_failure")
			if kind == "json" {
				assert.JSONEq(t, payload, rec.Body.String())
			}
			assert.NotContains(t, rec.Body.String(), smartWSUncertainCode)
			assert.Equal(t, 1, transport.callCount)
			metrics := svc.getOpenAIWSConnPool().SmartWSSnapshot().Counters
			assert.EqualValues(t, 1, metrics["upstream_terminal_error"])
			assert.Zero(t, metrics["dispatch_uncertain"])
			assert.Zero(t, metrics["succeeded"])
		})
	}
}

func TestSmartWSFallbackBareErrorTailHasDeadline(t *testing.T) {
	svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
	pipe, writer := io.Pipe()
	t.Cleanup(func() { _ = pipe.Close(); _ = writer.Close() })
	go func() {
		_, _ = io.WriteString(writer, "data: "+`{"type":"error","usage":{"input_tokens":7,"output_tokens":1},"error":{"code":"audit_failure","message":"keep error"}}`+"\n\n")
	}()
	transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pipe}}
	c, rec := forcedWSContext(t, "/v1/responses", 1, "bounded-tail")
	started := time.Now()
	result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","stream":false,"input":"synthetic audit"}`))
	require.Error(t, err)
	require.NotNil(t, result)
	assert.Less(t, time.Since(started), time.Second)
	assert.Equal(t, 7, result.Usage.InputTokens)
	assert.Contains(t, rec.Body.String(), "audit_failure")
	assert.NotContains(t, rec.Body.String(), smartWSUncertainCode)
}
