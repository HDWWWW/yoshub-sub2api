//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSmartWSNativeDeliveredErrorLogPreservesMessageAndStatus(t *testing.T) {
	const message = "System messages are not allowed"
	for _, tc := range []struct {
		name, body string
		status     int
		wantStatus int
	}{
		{"detail", `{"detail":"System messages are not allowed"}`, 400, 400},
		{"string_error", `{"error":"System messages are not allowed"}`, 400, 400},
		{"nested_error", `{"response":{"error":{"message":"System messages are not allowed"}}}`, 400, 400},
		{"nested_string_error", `{"response":{"error":"System messages are not allowed"}}`, 400, 400},
		{"top_message", `{"message":"System messages are not allowed"}`, 403, 403},
		{"plain_text", message, 400, 400},
		{"existing_error_envelope", `{"type":"error","error":{"message":"System messages are not allowed"}}`, 400, 400},
		{"existing_failed_envelope", `{"type":"response.failed","response":{"error":{"message":"System messages are not allowed"}}}`, 503, 503},
		{"explicit_error_status", `{"type":"error","error":{"message":"System messages are not allowed","status_code":429}}`, 400, 429},
		{"top_status_code", `{"type":"response.failed","status_code":400,"response":{"error":{"message":"System messages are not allowed"}}}`, 502, 400},
		{"string_status", `{"type":"error","status":"429","error":{"message":"System messages are not allowed"}}`, 400, 429},
		{"empty_nested_error", `{"type":"response.failed","response":{"error":{}},"error":{"code":"invalid_request_error","type":"invalid_request_error","message":"System messages are not allowed"}}`, 400, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newOpenAIUpstreamErrorTestContext(t)
			SetOpenAIClientTransport(c, OpenAIClientTransportWS)
			var delivered []byte
			err := writeSmartWSNativeFailure(c, nil, tc.status, []byte(tc.body), nil, func(payload []byte) error {
				delivered = append([]byte(nil), payload...)
				return nil
			})
			require.Error(t, err)
			require.Contains(t, string(delivered), message)
			marked := GetOpsStreamErrors(c)
			require.Len(t, marked, 1)
			require.Equal(t, message, marked[0].Message)
			require.Equal(t, tc.wantStatus, marked[0].IntendedStatus)
			if tc.name == "empty_nested_error" {
				require.Equal(t, "invalid_request_error", marked[0].Code)
				require.Equal(t, "invalid_request_error", marked[0].ErrType)
			}
		})
	}
}

func TestSmartWSHTTPBridgeDetailErrorIsSearchableInOps(t *testing.T) {
	const message = "System messages are not allowed"
	const body = `{"detail":"System messages are not allowed"}`
	c, _ := newOpenAIUpstreamErrorTestContext(t)
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)
	BeginOpsStreamTurn(c, 1)
	svc, account, _ := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected WS upstream call") })
	upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIUpstreamErrorResponse(400, body)}}
	svc.httpUpstream = upstream
	requestBody := []byte(`{"model":"gpt-5.1","input":"audit"}`)
	request := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/responses", strings.NewReader(string(requestBody)))
	var delivered []byte
	_, err := svc.proxySmartWSHTTPBridge(context.Background(), c, account, request, "", requestBody,
		"gpt-5.1", "gpt-5.1", "", "", "", apicompat.ResponsesClientToolMapping{}, func(payload []byte) error {
			delivered = append([]byte(nil), payload...)
			return nil
		})
	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
	require.Contains(t, string(delivered), message)
	marked := GetOpsStreamErrors(c)
	require.Len(t, marked, 1)
	require.Equal(t, http.StatusBadRequest, marked[0].IntendedStatus)
	require.Equal(t, message, marked[0].Message)
}

func TestSmartWSNativeFailedDeliveryDoesNotMarkVisibleError(t *testing.T) {
	c, _ := newOpenAIUpstreamErrorTestContext(t)
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)
	err := writeSmartWSNativeFailure(c, nil, 400, []byte(`{"detail":"not delivered"}`), nil, func([]byte) error {
		return context.Canceled
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, GetOpsStreamErrors(c))
}

func TestSmartWSNativeErrorLogDoesNotCaptureUnrelatedPayload(t *testing.T) {
	c, _ := newOpenAIUpstreamErrorTestContext(t)
	SetOpenAIClientTransport(c, OpenAIClientTransportWS)
	payload := []byte(`{"type":"response.failed","response":{"output":[{"type":"message","content":"private generated content"}],"error":{"message":"inspect https://example.invalid/?access_token=do-not-log","status_code":400}}}`)
	markOpenAIWSClientVisibleFailure(c, "response.failed", payload)
	marked := GetOpsStreamErrors(c)
	require.Len(t, marked, 1)
	require.Contains(t, marked[0].Message, "access_token=***")
	require.NotContains(t, marked[0].Message, "do-not-log")
	require.NotContains(t, marked[0].Message+marked[0].UpstreamDetail, "private generated content")
}

