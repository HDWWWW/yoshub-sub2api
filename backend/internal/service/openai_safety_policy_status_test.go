//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesSafetyPolicyStopsBeforeCompatibilityRetry(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		for _, passthrough := range []bool{false, true} {
			for _, fixture := range []struct {
				status  int
				message string
			}{
				{400, "blocked by upstream policy"},
				{403, "blocked by upstream policy"},
				{429, "blocked by upstream policy"},
				{502, "blocked by upstream policy"},
				{400, "context window exceeded"},
			} {
				t.Run(fmt.Sprintf("%s/passthrough=%t/%d/%s", code, passthrough, fixture.status, fixture.message), func(t *testing.T) {
					payload := fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, fixture.message)
					upstream := &httpUpstreamRecorder{responses: []*http.Response{
						{StatusCode: fixture.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(payload))},
						{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(`{"id":"unexpected_retry","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))},
					}}
					svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "gpt-5.4"}}, httpUpstream: upstream}
					account := compatCyberOAuthAccount()
					account.Extra = map[string]any{"openai_passthrough": passthrough}
					c, rec := newNonStreamingFailoverContext(t)
					MarkOpenAINativeCompactionV2(c)
					body := []byte(`{"model":"gpt-5.5","stream":false,"input":[{"type":"message","role":"user","content":"hello"},{"type":"compaction_trigger"}]}`)
					result, err := svc.Forward(c.Request.Context(), c, account, body)
					require.Error(t, err)
					require.Nil(t, result)
					require.Len(t, upstream.requests, 1, "a policy refusal must stop before retry or compact model fallback")
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
					wantStatus := http.StatusBadRequest
					if fixture.status == http.StatusForbidden {
						wantStatus = fixture.status
					}
					require.Equal(t, wantStatus, rec.Code)
					require.Equal(t, code, gjson.Get(rec.Body.String(), "error.code").String())
					require.Equal(t, code, GetOpsCyberPolicy(c).Code)
					require.Equal(t, fixture.status, GetOpsCyberPolicy(c).UpstreamStatus)
				})
			}
		}
	}
}

func TestResponsesSafetyPolicyBufferedStatus(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		for _, passthrough := range []bool{false, true} {
			for _, compact := range []bool{false, true} {
				for _, fixture := range []struct {
					name, body, contentType   string
					inputTokens, outputTokens int
				}{
					{"json_error_without_usage", fmt.Sprintf(`{"error":{"code":%q,"message":"blocked by upstream policy"}}`, code), "application/json", 0, 0},
					{"json_failed_with_usage", fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":{"code":%q,"message":"blocked by upstream policy"},"usage":{"input_tokens":11,"output_tokens":3}}}`, code), "application/json", 11, 3},
					{"sse_failed", "data: " + fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":{"code":%q,"status":502,"message":"blocked by upstream policy"},"usage":{"input_tokens":11,"output_tokens":3}}}`, code) + "\n\n", "text/event-stream", 11, 3},
					{"sse_error_after_usage", "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":11,\"output_tokens\":3}}}\n\ndata: " + fmt.Sprintf(`{"type":"error","error":{"code":%q,"status":429,"message":"blocked by upstream policy"}}`, code) + "\n\n", "text/event-stream", 11, 3},
				} {
					t.Run(fmt.Sprintf("%s/%s/passthrough=%t/compact=%t", code, fixture.name, passthrough, compact), func(t *testing.T) {
						c, rec := newCompactBridgeTestContext(t, compact)
						if compact {
							stop := StartOpenAICompactSSEKeepalive(c, keepaliveTestInterval)
							defer stop()
							waitForKeepaliveBeats()
						}
						resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {fixture.contentType}}, Body: io.NopCloser(bytes.NewBufferString(fixture.body))}
						repo := &openAIAuthPolicyAccountRepo{}
						svc := &OpenAIGatewayService{cfg: &config.Config{}}
						svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
						var err error
						if passthrough {
							result, forwardErr := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, compatCyberOAuthAccount(), "gpt-5.5", "gpt-5.5")
							require.Nil(t, result)
							err = forwardErr
						} else {
							result, forwardErr := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, compatCyberOAuthAccount(), "gpt-5.5", "gpt-5.5")
							require.Nil(t, result)
							err = forwardErr
						}
						require.ErrorIs(t, err, errOpenAICyberPolicyForwarded)
						mark := GetOpsCyberPolicy(c)
						require.NotNil(t, mark)
						require.Equal(t, code, mark.Code)
						require.Equal(t, http.StatusOK, mark.UpstreamStatus)
						require.Equal(t, fixture.inputTokens, mark.UpstreamInTok)
						require.Equal(t, fixture.outputTokens, mark.UpstreamOutTok)
						require.Zero(t, repo.tempCalls)
						require.Zero(t, repo.setErrorCalls)
						if compact {
							require.Equal(t, http.StatusOK, rec.Code)
							events := parseCompactBridgeSSE(t, stripKeepaliveComments(rec.Body.String()))
							require.Len(t, events, 1)
							require.Equal(t, "response.failed", events[0][0])
							require.Equal(t, code, gjson.Get(events[0][1], "response.error.code").String())
						} else {
							require.Equal(t, http.StatusBadRequest, rec.Code)
							require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
							require.Equal(t, code, gjson.Get(rec.Body.String(), "error.code").String())
						}
					})
				}
			}
		}
	}
}

