package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDetectOpenAISafetyPolicyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		body string
		code string
	}{
		{`{"error":{"code":"bio_policy","message":"blocked"}}`, OpenAIPolicyBio},
		{`{"response":{"error":{"code":" BIO_POLICY ","message":"blocked"}}}`, OpenAIPolicyBio},
		{`{"error":{"code":"cyber_policy","message":"blocked"}}`, OpenAIPolicyCyber},
		{`{"error":{"message":"This content was flagged for possible biological risk."}}`, ""},
		{`{"error":{"code":"bio_policy_unknown"}}`, ""},
		{`{"access_programs":{"cyber":true,"bio":true}}`, ""},
	} {
		hit, code, _ := detectOpenAICyberPolicy([]byte(tc.body))
		require.Equal(t, tc.code != "", hit, tc.body)
		require.Equal(t, tc.code, code, tc.body)
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	MarkOpsCyberPolicy(c, CyberPolicyMark{Code: "unknown"})
	require.Nil(t, GetOpsCyberPolicy(c))
	MarkOpsCyberPolicy(c, CyberPolicyMark{Code: OpenAIPolicyBio})
	MarkOpsCyberPolicy(c, CyberPolicyMark{Code: OpenAIPolicyCyber})
	require.Equal(t, OpenAIPolicyBio, GetOpsCyberPolicy(c).Code)
	ClearOpsCyberPolicy(c)
	MarkOpsCyberPolicy(c, CyberPolicyMark{Code: OpenAIPolicyCyber})
	require.Equal(t, OpenAIPolicyCyber, GetOpsCyberPolicy(c).Code)
}

func TestForwardCompatSafetyPolicies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				for _, upstreamStatus := range []int{http.StatusOK, http.StatusBadGateway} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/upstream=%d", code, path, stream, upstreamStatus), func(t *testing.T) {
						wire := fmt.Sprintf(`{"error":{"type":"invalid_request_error","code":%q,"message":"blocked by upstream policy"}}`, code)
						contentType := "application/json"
						if upstreamStatus == http.StatusOK {
							wire = "data: " + fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_policy","object":"response","model":"gpt-5.5","status":"failed","output":[],"error":{"code":%q,"message":"blocked by upstream policy"},"usage":{"input_tokens":11,"output_tokens":3}}}`, code) + "\n\n"
							contentType = "text/event-stream"
						}
						upstream := &httpUpstreamRecorder{resp: &http.Response{
							StatusCode: upstreamStatus,
							Header:     http.Header{"Content-Type": []string{contentType}},
							Body:       io.NopCloser(strings.NewReader(wire)),
						}}
						repo := &openAIAuthPolicyAccountRepo{}
						svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
						svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
						body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_tokens":128,"messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream))
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
						account := compatCyberOAuthAccount()
						var result *OpenAIForwardResult
						var err error
						if path == "/v1/messages" {
							result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.5")
						} else {
							result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.5")
						}
						require.Error(t, err)
						require.Nil(t, result, "handler must record policy usage exactly once through the marked-error path")
						var failover *UpstreamFailoverError
						require.False(t, errors.As(err, &failover))
						require.Len(t, upstream.requests, 1, "policy refusals must not be retried")
						require.Zero(t, repo.tempCalls)
						require.Zero(t, repo.setErrorCalls)
						require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
						mark := GetOpsCyberPolicy(c)
						require.NotNil(t, mark)
						require.Equal(t, code, mark.Code)
						require.Equal(t, upstreamStatus, mark.UpstreamStatus)
						require.Contains(t, rec.Body.String(), fmt.Sprintf(`"code":%q`, code))
						require.Contains(t, rec.Body.String(), "blocked by upstream policy")
						if upstreamStatus == http.StatusOK {
							require.Equal(t, 11, mark.UpstreamInTok, "retain reported usage for existing billing")
							require.Equal(t, 3, mark.UpstreamOutTok)
						}
						if stream && upstreamStatus == http.StatusOK {
							require.Equal(t, http.StatusOK, rec.Code)
							if path == "/v1/messages" {
								require.Contains(t, rec.Body.String(), "event: error")
							} else {
								require.Contains(t, rec.Body.String(), "data: [DONE]")
							}
						} else {
							require.Equal(t, http.StatusBadRequest, rec.Code)
							require.Equal(t, code, gjson.Get(rec.Body.String(), "error.code").String())
						}
					})
				}
			}
		}
	}
}

func TestResponsesSafetyPolicyHTTPError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		for _, status := range []int{400, 403, 429, 502} {
			for _, passthrough := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%d/passthrough=%t", code, status, passthrough), func(t *testing.T) {
					body := []byte(fmt.Sprintf(`{"error":{"type":"invalid_request_error","code":%q,"message":"blocked by upstream policy"}}`, code))
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					resp := &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
					repo := &openAIAuthPolicyAccountRepo{}
					svc := &OpenAIGatewayService{cfg: &config.Config{}}
					svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
					account := compatCyberOAuthAccount()
					require.False(t, svc.shouldFailoverOpenAIUpstreamResponse(account, status, "wrapped failure", body))
					require.False(t, shouldFailoverOpenAIPassthroughResponse(account, status, body))
					var err error
					if passthrough {
						err = svc.handleErrorResponsePassthrough(context.Background(), resp, c, account, nil, body)
					} else {
						_, err = svc.handleErrorResponse(context.Background(), resp, c, account, nil)
					}
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
					wantStatus := http.StatusBadRequest
					if status == http.StatusForbidden {
						wantStatus = status
					}
					require.Equal(t, wantStatus, rec.Code)
					require.Equal(t, code, gjson.Get(rec.Body.String(), "error.code").String())
					require.Equal(t, status, GetOpsCyberPolicy(c).UpstreamStatus)
					require.Zero(t, repo.tempCalls)
					require.Zero(t, repo.setErrorCalls)
					require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
				})
			}
		}
	}
}

