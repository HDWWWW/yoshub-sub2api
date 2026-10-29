//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAntigravitySingleAccountRetriesRespectAccountBudget(t *testing.T) {
	for _, count := range []int{0, 2} {
		upstream := &mockSmartRetryUpstream{}
		for i := 0; i < count; i++ {
			upstream.responses = append(upstream.responses, &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":503}}`)),
			})
			upstream.errors = append(upstream.errors, nil)
		}
		account := &Account{ID: 100, Type: AccountTypeOAuth, Platform: PlatformAntigravity, Extra: map[string]any{"same_account_retry_count": count}}
		params := antigravityRetryLoopParams{ctx: context.Background(), account: account, accessToken: "test", action: "generateContent", body: []byte(`{}`), httpUpstream: upstream}
		service := &AntigravityGatewayService{}
		result := service.handleSingleAccountRetryInPlace(params, &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}}, []byte(`{"error":{"code":503}}`), "https://ag.test", time.Millisecond, "test-model")
		require.NotNil(t, result.resp)
		require.Len(t, upstream.calls, count)
	}
}
