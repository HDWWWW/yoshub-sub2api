package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestExcelBPSCompatEndpoints(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("messages=%v/stream=%v", anthropic, stream), func(t *testing.T) {
				wire := "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compat\",\"status\":\"completed\",\"model\":\"gpt-6-astra\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n"
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}}
				s := openAIClientToolsTestService(upstream)
				a := excelAccount()
				a.Credentials["model_mapping"] = map[string]any{"alias": "gpt-6-astra", "gpt-6-astra": "must-not-map-twice"}
				a.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
				body := []byte(fmt.Sprintf(`{"model":"alias","stream":%v,"max_tokens":100,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hello"}]}`, stream))
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body)))
				forward := s.ForwardAsChatCompletions
				if anthropic {
					forward = s.ForwardAsAnthropic
				}
				result, err := forward(context.Background(), c, a, body, "", "")
				require.NoError(t, err)
				require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
				require.Len(t, upstream.requests, 1)
				require.Equal(t, "alias", result.Model)
				require.Equal(t, "gpt-6-astra", result.BillingModel)
				require.Equal(t, "gpt-6-astra", result.UpstreamModel)
				require.Equal(t, stream, result.Stream)
				require.Equal(t, 12, result.Usage.InputTokens)
				require.Equal(t, 3, result.Usage.OutputTokens)
				require.Contains(t, rec.Body.String(), "hello")
				require.NotContains(t, rec.Body.String(), "response.completed")
				if stream {
					if anthropic {
						require.Contains(t, rec.Body.String(), "message_stop")
					} else {
						require.Equal(t, 1, strings.Count(rec.Body.String(), "[DONE]"))
						require.Contains(t, rec.Body.String(), `"completion_tokens":3`)
					}
				} else if anthropic {
					require.Equal(t, "message", gjson.Get(rec.Body.String(), "type").String())
				} else {
					require.Equal(t, "chat.completion", gjson.Get(rec.Body.String(), "object").String())
				}
			})
		}
	}
}

func TestExcelBPSCompatMessagesReasoningPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, preference, target, ceiling, overLimit string
		status                                       int
	}{
		{"synthesized none is mapped", `,"thinking":{"type":"disabled"}`, "low", "", "", 200},
		{"unsupported mapped effort is rejected", `,"thinking":{"type":"disabled"}`, "minimal", "", "", 400},
		{"raw invalid effort cannot be hidden", `,"output_config":{"effort":"none"}`, "low", "", "", 400},
		{"mapping respects ceiling denial", `,"thinking":{"type":"disabled"}`, "high", "low", ReasoningEffortOverLimitDeny, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_bps_effort", "gpt-6.1-sol")}
			svc := openAIClientToolsTestService(upstream)
			account := excelAccount()
			account.Extra["openai_excel_bps_models"] = []string{"gpt-6.1-sol"}
			body := []byte(`{"model":"gpt-6.1-sol","stream":false,"max_tokens":100,"messages":[{"role":"user","content":"hello"}]` + tc.preference + `}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			ctx := WithOpenAIReasoningEffortPolicyForMessages(c.Request.Context(), body, tc.ceiling, []ReasoningEffortMapping{{From: "none", To: tc.target}}, tc.overLimit)
			_, err := svc.ForwardAsAnthropic(ctx, c, account, body, "", "")
			if tc.status == 200 {
				require.NoError(t, err)
				require.Len(t, upstream.requests, 1)
				require.Equal(t, tc.target, gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
			} else {
				require.Error(t, err)
				require.Empty(t, upstream.requests)
			}
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
		})
	}
}

func TestExcelBPSCompatFailureIsNotSuccess(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		for _, status := range []int{200, 403} {
			t.Run(fmt.Sprintf("messages=%v/status=%d", anthropic, status), func(t *testing.T) {
				wire := `{"error":{"message":"denied","code":"denied"}}`
				if status == 200 {
					wire = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"denied\",\"code\":\"denied\"}}}\n\n"
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}}
				s := openAIClientToolsTestService(upstream)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
				forward := s.ForwardAsChatCompletions
				if anthropic {
					forward = s.ForwardAsAnthropic
				}
				_, err := forward(context.Background(), c, excelAccount(), []byte(`{"model":"gpt-6-astra","stream":true,"messages":[{"role":"user","content":"hi"}]}`), "", "")
				require.Error(t, err)
				require.Len(t, upstream.requests, 1)
				require.Contains(t, rec.Body.String(), "error")
				require.NotContains(t, rec.Body.String(), "message_stop")
				require.NotContains(t, rec.Body.String(), "[DONE]")
				if status != 200 {
					require.Equal(t, status, rec.Code)
				}
			})
		}
	}
}

func TestExcelBPSCompatDisabledDoesNotUseCodex(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		a := excelAccount()
		a.Extra["openai_excel_bps"] = false
		upstream := &httpUpstreamRecorder{}
		s := openAIClientToolsTestService(upstream)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
		forward := s.ForwardAsChatCompletions
		if anthropic {
			forward = s.ForwardAsAnthropic
		}
		_, err := forward(context.Background(), c, a, []byte(`{"model":"gpt-6-astra","messages":[]}`), "", "")
		require.ErrorIs(t, err, errExcelOAuthRouteUnavailable)
		require.Empty(t, upstream.requests)
		require.Equal(t, 400, c.Writer.Status())
	}
}

func TestExcelBPSAdminConnectionUsesGateway(t *testing.T) {
	for _, success := range []bool{true, false} {
		wire := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}\n\n"
		if !success {
			wire = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"message\":\"test failed\"}}}\n\n"
		}
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(wire))}}
		s := &AccountTestService{openaiGatewayService: openAIClientToolsTestService(upstream)}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/api/v1/admin/accounts/300/test", nil)
		err := s.testOpenAIAccountConnection(c, excelAccount(), "gpt-6-astra", "hi", "")
		require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
		if success {
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), `"success":true`)
			require.Contains(t, rec.Body.String(), "hello")
		} else {
			require.Error(t, err)
			require.NotContains(t, rec.Body.String(), `"success":true`)
		}
	}
}
