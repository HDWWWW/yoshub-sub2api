//go:build unit

package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAnthropicPreContentObservedWorkPreventsReplay(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		retry       bool
	}{
		{"function_call", `{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`, false},
		{"server_tool", `{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`, false},
		{"observed_usage", `{"type":"response.in_progress","response":{"id":"resp_pre","usage":{"input_tokens":9,"output_tokens":0},"output":[]}}`, false},
		{"null_usage", `{"type":"response.in_progress","response":{"id":"resp_pre","usage":null,"output":[]}}`, true},
		{"empty_reasoning", `{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`, true},
		{"empty_message", `{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","content":[]}}`, true},
	} {
		for _, terminal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/terminal=%t", tc.name, terminal), func(t *testing.T) {
				stream := preContentCreatedFrame + "data: " + tc.frame + "\n\n"
				if terminal {
					stream += preContentFailoverFailed
				}
				rec, _, err := forwardPreContent(t, rawChatCompletionsTestConfig(), nil, io.NopCloser(strings.NewReader(stream)))
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.Equal(t, tc.retry, errors.As(err, &failover), "observed work must not be retried just because the prelude is buffered")
				if tc.retry {
					require.Empty(t, rec.Body.String())
				} else {
					require.Contains(t, rec.Body.String(), "event: error")
				}
			})
		}
	}
}

func TestAnthropicPreContentFailurePreservesObservedUsage(t *testing.T) {
	stream := preContentCreatedFrame +
		`data: {"type":"response.in_progress","response":{"id":"resp_pre","usage":{"input_tokens":9,"output_tokens":0},"output":[]}}` + "\n\n" +
		`data: {"type":"response.failed","response":{"id":"resp_pre","status":"failed","usage":{"input_tokens":0,"output_tokens":0},"error":{"code":"server_error","message":"temporary upstream failure"}}}` + "\n\n"
	rec, c, _ := newPreContentContext()
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig()}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
	result, err := svc.handleAnthropicStreamingResponse(resp, c, rawChatCompletionsTestAccount(), "model", "model", "model", time.Now())
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.Equal(t, 9, result.Usage.InputTokens, "zero terminal usage must not erase observed billable work")
	require.Contains(t, rec.Body.String(), "event: error")
}

func TestAnthropicPreContentOutputSnapshotPreventsReplay(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		retry        bool
	}{
		{"server_tool", `[{"type":"web_search_call","id":"ws_1","status":"completed"}]`, false},
		{"function_tool", `[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}]`, false},
		{"message_text", `[{"type":"message","content":[{"type":"output_text","text":"already generated"}]}]`, false},
		{"reasoning_text", `[{"type":"reasoning","summary":[{"type":"summary_text","text":"already reasoned"}]}]`, false},
		{"encrypted_reasoning", `[{"type":"reasoning","encrypted_content":"sealed","summary":[]}]`, false},
		{"opaque_item", `[{"type":"provider_tool","status":"completed","result":{"ok":true}}]`, false},
		{"opaque_content", `[{"type":"message","content":[{"type":"audio","data":"encoded"}]}]`, false},
		{"empty_message", `[{"type":"message","content":[]}]`, true},
		{"empty_message_part", `[{"type":"message","content":[{"type":"output_text","text":""}]}]`, true},
		{"empty_reasoning", `[{"type":"reasoning","summary":[]}]`, true},
		{"empty_reasoning_part", `[{"type":"reasoning","summary":[{"type":"summary_text","text":""}]}]`, true},
	} {
		for _, terminal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/terminal=%t", tc.name, terminal), func(t *testing.T) {
				kind, status, failure := "response.in_progress", "in_progress", ""
				if terminal {
					kind, status, failure = "response.failed", "failed", `,"error":{"code":"server_error","message":"temporary upstream failure"}`
				}
				frame := fmt.Sprintf(`{"type":%q,"response":{"id":"resp_pre","status":%q,"usage":null,"output":%s%s}}`, kind, status, tc.output, failure)
				stream := preContentCreatedFrame + "data: " + frame + "\n\n"
				rec, _, err := forwardPreContent(t, rawChatCompletionsTestConfig(), nil, io.NopCloser(strings.NewReader(stream)))
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.Equal(t, tc.retry, errors.As(err, &failover), "output snapshots carry the same execution boundary as individual item events")
				if tc.retry {
					require.Empty(t, rec.Body.String())
				} else {
					require.Contains(t, rec.Body.String(), "event: error")
				}
			})
		}
	}
}

func TestWriteAnthropicPolicyErrorAfterKeepalivePreservesCode(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		t.Run(code, func(t *testing.T) {
			c, rec := newCompatStreamContext("/v1/messages", nil)
			setSSEHeaders(c)
			_, err := io.WriteString(c.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
			require.NoError(t, err)
			MarkOpsCyberPolicy(c, CyberPolicyMark{Code: code, Message: "blocked"})
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "blocked")
			body := rec.Body.String()
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, 1, strings.Count(body, "event: error\n"))
			for _, line := range strings.Split(body, "\n") {
				require.False(t, strings.HasPrefix(line, "{"), "an error after ping must remain SSE")
				if payload, ok := extractOpenAISSEDataLine(line); ok && gjson.Get(payload, "type").String() == "error" {
					require.Equal(t, code, gjson.Get(payload, "error.code").String())
				}
			}
		})
	}
}

func TestAnthropicStreamErrorAfterPingPreservesUpstreamCode(t *testing.T) {
	c, rec := newCompatStreamContext("/v1/messages", nil)
	setSSEHeaders(c)
	n, err := io.WriteString(c.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	require.NoError(t, err)
	recordOpenAIStreamKeepaliveBytes(c, n)
	payload := `{"type":"error","status":400,"error":{"code":"invalid_request_error","type":"invalid_request_error","message":"invalid input"}}`
	svc, _, account := newCompatStreamSideEffectsService(compatStreamSideEffectsSSE(false, payload))
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(compatStreamSideEffectsSSE(false, payload)))}
	_, err = svc.handleAnthropicStreamingResponse(resp, c, account, "model", "model", "model", time.Now())
	require.Error(t, err)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error\n"))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		require.False(t, strings.HasPrefix(line, "{"), "the custom upstream-code branch must also remain SSE")
		if data, ok := extractOpenAISSEDataLine(line); ok && gjson.Get(data, "type").String() == "error" {
			require.Equal(t, "invalid_request_error", gjson.Get(data, "error.code").String())
			require.Equal(t, int64(http.StatusBadRequest), gjson.Get(data, "error.status_code").Int())
		}
	}
}
