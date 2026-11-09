package handler

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
	"time"
)

func TestRecoveryBudgetPreservesAccountRetryZeroAndOne(t *testing.T) {
	for _, limit := range []int{0, 1, 3, 10} {
		t.Run(string(rune('A'+limit)), func(t *testing.T) {
			b := &openAI429Recovery{}
			now := time.Now()
			account := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
			err := &service.UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, RetryableOnSameAccount: true}
			for i := 0; i < 3; i++ {
				account.ID = int64(i + 1)
				require.True(t, b.allow(account, err, limit, limit, now))
			}
			require.False(t, b.allow(account, err, limit, limit, now))
			require.False(t, sameAccountRetryAllowed(err, limit, limit))
		})
	}
}

func TestRecoveryBudgetRejectsFullRetryAfterOutsideWindow(t *testing.T) {
	b := &openAI429Recovery{}
	now := time.Now()
	account := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	err := &service.UpstreamFailoverError{StatusCode: 429, RetryableOnSameAccount: true, SameAccountRetryDelay: 45 * time.Second}
	require.False(t, b.allow(account, err, 0, 3, now))
	require.Equal(t, 45*time.Second, err.SameAccountRetryDelay)
	ctx, cancel := b.selectionContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.Equal(t, now.Add(30*time.Second), deadline)
	b.reset()
	require.Nil(t, b.expired(now.Add(time.Hour)))
}

func TestRecoveryBudgetOnlyStartsForOAuth429(t *testing.T) {
	b := &openAI429Recovery{}
	now := time.Now()
	a := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	require.True(t, b.allow(a, &service.UpstreamFailoverError{StatusCode: 429}, 0, 0, now))
	require.True(t, b.deadline.IsZero())
	a.Type = service.AccountTypeOAuth
	require.True(t, b.allow(a, &service.UpstreamFailoverError{StatusCode: 503}, 0, 0, now))
	require.True(t, b.deadline.IsZero())
}
