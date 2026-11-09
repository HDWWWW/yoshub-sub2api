//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillTimeoutCapturedBeforeCompletionDoesNotSend(t *testing.T) {
	for _, status := range []string{"imported", "delivery_expired", "dismissed", "awaiting_push"} {
		t.Run(status, func(t *testing.T) {
			s, store, _, _ := failureRefillHarness(t)
			seedStalledRefill(t, store, "blocked", time.Now().Add(-20*time.Minute))
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			state.EncryptedWebhook = "encrypted-fixture"
			n := &refillNoticeStub{}
			s.notifier = n
			notice := captureRefillTimeoutNotice(&state, time.Now())
			require.NotNil(t, notice)
			state.Jobs[0].Status = status
			if status == "awaiting_push" {
				state.Jobs[0].AccountID = 344
			}
			require.NoError(t, s.notifyRefillTimeoutRetry(context.Background(), &state, notice, time.Now(), func() error { return nil }))
			require.Empty(t, n.messages)
			require.Empty(t, state.TimeoutNoticeContent)
		})
	}
}

func TestRefillTimeoutQueuedRetryDropsResolvedJobs(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "all-completed", true: "one-still-pending"}[partial], func(t *testing.T) {
			s, store, _, _ := failureRefillHarness(t)
			seedStalledRefill(t, store, "blocked", time.Now().Add(-20*time.Minute))
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			state.EncryptedWebhook = "encrypted-fixture"
			if partial {
				other := state.Jobs[0]
				other.ID, other.SupplierID = "still-pending", "supplier-pending"
				state.Jobs = append(state.Jobs, other)
			}
			n := &refillNoticeStub{err: errors.New("offline")}
			s.notifier = n
			require.NoError(t, s.notifyRefillTimeoutRetry(context.Background(), &state, captureRefillTimeoutNotice(&state, time.Now()), time.Now(), func() error { return nil }))
			require.Len(t, n.messages, 1)
			state.Jobs[0].Status, state.Jobs[0].AccountID = "imported", 344
			state.TimeoutNoticeRetryAt = time.Now().Add(-time.Second)
			n.err = nil
			require.NoError(t, s.notifyRefillTimeoutRetry(context.Background(), &state, nil, time.Now(), func() error { return nil }))
			if partial {
				require.Len(t, n.messages, 2)
				require.NotContains(t, n.messages[1], "stalled")
				require.Contains(t, n.messages[1], "supplier-pending")
			} else {
				require.Len(t, n.messages, 1)
				require.Empty(t, state.TimeoutNoticeContent)
			}
			require.True(t, state.TimeoutNoticeRetryAt.IsZero())
		})
	}
}
