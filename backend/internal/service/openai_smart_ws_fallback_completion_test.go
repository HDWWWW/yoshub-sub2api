//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSmartWSFallbackRequiresTerminal(t *testing.T) {
	partial := `{"type":"response.in_progress","response":{"id":"resp_partial","status":"in_progress","model":"gpt-5.1","usage":{"input_tokens":13,"output_tokens":2}}}`
	delta := "data: " + partial + "\n\ndata: " + `{"type":"response.output_text.delta","delta":"partial"}` + "\n\n"
	for _, passthrough := range []bool{false, true} {
		for _, scenario := range []struct {
			name, body, contentType          string
			stream, split, usage, background bool
		}{
			{"empty_json", "", "application/json", false, false, false, false},
			{"truncated_json", `{"id":"resp_partial","status":"completed","output":[`, "application/json", false, false, false, false},
			{"nonterminal_json", `{"id":"resp_partial","status":"in_progress","output":[],"usage":{"input_tokens":13,"output_tokens":2}}`, "application/json", false, false, true, false},
			{"delta_then_eof", delta, "text/event-stream", true, false, true, false},
			{"delta_then_done", delta + "data: [DONE]\n\n", "text/event-stream", true, false, true, false},
			{"delta_then_done_json", delta + "data: [DONE]\n\n", "text/event-stream", false, false, true, false},
			{"split_done_no_newline", delta + "data: [DONE]", "text/event-stream", true, true, true, false},
			{"split_missing_type", delta + "data: [DONE]\r\n\r\n", "", false, true, true, false},
			{"empty_sse", "", "text/event-stream", true, false, false, false},
			{"background_stream_requires_terminal", delta + "data: [DONE]\n\n", "text/event-stream", true, true, true, true},
		} {
			t.Run(fmt.Sprintf("passthrough=%v/%s", passthrough, scenario.name), func(t *testing.T) {
				svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
				account.Extra["openai_passthrough"] = passthrough
				var reader io.Reader = strings.NewReader(scenario.body)
				if scenario.split {
					reader = iotest.OneByteReader(reader)
				}
				transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {scenario.contentType}}, Body: io.NopCloser(reader)}}
				c, rec := forcedWSContext(t, "/v1/responses", 1, "missing-terminal")
				result, err := svc.Forward(c.Request.Context(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"background":%t,"input":"controlled missing terminal","reasoning":{"effort":"high"},"service_tier":"priority"}`, scenario.stream, scenario.background)))
				metrics := svc.getOpenAIWSConnPool().SmartWSSnapshot().Counters
				require.Equal(t, 1, transport.callCount, "uncertain requests must never replay")
				assert.Error(t, err)
				assert.Contains(t, rec.Body.String(), smartWSUncertainCode)
				assert.NotContains(t, rec.Body.String(), "[DONE]")
				assert.NotContains(t, rec.Body.String(), `"status":"completed"`)
				assert.EqualValues(t, 1, metrics["dispatch_uncertain"])
				assert.EqualValues(t, 1, metrics["transport_error"])
				assert.Zero(t, metrics["succeeded"])
				if scenario.usage {
					require.NotNil(t, result, "observed partial usage must remain billable")
					assert.Equal(t, 13, result.Usage.InputTokens)
					assert.Equal(t, 2, result.Usage.OutputTokens)
					assert.False(t, result.OpenAIWSMode)
					assert.Equal(t, "high", *result.ReasoningEffort)
					assert.Equal(t, "priority", *result.ServiceTier)
				}
			})
		}
	}
}

func TestSmartWSFallbackAcceptsBackgroundAcknowledgement(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, status := range []string{"queued", "in_progress"} {
			for _, large := range []bool{false, true} {
				t.Run(fmt.Sprintf("passthrough=%t/%s/large=%t", passthrough, status, large), func(t *testing.T) {
					svc, account, transport := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("background request attempted WS") })
					account.Extra["openai_passthrough"] = passthrough
					input := "hello"
					if large {
						svc.cfg.Gateway.OpenAIWS.MaxRequestBytes = 2048
						input = strings.Repeat("x", 4096)
					}
					payload := fmt.Sprintf(`{"id":"resp_background","object":"response","model":"gpt-5.1","status":"%s","background":true,"output":[],"usage":null}`, status)
					transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}}
					c, rec := forcedWSContext(t, "/v1/responses", 1, "background")
					_, err := svc.Forward(c.Request.Context(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","background":true,"input":"%s"}`, input)))
					require.NoError(t, err)
					assert.Equal(t, http.StatusOK, rec.Code)
					assert.Contains(t, rec.Body.String(), `"status":"`+status+`"`)
					assert.Contains(t, rec.Body.String(), "resp_background")
					assert.NotContains(t, rec.Body.String(), smartWSUncertainCode)
					assert.Equal(t, 1, transport.callCount)
					metrics := svc.getOpenAIWSConnPool().SmartWSSnapshot().Counters
					assert.EqualValues(t, 1, metrics["background_accepted"])
					assert.Zero(t, metrics["succeeded"])
					assert.Zero(t, metrics["failed"])
					assert.Zero(t, metrics["dispatch_uncertain"])
				})
			}
		}
	}
}

