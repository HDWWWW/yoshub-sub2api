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
)

func TestHappyFirstTokenUpstreamKeepalivePreservesNotificationAndRetry(t *testing.T) {
	for _, delta := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			for _, passthrough := range []bool{false, true} {
				t.Run(fmt.Sprintf("delta=%t/partial_frame=%t/passthrough=%t", delta, partial, passthrough), func(t *testing.T) {
					c, rec := happyTestContext(t)
					svc := happyTestService(t, 30, delta)
					pr, pw := io.Pipe()
					t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
					go func() {
						defer pw.Close()
						preamble := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed_attempt\"}}\n\n"
						keepalive := upstreamKeepaliveEvent
						if partial {
							keepalive = "event: keepalive\n"
						}
						if _, err := io.WriteString(pw, preamble+keepalive); err != nil {
							return
						}
						select {
						case <-rec.flushed:
						case <-time.After(2 * time.Second):
							_ = pw.CloseWithError(errors.New("upstream keepalive canceled the happy-first notification"))
							return
						}
						if partial {
							_, _ = io.WriteString(pw, upstreamKeepaliveData+"\n\n")
						}
						_, _ = io.WriteString(pw, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"overloaded\"}}}\n\n")
					}()
					first := &http.Response{StatusCode: http.StatusOK,
						Header: http.Header{"X-Codex-Turn-State": {"failed-state"}, "X-Request-Id": {"failed-request"}}, Body: pr}
					result, err := happyRunStream(svc, c, first, passthrough, time.Now())
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.NotNil(t, result)
					require.Nil(t, result.firstTokenMs, "control frames must not count as model TTFT")
					require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
					expectedSignal := openAIHappyFirstTokenEvent
					if delta {
						expectedSignal = openAIHappyFirstTokenDeltaEvent
					}
					expected := upstreamKeepaliveEvent + expectedSignal
					if partial {
						expected = expectedSignal + upstreamKeepaliveEvent
					}
					require.Equal(t, expected, rec.Body.String(), "control frames remain whole and the failed attempt stays private")
					require.Empty(t, rec.Result().Header.Get("X-Codex-Turn-State"))
					require.Empty(t, rec.Result().Header.Get("X-Request-Id"))

					second := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(happySuccessStream))}
					result, err = happyRunStream(svc, c, second, passthrough, time.Now())
					require.NoError(t, err)
					require.EqualValues(t, 5, result.usage.InputTokens)
					require.EqualValues(t, 2, result.usage.OutputTokens)
					require.Equal(t, 1, strings.Count(rec.Body.String(), `"gateway_status":"upstream_connected"`))
					require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.completed"`))
					require.NotContains(t, rec.Body.String(), "resp_failed_attempt")
					require.NotContains(t, rec.Body.String(), "response.failed")
				})
			}
		}
	}
}

func TestHappyFirstTokenLocalKeepalivePreservesNotification(t *testing.T) {
	c, rec := happyTestContext(t)
	svc := happyTestService(t, 1300)
	svc.cfg.Gateway.StreamKeepaliveInterval = 1
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: happyBodyAfterSignal(t, rec, happySuccessStream, 0)}
	result, err := happyRunStream(svc, c, resp, false, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result.firstTokenMs)
	require.True(t, strings.HasPrefix(rec.Body.String(), ":\n\n"), "local keepalive precedes the delayed notification")
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"gateway_status":"upstream_connected"`))
	require.Contains(t, rec.Body.String(), `"type":"response.completed"`)
}

func TestOpenAIKeepaliveHeaderCannotHideModelOutput(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			c, rec := happyTestContext(t)
			svc := happyTestService(t, 60000)
			stream := "event: keepalive\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"visible\"}\n\n" + preContentFailoverFailed
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}
			_, err := happyRunStream(svc, c, resp, passthrough, time.Now())
			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "a mislabeled SSE header must not permit replay after visible output")
			require.Contains(t, rec.Body.String(), `"delta":"visible"`)
			require.Greater(t, OpenAICompactKeepaliveAdjustedWrittenSize(c), 0)
		})
	}
}

func TestOpenAINativeKeepaliveFrameBoundBeforeOutput(t *testing.T) {
	svc := newKeepaliveForwardService(0)
	c, rec := newKeepaliveForwardContext()
	frame := "event: keepalive\ndata: {\"type\":\"keepalive\",\"padding\":\"" + strings.Repeat("x", openAIEarlyControlMaxBytes) + "\"}\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(frame))}
	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, happyAccount(), time.Now(), "model", "model")
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Empty(t, rec.Body.String(), "an oversized control frame must not be partially committed")
}

func TestOpenAIKeepaliveDoesNotDetachFromOrdinaryEventHeader(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			c, rec := happyTestContext(t)
			svc := happyTestService(t, 60000)
			frame := "event: response.created\ndata: {\"type\":\"keepalive\"}\n\n"
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(frame + happySuccessStream))}
			_, err := happyRunStream(svc, c, resp, passthrough, time.Now())
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(rec.Body.String(), frame), "a data-only keepalive must not escape its existing event header")
			require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"keepalive"`))
		})
	}
}
