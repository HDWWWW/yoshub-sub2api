//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testEarlyLimits = "event: codex.rate_limits\ndata: {\"type\":\"codex.rate_limits\",\"rate_limits\":{\"primary\":{\"used_percent\":12}}}\n\n"

func TestEarlyRateLimitsHappyAndTTFT(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, mode := range []string{OpenAITTFTModeSemantic, OpenAITTFTModeVisible} {
			for _, delta := range []bool{false, true} {
				t.Run(fmt.Sprintf("pass=%v/mode=%s/delta=%v", passthrough, mode, delta), func(t *testing.T) {
					gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: mode, expiresAt: time.Now().Add(time.Minute).UnixNano()})
					defer gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{openAITTFTMode: OpenAITTFTModeSemantic, expiresAt: time.Now().Add(time.Minute).UnixNano()})
					c, rec := happyTestContext(t)
					id := int64(7)
					c.Set("api_key", &APIKey{GroupID: &id})
					s := happyTestService(t, 35, delta)
					s.settingService.defaultSubGroupReader = refillTestGroups{}
					require.NoError(t, s.settingService.UpdateGroupFirstTokenSettings(context.Background(), id, GroupFirstTokenSettings{true}))
					rd, wr := io.Pipe()
					defer func() { _ = rd.Close() }()
					go func() {
						defer func() { _ = wr.Close() }()
						_, _ = io.WriteString(wr, testEarlyLimits)
						select {
						case <-rec.flushed:
						case <-time.After(time.Second):
							_ = wr.CloseWithError(fmt.Errorf("happy event missing"))
							return
						}
						time.Sleep(35 * time.Millisecond)
						_, _ = io.WriteString(wr, happySuccessStream)
					}()
					started := time.Now()
					result, err := happyRunStream(s, c, &http.Response{StatusCode: 200, Header: make(http.Header), Body: rd}, passthrough, started)
					require.NoError(t, err)
					require.NotNil(t, result.firstTokenMs)
					require.True(t, strings.HasPrefix(rec.Body.String(), testEarlyLimits), rec.Body.String())
					require.Equal(t, 1, strings.Count(rec.Body.String(), `"gateway_status"`))
					require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"codex.rate_limits"`))
					require.EqualValues(t, 2, result.usage.OutputTokens)
					if mode == OpenAITTFTModeVisible {
						require.GreaterOrEqual(t, *result.firstTokenMs, 60)
					} else {
						require.Less(t, *result.firstTokenMs, 35)
					}
				})
			}
		}
	}
}
func TestEarlyRateLimitsRetryAndFinalError(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			c, rec := happyTestContext(t)
			id := int64(7)
			c.Set("api_key", &APIKey{GroupID: &id})
			s := happyTestService(t, 60000)
			s.settingService.defaultSubGroupReader = refillTestGroups{}
			require.NoError(t, s.settingService.UpdateGroupFirstTokenSettings(context.Background(), id, GroupFirstTokenSettings{true}))
			payload := testEarlyLimits + "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"rate_limit_exceeded\",\"type\":\"rate_limit_error\",\"message\":\"Too many requests\"}}}\n\n"
			_, err := happyRunStream(s, c, &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, passthrough, time.Now())
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover)
			require.Equal(t, testEarlyLimits, rec.Body.String())
			require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
			writeOpenAIJSONError(c, 429, map[string]any{"error": map[string]string{"type": "rate_limit_error", "message": "retry exhausted"}})
			require.Equal(t, 200, rec.Code)
			require.Contains(t, rec.Body.String(), "event: response.failed")
			require.Contains(t, rec.Body.String(), "retry exhausted")
		})
	}
}
func TestEarlyRateLimitsFilterPreservesMalformedAndOrder(t *testing.T) {
	for _, raw := range []string{
		"event: codex.rate_limits\ndata: invalid\n\n", "event: codex.rate_limits\ndata: {\"type\":\"response.created\"}\n\n",
		"event: codex.rate_limits\ndata: " + strings.Repeat("x", openAIEarlyControlMaxBytes) + "\n\n",
		"data: {\"type\":\"response.created\"}\n\n" + testEarlyLimits,
	} {
		f := &openAIEarlyControlFilter{}
		var ordinary []string
		for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
			lines, event := f.Push(line)
			require.Empty(t, event)
			ordinary = append(ordinary, lines...)
		}
		ordinary = append(ordinary, f.Finish()...)
		require.Equal(t, raw, strings.Join(ordinary, "\n")+"\n")
	}
	f := &openAIEarlyControlFilter{}
	_, _ = f.Push("event: codex.rate_limits")
	require.Equal(t, []string{"event: codex.rate_limits"}, f.Finish())
}
func TestGroupFirstTokenSettingsPersistenceAndIsolation(t *testing.T) {
	repo := &happySettingsRepo{}
	s := &SettingService{settingRepo: repo, defaultSubGroupReader: refillTestGroups{}}
	require.False(t, s.groupEarlyRateLimitsEnabled(context.Background(), 7))
	require.NoError(t, s.UpdateGroupFirstTokenSettings(context.Background(), 7, GroupFirstTokenSettings{true}))
	require.True(t, s.groupEarlyRateLimitsEnabled(context.Background(), 7))
	require.False(t, s.groupEarlyRateLimitsEnabled(context.Background(), 8))
	reloaded := &SettingService{settingRepo: repo}
	require.True(t, reloaded.groupEarlyRateLimitsEnabled(context.Background(), 7))
	require.NoError(t, s.UpdateGroupFirstTokenSettings(context.Background(), 7, GroupFirstTokenSettings{}))
	require.False(t, s.groupEarlyRateLimitsEnabled(context.Background(), 7))
}
func TestCyberEntitlementClassification(t *testing.T) {
	body := []byte(`{"error":{"code":"permission_denied","message":"` + openAICyberAccessDeniedMessage + `"}}`)
	require.True(t, isOpenAICyberAccessDenied(403, body))
	require.False(t, isOpenAICyberAccessDenied(401, body))
	require.False(t, isOpenAICyberAccessDenied(403, []byte(strings.Replace(string(body), "permission_denied", "invalid_api_key", 1))))
	require.False(t, isOpenAICyberAccessDenied(403, []byte(`{"input":"`+openAICyberAccessDeniedMessage+`"}`)))
}
func TestWSProxyClientCacheConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{{"16", 16}, {"0", 256}, {"bad", 256}, {"20000", 256}} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("OPENAI_WS_PROXY_CLIENT_CACHE_MAX_ENTRIES", tc.value)
			d := newDefaultOpenAIWSClientDialer().(*coderOpenAIWSClientDialer)
			require.Equal(t, tc.want, d.maxProxyClients)
		})
	}
}

func TestEarlyRateLimitsSubsequentHTTPResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		terminal string
	}{
		{"permission", 403, `{"error":{"type":"permission_error","message":"` + openAICyberAccessDeniedMessage + `"}}`, "response.failed"},
		{"generic_error", 400, `{"error":{"code":"bad_request","message":"bad history"}}`, "response.failed"},
		{"success", 200, `{"id":"resp_ok","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":3}}`, "response.completed"},
		{"incomplete", 200, `{"id":"resp_ok","status":"incomplete","output":[],"usage":{"input_tokens":10,"output_tokens":3}}`, "response.incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := happyTestContext(t)
			c.Header("Content-Type", "text/event-stream")
			require.NoError(t, writeOpenAIEarlyControlFrame(c, testEarlyLimits))
			require.True(t, writeOpenAICompactSSEBridge(c, tc.status, []byte(tc.body)))
			require.Equal(t, 200, rec.Code)
			require.True(t, strings.HasPrefix(rec.Body.String(), testEarlyLimits))
			require.Contains(t, rec.Body.String(), "event: "+tc.terminal+"\n")
			if tc.status == 200 {
				require.Contains(t, rec.Body.String(), `"output_tokens":3`)
				require.NotContains(t, rec.Body.String(), "response.failed")
			} else {
				require.NotContains(t, rec.Body.String(), "response.completed")
			}
		})
	}
}