func TestSmartWSSafetyPolicyHandshakeHTTPStatus(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		for _, status := range []int{400, 401, 403, 429, 502, 529} {
			t.Run(fmt.Sprintf("%s/%d", code, status), func(t *testing.T) {
				body := fmt.Sprintf(`{"error":{"code":%q,"message":"blocked by upstream policy"}}`, code)
				svc, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, body)
				})
				repo := &rateLimitAccountRepoStub{}
				svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
				c, rec := forcedWSContext(t, "/v1/responses", 1, "policy-handshake")
				_, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","input":"hello","stream":true}`))
				wantStatus := http.StatusBadRequest
				if status == http.StatusForbidden {
					wantStatus = status
				}
				require.Equal(t, wantStatus, rec.Code)
				require.JSONEq(t, body, rec.Body.String())
				require.Equal(t, status, GetOpsCyberPolicy(c).UpstreamStatus)
				require.Zero(t, upstream.callCount, "policy refusal must not trigger HTTP replay")
				require.Zero(t, repo.tempCalls)
				require.Zero(t, repo.setErrorCalls)
				var stop *UpstreamFailoverError
				require.ErrorAs(t, err, &stop)
				require.Equal(t, status, stop.StatusCode, "retain original upstream diagnostics")
				require.False(t, stop.ShouldRetryNextAccount())
				require.False(t, stop.RetryableOnSameAccount)
				require.False(t, stop.ShouldReportAccountScheduleFailure())
			})
		}
	}
}

func TestSmartWSSafetyPolicyFailedEventHTTPStatus(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		for _, stream := range []bool{false, true} {
			for _, prelude := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/prelude=%t", code, stream, prelude), func(t *testing.T) {
					payload := fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":{"code":%q,"status":502,"message":"blocked by upstream policy"},"usage":{"input_tokens":11,"output_tokens":3}}}`, code)
					svc, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
						conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer conn.Close()
						_, _, _ = conn.ReadMessage()
						if prelude {
							_ = conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated))
						}
						_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
						_, _, _ = conn.ReadMessage()
					})
					c, rec := forcedWSContext(t, "/v1/responses", 1, "policy-event")
					_, err := svc.Forward(c.Request.Context(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","input":"hello","stream":%t}`, stream)))
					require.Error(t, err)
					wantStatus := http.StatusBadRequest
					if stream && prelude {
						wantStatus = http.StatusOK
					}
					require.Equal(t, wantStatus, rec.Code)
					require.Contains(t, rec.Body.String(), fmt.Sprintf(`"code":%q`, code))
					mark := GetOpsCyberPolicy(c)
					require.NotNil(t, mark)
					require.Equal(t, code, mark.Code)
					require.Equal(t, 11, mark.UpstreamInTok)
					require.Equal(t, 3, mark.UpstreamOutTok)
					require.Zero(t, upstream.callCount)
				})
			}
		}
	}
}

func TestSmartWSSafetyPolicyHTTPAfterCompactKeepalive(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		t.Run(code, func(t *testing.T) {
			c, rec := newCompactBridgeTestContext(t, true)
			stop := StartOpenAICompactSSEKeepalive(c, keepaliveTestInterval)
			defer stop()
			waitForKeepaliveBeats()
			body := []byte(fmt.Sprintf(`{"error":{"code":%q,"message":"blocked by upstream policy"}}`, code))
			resp := openAIHTTPWSErrorResponse(c.Request, http.StatusBadGateway, nil, body)
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			err := svc.writeSmartWSHTTPError(c, compatCyberOAuthAccount(), resp, body)
			require.Error(t, err)
			require.Equal(t, http.StatusOK, rec.Code)
			events := parseCompactBridgeSSE(t, stripKeepaliveComments(rec.Body.String()))
			require.Len(t, events, 1)
			require.Equal(t, "response.failed", events[0][0])
			require.Equal(t, code, gjson.Get(events[0][1], "response.error.code").String())
			require.Equal(t, http.StatusBadGateway, GetOpsCyberPolicy(c).UpstreamStatus)
		})
	}
}

func TestSmartWSSafetyPolicyNativeHandshakeMarksRefusal(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		t.Run(code, func(t *testing.T) {
			c, _ := newNonStreamingFailoverContext(t)
			repo := &openAIAuthPolicyAccountRepo{}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			svc.rateLimitService = NewRateLimitService(repo, nil, svc.cfg, nil, nil)
			body := []byte(fmt.Sprintf(`{"error":{"code":%q,"message":"blocked by upstream policy"}}`, code))
			var delivered []byte
			err := svc.smartWSNativeHandshakeFailure(context.Background(), c, compatCyberOAuthAccount(), "gpt-5.5",
				&openAIWSDialError{StatusCode: http.StatusTooManyRequests, ResponseBody: body}, func(payload []byte) error {
					delivered = payload
					return nil
				})
			require.Equal(t, code, gjson.GetBytes(delivered, "error.code").String())
			require.Equal(t, code, GetOpsCyberPolicy(c).Code)
			require.Equal(t, http.StatusTooManyRequests, GetOpsCyberPolicy(c).UpstreamStatus)
			require.Zero(t, repo.tempCalls)
			require.Zero(t, repo.setErrorCalls)
			var stop *UpstreamFailoverError
			require.ErrorAs(t, err, &stop)
			require.False(t, stop.ShouldRetryNextAccount())
			require.False(t, stop.ShouldReportAccountScheduleFailure())
		})
	}
}

func TestSmartWSSafetyPolicyDoesNotCountAsAccountHealthFailure(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		for _, status := range []int{429, 502} {
			payload := []byte(fmt.Sprintf(`{"type":"response.failed","response":{"error":{"code":%q,"message":"blocked by upstream policy"}}}`, code))
			failure := smartWSStopFailure(status, payload, nil)
			require.False(t, failure.ShouldRetryNextAccount())
			require.False(t, failure.RetryableOnSameAccount)
			_, _, eligible := classifyOpenAIAPIKeyHealthFailure(failure)
			require.False(t, eligible, "policy refusals must not trip the shared account health breaker")
		}
	}
}
