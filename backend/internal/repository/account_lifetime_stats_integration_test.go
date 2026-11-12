//go:build integration

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestAccountLifetimeStatsRetainsTotalsAndCleansDeletion(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newUsageLogRepositoryWithSQL(client, tx)
	user := mustCreateUser(t, client, &service.User{Email: "lifetime@test.com"})
	key := mustCreateApiKey(t, client, &service.APIKey{UserID: user.ID, Key: "sk-lifetime", Name: "lifetime"})
	a := mustCreateAccount(t, client, &service.Account{Name: "lifetime-account"})
	rate, customCost := 2.0, 1.5
	entry := &service.UsageLog{UserID: user.ID, APIKeyID: key.ID, AccountID: a.ID, RequestID: "lifetime-request", Model: "gpt-5",
		InputTokens: 10, OutputTokens: 20, CacheCreationTokens: 3, CacheReadTokens: 7, TotalCost: 2, ActualCost: 0.5,
		AccountRateMultiplier: &rate, AccountStatsCost: &customCost, CreatedAt: time.Now()}
	_, err := repo.Create(ctx, entry)
	require.NoError(t, err)
	_, err = repo.Create(ctx, entry)
	require.NoError(t, err)
	stats, err := repo.GetAccountLifetimeStatsBatch(ctx, []int64{a.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), stats[a.ID].Requests, "deduplicated writes count once")
	require.Equal(t, int64(40), stats[a.ID].Tokens)
	require.InDelta(t, 3, stats[a.ID].Cost, 1e-9)
	require.InDelta(t, 2, stats[a.ID].StandardCost, 1e-9)
	require.InDelta(t, 0.5, stats[a.ID].UserCost, 1e-9)
	// Existing accounts receive a one-time baseline; rerunning is idempotent.
	_, err = tx.ExecContext(ctx, "DELETE FROM account_lifetime_stats WHERE account_id=$1", a.ID)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("258_account_lifetime_stats.sql")
	require.NoError(t, err)
	for range 2 {
		_, err = tx.ExecContext(ctx, string(migration))
		require.NoError(t, err)
		backfill, err := repo.GetAccountLifetimeStatsBatch(ctx, []int64{a.ID})
		require.NoError(t, err)
		require.Equal(t, stats, backfill)
	}
	_, err = tx.ExecContext(ctx, "UPDATE usage_logs SET output_tokens=25, actual_cost=0.75 WHERE account_id=$1", a.ID)
	require.NoError(t, err)
	stats, err = repo.GetAccountLifetimeStatsBatch(ctx, []int64{a.ID})
	require.NoError(t, err)
	require.Equal(t, int64(1), stats[a.ID].Requests)
	require.Equal(t, int64(45), stats[a.ID].Tokens)
	require.InDelta(t, 0.75, stats[a.ID].UserCost, 1e-9)
	_, err = tx.ExecContext(ctx, "DELETE FROM usage_logs WHERE account_id=$1", a.ID)
	require.NoError(t, err)
	retained, err := repo.GetAccountLifetimeStatsBatch(ctx, []int64{a.ID})
	require.NoError(t, err)
	require.Equal(t, stats, retained, "request retention cannot erase lifetime totals")
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET deleted_at=NOW() WHERE id=$1", a.ID)
	require.NoError(t, err)
	entry.RequestID = "late-request"
	_, err = repo.Create(ctx, entry)
	require.NoError(t, err)
	var count int
	require.NoError(t, scanSingleRow(ctx, tx, "SELECT COUNT(*) FROM account_lifetime_stats WHERE account_id=$1", []any{a.ID}, &count))
	require.Zero(t, count, "late requests cannot recreate deleted-account statistics")
	stats, err = repo.GetAccountLifetimeStatsBatch(ctx, []int64{a.ID})
	require.NoError(t, err)
	require.Empty(t, stats)
	_, err = tx.ExecContext(ctx, "DELETE FROM usage_logs WHERE account_id=$1", a.ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM accounts WHERE id=$1", a.ID)
	require.NoError(t, err)
	require.NoError(t, scanSingleRow(ctx, tx, "SELECT COUNT(*) FROM account_lifetime_stats WHERE account_id=$1", []any{a.ID}, &count))
	require.Zero(t, count)
}

func TestAccountLifetimeStatsConcurrentDeleteAndUsage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := testEntClient(t)
	u := mustCreateUser(t, client, &service.User{Email: "lifetime-race@test.com"})
	k := mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, Key: "sk-lifetime-race", Name: "race"})
	cleanupUsageLogBatchRows(t, k.ID)
	a := mustCreateAccount(t, client, &service.Account{Name: "lifetime-race"})
	start := make(chan struct{})
	errs := make(chan error, 17)
	var wg sync.WaitGroup
	for i := range 17 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			var err error
			if i == 8 {
				_, err = integrationDB.ExecContext(ctx, "UPDATE accounts SET deleted_at=NOW() WHERE id=$1", a.ID)
			} else {
				_, err = integrationDB.ExecContext(ctx, `INSERT INTO usage_logs (user_id,api_key_id,account_id,request_id,model,input_tokens,output_tokens,total_cost,actual_cost) VALUES ($1,$2,$3,$4,'gpt-5',10,20,1,0.5)`, u.ID, k.ID, a.ID, fmt.Sprintf("lifetime-race-%d", i))
			}
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM account_lifetime_stats WHERE account_id=$1", a.ID).Scan(&count))
	require.Zero(t, count)
}
