//go:build unit

package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHandleSmartRetry_LastRetryInfoControlsCooldown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		delay  time.Duration
		budget int
		single bool
	}{
		{"short_reset_on_final_retry", 5 * time.Second, 1, false},
		{"long_reset_on_final_retry", time.Minute, 1, false},
		{"long_reset_with_retries_remaining", time.Minute, 3, false},
		{"single_account_503_with_long_reset", time.Minute, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, ctx := http.StatusTooManyRequests, context.Background()
			if tc.single {
				status, ctx = http.StatusServiceUnavailable, ctxWithSingleAccountRetry()
			}
			makeBody := func(d string) string {
				return fmt.Sprintf(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3-flash"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":%q}]}}`, d)
			}
			first := []byte(makeBody("1s"))
			upstream := &mockSmartRetryUpstream{
				responses: []*http.Response{{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(makeBody(tc.delay.String())))}},
				errors:    []error{nil},
			}
			repo := &stubAntigravityAccountRepo{}
			account := &Account{ID: 2, Type: AccountTypeOAuth, Platform: PlatformAntigravity, Extra: map[string]any{"same_account_retry_count": tc.budget}}
			params := antigravityRetryLoopParams{
				ctx: ctx, prefix: "[test]", account: account,
				accessToken: "synthetic-test-token", action: "generateContent", body: []byte(`{"input":"test"}`),
				httpUpstream: upstream, accountRepo: repo,
			}
			resp := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(first))}
			start := time.Now()
			result := (&AntigravityGatewayService{}).handleSmartRetry(params, resp, first, "https://ag-1.test", 0, []string{"https://ag-1.test"})
			end := time.Now()
			require.NotNil(t, result)
			require.Len(t, upstream.calls, 1, "the final reset must not cause another retry")
			if tc.single {
				require.Nil(t, result.switchError)
				require.NotNil(t, result.resp)
				require.Equal(t, status, result.resp.StatusCode)
				body, err := io.ReadAll(result.resp.Body)
				require.NoError(t, err)
				require.NoError(t, result.resp.Body.Close())
				require.JSONEq(t, makeBody(tc.delay.String()), string(body))
				require.Empty(t, repo.modelRateLimitCalls)
				return
			}
			require.NotNil(t, result.switchError)
			require.Len(t, repo.modelRateLimitCalls, 2)
			for _, call := range repo.modelRateLimitCalls {
				require.False(t, call.resetAt.Before(start.Add(tc.delay)), "cooldown must honor the final response")
				require.False(t, call.resetAt.After(end.Add(tc.delay)), "cooldown must not exceed the final delay")
			}
		})
	}
}
