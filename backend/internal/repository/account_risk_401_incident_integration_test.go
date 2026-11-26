//go:build integration

package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRisk401IncidentPostgres(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	group := mustCreateGroup(t, client, &service.Group{Name: t.Name(), Platform: service.PlatformOpenAI})
	account := mustCreateAccount(t, client, &service.Account{Name: t.Name(), Platform: service.PlatformOpenAI})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id=$1`, account.ID)
		_, _ = integrationDB.Exec(`DELETE FROM groups WHERE id=$1`, group.ID)
	})
	store := NewAccountRiskStore(integrationDB, nil)
	at := time.Now().UTC().Truncate(time.Microsecond)
	abort := errors.New("abort history write")
	err := store.Update(ctx, func(_ *service.AccountRiskState, tx service.AccountRiskScheduling) error {
		_, _, err := tx.Record401(group.ID, account.ID, "rollback", at)
		if err != nil {
			return err
		}
		return abort
	})
	require.ErrorIs(t, err, abort)
	record := func(id string, when time.Time) (string, int, error) {
		tx, err := integrationDB.BeginTx(ctx, nil)
		if err != nil {
			return "", 0, err
		}
		defer func() { _ = tx.Rollback() }()
		first, count, err := recordRisk401(ctx, tx, group.ID, account.ID, id, when)
		if err != nil {
			return "", 0, err
		}
		return first, count, tx.Commit()
	}
	// Independent transactions race for the very first receipt. Even without
	// the outer advisory lock the unique row elects exactly one notification.
	const parallel = 16
	type result struct {
		first string
		count int
		err   error
	}
	results := make(chan result, parallel)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, n, err := record(fmt.Sprintf("concurrent-%d", i), at)
			results <- result{id, n, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	firstID := ""
	counts := map[int]bool{}
	for v := range results {
		require.NoError(t, v.err)
		if firstID == "" {
			firstID = v.first
		}
		require.Equal(t, firstID, v.first)
		counts[v.count] = true
	}
	require.NotEqual(t, "rollback", firstID)
	require.Len(t, counts, parallel)
	require.True(t, counts[1])
	require.True(t, counts[parallel])
	check := func(id string, when time.Time, wantID string, wantCount int) {
		t.Helper()
		first, n, err := record(id, when)
		require.NoError(t, err)
		require.Equal(t, wantID, first)
		require.Equal(t, wantCount, n)
	}
	require.NoError(t, store.Recover401(ctx, account.ID, at))
	check("stale-success", at, firstID, parallel+1)
	recovered := at.Add(time.Minute)
	require.NoError(t, store.Recover401(ctx, account.ID, recovered))
	check("delayed", recovered, firstID, parallel+2)
	check("new", recovered.Add(time.Second), "new", 1)
	check("old-arrival", at, "new", 1)
	check("second", recovered.Add(2*time.Second), "new", 2)
}