func TestRecordBioPolicyEventUsesExistingControls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   string
		config    string
		logOnly   bool
		wantLogs  int
		wantCount bool
	}{
		{"enabled", "true", "{}", false, 1, true},
		{"excluded from ban count", "true", `{"cyber_policy_exclude_from_ban_count":true}`, false, 1, false},
		{"trusted user", "true", "{}", true, 1, false},
		{"risk control disabled", "false", "{}", false, 0, false},
		{"model excluded", "true", `{"model_filter":{"type":"include","models":["other"]}}`, false, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &banCountArgsTestRepo{}
			svc := NewContentModerationService(&contentModerationTestSettingRepo{values: map[string]string{
				SettingKeyRiskControlEnabled:      tc.enabled,
				SettingKeyContentModerationConfig: tc.config,
			}}, repo, nil, nil, nil, nil, nil, nil)
			svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{
				PolicyCode: OpenAIPolicyBio, UserID: 1, Model: "gpt-5", LogOnly: tc.logOnly,
				UpstreamMessage: "blocked by biological safety policy",
			})
			logs := repo.snapshotLogs()
			require.Len(t, logs, tc.wantLogs)
			if tc.wantCount {
				require.Equal(t, []bool{false}, repo.snapshotCountCalls())
				require.Greater(t, logs[0].ViolationCount, 0)
			} else {
				require.Empty(t, repo.snapshotCountCalls())
			}
			if tc.wantLogs > 0 {
				require.Equal(t, ContentModerationActionBioPolicy, logs[0].Action)
				require.Equal(t, OpenAIPolicyBio, logs[0].HighestCategory)
				require.False(t, logs[0].EmailSent)
				if tc.logOnly {
					require.Equal(t, ContentModerationModeCyberLogOnly, logs[0].Mode)
				}
				if !tc.wantCount {
					require.Zero(t, logs[0].ViolationCount)
					require.False(t, logs[0].AutoBanned)
				}
				html := buildCyberPolicyNoticeEmailBody("Test", &logs[0])
				require.Contains(t, html, "生物安全策略")
				require.Contains(t, html, "bio_policy")
				require.NotContains(t, html, "网络安全策略")
			}
		})
	}
}

func TestBioPolicyErrorClassification(t *testing.T) {
	require.Equal(t, "bio", MapUserErrorCategory("request", OpenAIPolicyBio))
	phases, types := CategoryToFilter("bio")
	require.Equal(t, []string{"request"}, phases)
	require.Equal(t, []string{OpenAIPolicyBio}, types)
	for _, status := range []int{200, 400, 429, 502} {
		require.Equal(t, "content_policy", ClassifyChannelMonitorV2Error(ChannelMonitorV2ErrorInput{
			ErrorType: OpenAIPolicyBio, StatusCode: status, UpstreamStatusCode: status,
		}))
	}
}

func TestCompactSafetyPolicyAfterKeepalivePreservesErrorCode(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		for _, passthrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/passthrough=%t", code, passthrough), func(t *testing.T) {
				c, rec := newCompactBridgeTestContext(t, true)
				stop := StartOpenAICompactSSEKeepalive(c, keepaliveTestInterval)
				defer stop()
				waitForKeepaliveBeats()
				body := []byte(fmt.Sprintf(`{"error":{"code":%q,"message":"blocked by upstream policy"}}`, code))
				resp := &http.Response{StatusCode: 502, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
				svc := &OpenAIGatewayService{cfg: &config.Config{}}
				account := compatCyberOAuthAccount()
				var err error
				if passthrough {
					err = svc.handleErrorResponsePassthrough(context.Background(), resp, c, account, nil, body)
				} else {
					_, err = svc.handleErrorResponse(context.Background(), resp, c, account, nil)
				}
				require.Error(t, err)
				require.Equal(t, 200, rec.Code)
				events := parseCompactBridgeSSE(t, stripKeepaliveComments(rec.Body.String()))
				require.Len(t, events, 1)
				require.Equal(t, "response.failed", events[0][0])
				require.Equal(t, code, gjson.Get(events[0][1], "response.error.code").String())
				require.Equal(t, code, GetOpsCyberPolicy(c).Code)
			})
		}
	}
}

func TestBioPolicyNoticeTemplatesPreserveSubtype(t *testing.T) {
	svc := NewNotificationEmailService(newNotificationEmailMemorySettingRepo(), nil)
	for _, locale := range []string{"en", "zh"} {
		preview, err := svc.PreviewTemplate(context.Background(), NotificationEmailPreviewInput{
			Event: NotificationEmailEventBioPolicyNotice, Locale: locale,
			Variables: map[string]string{"upstream_message": "<script>untrusted</script>"},
		})
		require.NoError(t, err)
		require.Contains(t, preview.HTML, "bio_policy")
		require.NotContains(t, preview.HTML, "cyber_policy")
		require.NotContains(t, preview.HTML, "<script>")
		require.Contains(t, preview.HTML, "&lt;script&gt;")
	}
}