func TestSmartWSFallbackCompletedSentinelWithSplitReads(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/stream=%t", passthrough, stream), func(t *testing.T) {
				svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
				account.Extra["openai_passthrough"] = passthrough
				payload := "data: " + forcedWSCompleted + "\r\n\r\ndata: [DONE]\r\n\r\n"
				transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(iotest.OneByteReader(strings.NewReader(payload)))}}
				c, rec := forcedWSContext(t, "/v1/responses", 1, "complete-split")
				result, err := svc.Forward(c.Request.Context(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"hello"}`, stream)))
				require.NoError(t, err)
				require.NotNil(t, result)
				assert.Contains(t, rec.Body.String(), "completed")
				assert.NotContains(t, rec.Body.String(), smartWSUncertainCode)
				metrics := svc.getOpenAIWSConnPool().SmartWSSnapshot().Counters
				assert.EqualValues(t, 1, metrics["succeeded"])
				assert.Zero(t, metrics["dispatch_uncertain"])
			})
		}
	}
}

func TestSmartWSNativeBridgeRejectsDoneWithoutTerminal(t *testing.T) {
	svc, account, transport := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("bridge attempted WS") })
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
	svc.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 1
	transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + forcedWSCreated + "\n\ndata: [DONE]\n\n"))}}
	frames, err := nativeContractFrames(t, svc, account, `{"type":"response.create","model":"gpt-5.1","input":"hello"}`)
	require.Error(t, err)
	require.NotEmpty(t, frames)
	assert.Contains(t, string(frames[len(frames)-1]), smartWSUncertainCode)
	assert.Equal(t, 1, transport.callCount)
	metrics := svc.getOpenAIWSConnPool().SmartWSSnapshot().Counters
	assert.EqualValues(t, 1, metrics["dispatch_uncertain"])
	assert.Zero(t, metrics["succeeded"])
}

func TestSmartWSFallbackResponseBounds(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, format := range []string{"json", "sse"} {
			t.Run(fmt.Sprintf("passthrough=%t/%s", passthrough, format), func(t *testing.T) {
				svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
				account.Extra["openai_passthrough"] = passthrough
				svc.cfg.Gateway.UpstreamResponseReadMaxBytes = 512
				svc.cfg.Gateway.MaxLineSize = 512
				payload := `{"status":"in_progress","padding":"` + strings.Repeat("x", 2048) + `"}`
				contentType := "application/json"
				if format == "sse" {
					contentType = "text/event-stream"
					payload = "data: " + payload + "\n\n"
				}
				transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}}
				c, rec := forcedWSContext(t, "/v1/responses", 1, "response-bound")
				_, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","input":"hello"}`))
				require.Error(t, err)
				assert.Equal(t, http.StatusBadGateway, rec.Code)
				assert.True(t, json.Valid(rec.Body.Bytes()), "must not append SSE to an already written JSON error")
				assert.Contains(t, rec.Body.String(), smartWSUncertainCode)
				assert.Equal(t, 1, transport.callCount)
			})
		}
	}
}

func TestSmartWSFallbackLargeEventIsFullyObservedBeforeDelivery(t *testing.T) {
	for _, size := range []int{4096, 4097, 8192} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
			payload := "data: " + strings.TrimSuffix(forcedWSCompleted, "}") + `,"padding":"` + strings.Repeat("x", size) + `"}`
			transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload + "\n\ndata: [DONE]\n\n"))}}
			c, _ := forcedWSContext(t, "/v1/responses", 1, "large-event")
			_, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","input":"hello","stream":true}`))
			require.NoError(t, err)
			assert.False(t, smartWSOutcomeUncertain(c))
			assert.EqualValues(t, 1, svc.getOpenAIWSConnPool().SmartWSSnapshot().Counters["succeeded"])
		})
	}
}
