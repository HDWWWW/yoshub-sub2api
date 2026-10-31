package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type delayedUsageProbeRepo struct {
	AccountRepository
	entered chan struct{}
	release chan struct{}
	failure error
}

func (r *delayedUsageProbeRepo) UpdateExtra(ctx context.Context, _ int64, _ map[string]any) error {
	close(r.entered)
	select {
	case <-r.release:
		return r.failure
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestQuotaProbeWaitsForDurableSnapshotBeforeReturning(t *testing.T) {
	for _, fail := range []bool{false, true} {
		r := &delayedUsageProbeRepo{entered: make(chan struct{}), release: make(chan struct{})}
		if fail {
			r.failure = errors.New("database write failed")
		}
		s := &AccountUsageService{accountRepo: r}
		done := make(chan error, 1)
		go func() { done <- s.persistOpenAICodexProbeSnapshot(39, map[string]any{"codex_7d_used_percent": 97.0}) }()
		select {
		case <-r.entered:
		case <-time.After(time.Second):
			t.Fatal("write did not start")
		}
		select {
		case <-done:
			t.Fatal("returned quota before its local snapshot was durable")
		case <-time.After(30 * time.Millisecond):
		}
		close(r.release)
		select {
		case err := <-done:
			require.Equal(t, r.failure, err)
		case <-time.After(time.Second):
			t.Fatal("write did not finish")
		}
	}
}