func TestOpenAIWSHTTPBridge422LogPrivacy(t *testing.T) {
	const validation = `[{"loc":["body","model"],"msg":"Invalid model","input":{"message":"LOCAL_INPUT_MARK"}}]`
	const noReason = `[{"msg":{"input":"LOCAL_INPUT_MARK"},"input":{"msg":"LOCAL_INPUT_MARK"},"ctx":{"message":"LOCAL_INPUT_MARK"}}]`
	for _, route := range []struct{ name, platform string }{
		{"raw_envelope", PlatformOpenAI},
		{"bridge_string_message", PlatformGrok}, // Non-OpenAI accounts use the ordinary HTTP bridge encoder.
	} {
		for _, tc := range []struct{ name, detail, wireMessage, logMessage string }{
			{"validation", validation, validation, "Invalid model"},
			{"no_text_reason", noReason, noReason, "upstream websocket request failed"},
			{"plain_text", `"[validation] Invalid model"`, "[validation] Invalid model", "[validation] Invalid model"},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				const status = http.StatusUnprocessableEntity
				body := `{"detail":` + tc.detail + `}`
				up := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIUpstreamErrorResponse(status, body)}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: up}
				account := &Account{ID: 704, Platform: route.platform, Type: AccountTypeAPIKey, Concurrency: 1}
				c, _ := newOpenAIUpstreamErrorTestContext(t)
				SetOpenAIClientTransport(c, OpenAIClientTransportWS)
				BeginOpsStreamTurn(c, 1)
				var logged []OpsStreamError
				BindOpsStreamErrorObserver(c, func(e OpsStreamError) { logged = append(logged, e) })
				request := []byte(`{"type":"response.create","model":"gpt-5.1","stream":true,"input":"hello"}`)
				var frames []string
				_, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "test-token", request, len(request),
					"gpt-5.1", "", "", "", "", 1, func(p []byte) error { frames = append(frames, string(p)); return nil })
				require.Error(t, err)
				require.Len(t, up.requests, 1)
				require.Len(t, frames, 1)
				if route.platform == PlatformOpenAI {
					require.Equal(t, int64(422), gjson.Get(frames[0], "status").Int())
					require.Equal(t, tc.logMessage, gjson.Get(frames[0], "error.message").Str)
					require.NotContains(t, frames[0], "LOCAL_INPUT_MARK")
				} else {
					msg, err := json.Marshal(tc.wireMessage)
					require.NoError(t, err)
					require.JSONEq(t, `{"type":"error","status":422,"sequence_number":0,"error":{"type":"upstream_error","message":`+string(msg)+`}}`, frames[0])
				}
				require.Len(t, logged, 1)
				require.Equal(t, logged, GetOpsStreamErrors(c))
				require.Equal(t, status, logged[0].IntendedStatus)
				require.Equal(t, tc.logMessage, logged[0].Message)
				snapshot, err := json.Marshal(logged[0])
				require.NoError(t, err)
				require.NotContains(t, string(snapshot), "LOCAL_INPUT_MARK")
			})
		}
	}
}

func TestOpenAIWS422LogReasonOnDeliveredEnvelope(t *testing.T) {
	const fallback = "upstream websocket request failed"
	for _, tc := range []struct{ name, fields, want string }{
		{"skip_non_text_entries", `"detail":["LOCAL_INPUT_MARK",{"msg":false},{"msg":" "},{"msg":"Invalid model","input":"LOCAL_INPUT_MARK"}]`, "Invalid model"},
		{"skip_input_and_output", `"detail":{"input":{"message":"LOCAL_INPUT_MARK"}},"response":{"output":[{"message":"LOCAL_INPUT_MARK"}]}`, fallback},
		{"object_in_message_string", `"error":{"message":"{\"input\":{\"message\":\"LOCAL_INPUT_MARK\"}}"}`, fallback},
		{"structured_msg_is_not_text", `"detail":[{"msg":"{\"input\":\"LOCAL_INPUT_MARK\"}"}]`, fallback},
		{"truncated_bridge_array", `"error":{"message":"[{\"input\":\"LOCAL_INPUT_MARK\""}`, fallback},
		{"next_explicit_reason", `"response":{"error":{"message":{"input":"LOCAL_INPUT_MARK"}}},"message":"Bad model"`, "Bad model"},
		{"redact_validation_reason", `"detail":[{"msg":"see https://example.invalid/?access_token=LOCAL_INPUT_MARK"}]`, "see https://example.invalid/?access_token=***"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := `{"type":"response.failed", ` + tc.fields + `}`
			require.True(t, gjson.Valid(frame))
			c, _ := newOpenAIUpstreamErrorTestContext(t)
			SetOpenAIClientTransport(c, OpenAIClientTransportWS)
			err := writeSmartWSNativeFailure(c, nil, http.StatusUnprocessableEntity, []byte(frame), nil, func(p []byte) error {
				require.Equal(t, "response.failed", gjson.GetBytes(p, "type").Str)
				require.False(t, gjson.GetBytes(p, "status").Exists())
				require.NotContains(t, string(p), "LOCAL_INPUT_MARK")
				return nil
			})
			require.Error(t, err)
			marked := GetOpsStreamErrors(c)
			require.Len(t, marked, 1)
			require.Equal(t, http.StatusUnprocessableEntity, marked[0].IntendedStatus)
			require.Equal(t, tc.want, marked[0].Message)
		})
	}
}
