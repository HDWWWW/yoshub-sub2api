//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
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
			err := writeSmartWSNativeFailure(c, tc.status, []byte(tc.body), nil, func(payload []byte) error {
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
	err := writeSmartWSNativeFailure(c, 400, []byte(`{"detail":"not delivered"}`), nil, func([]byte) error {
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
