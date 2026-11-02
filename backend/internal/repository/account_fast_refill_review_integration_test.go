//go:build integration

package repository

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRefillReviewPostgresSerializesWorkersAcrossRules(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := &accountRefillStore{db: integrationDB, groupID: 970001}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM settings WHERE key=$1", store.key())
	})
	go func() {
		done <- store.WithLock(ctx, func(_ string, save func(string) error) error {
			if err := save(`{"replacement_job_id":"durable-order"}`); err != nil {
				close(entered)
				return err
			}
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			other := &accountRefillStore{db: integrationDB, groupID: int64(970001 + i%2)}
			errs <- other.WithLock(ctx, func(string, func(string) error) error {
				return fmt.Errorf("a second worker entered the reservation callback")
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	close(release)
	require.NoError(t, <-done)
	for err := range errs {
		require.ErrorIs(t, err, service.ErrAccountRefillBusy)
	}
	// A new worker reads the committed linkage after the owner releases its lock.
	restarted := &accountRefillStore{db: integrationDB, groupID: 970001}
	require.NoError(t, restarted.WithLock(ctx, func(raw string, _ func(string) error) error {
		require.JSONEq(t, `{"replacement_job_id":"durable-order"}`, raw)
		return nil
	}))
}

func TestAccountForceFastReviewPostgresConcurrentExtraUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	account := &service.Account{Name: fmt.Sprintf("fast-review-%d", time.Now().UnixNano()), Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"access_token": "fixture"}, Extra: map[string]any{"preserved": "original"}}
	require.NoError(t, repo.Create(ctx, account))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM scheduler_outbox WHERE account_id=$1", account.ID)
		_, _ = integrationDB.ExecContext(context.Background(), "DELETE FROM accounts WHERE id=$1", account.ID)
	})
	errs := make(chan error, 33)
	var wg sync.WaitGroup
	for i := 0; i < 33; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			update := map[string]any{fmt.Sprintf("concurrent_%d", i): true}
			if i == 32 {
				update = map[string]any{service.OpenAIForceFastExtraKey: true}
			}
			errs <- repo.UpdateExtra(ctx, account.ID, update)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	got, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.True(t, got.IsOpenAIForceFastEnabled())
	require.Equal(t, "original", got.Extra["preserved"])
	for i := 0; i < 32; i++ {
		require.Equal(t, true, got.Extra[fmt.Sprintf("concurrent_%d", i)])
	}
	expectedExtra := maps.Clone(got.Extra)
	expectedExtra[service.OpenAIForceFastExtraKey] = false
	require.NoError(t, repo.UpdateExtra(ctx, account.ID, map[string]any{service.OpenAIForceFastExtraKey: false}))
	got, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.False(t, got.IsOpenAIForceFastEnabled())
	require.Equal(t, expectedExtra, got.Extra)
	var outboxCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM scheduler_outbox WHERE account_id=$1", account.ID).Scan(&outboxCount))
	require.Positive(t, outboxCount, "persisted changes must invalidate scheduler snapshots")
}
