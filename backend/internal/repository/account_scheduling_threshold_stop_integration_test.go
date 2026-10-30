//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountSchedulingThresholdStopLifecycle(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			tx := testEntTx(t)
			ctx := dbent.NewTxContext(context.Background(), tx)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			account := &service.Account{Name: "threshold-lifecycle", Platform: service.PlatformOpenAI,
				Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 50, Priority: 7}
			require.NoError(t, repo.Create(ctx, account))
			updates := map[string]any{"codex_usage_updated_at": time.Now().Format(time.RFC3339),
				"codex_" + window + "_used_percent": 98.9, "codex_" + window + "_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}
			load := func() *service.Account {
				a, err := repo.GetByID(ctx, account.ID)
				require.NoError(t, err)
				require.Equal(t, 7, a.Priority)
				require.Equal(t, 50, a.Concurrency)
				require.Nil(t, a.TempUnschedulableUntil)
				require.Empty(t, a.TempUnschedulableReason)
				return a
			}
			require.NoError(t, repo.UpdateExtra(ctx, account.ID, updates))
			require.True(t, load().Schedulable)
			// Model consumption of the creation event before the threshold is hit.
			_, err := tx.ExecContext(ctx, "UPDATE scheduler_outbox SET dedup_key=NULL WHERE account_id=$1", account.ID)
			require.NoError(t, err)
			var beforeEvents int
			require.NoError(t, scanSingleRow(ctx, tx, "SELECT count(*) FROM scheduler_outbox WHERE account_id=$1", []any{account.ID}, &beforeEvents))
			updates["codex_"+window+"_used_percent"] = 99.0
			require.NoError(t, repo.UpdateExtra(ctx, account.ID, updates))
			require.False(t, load().Schedulable)
			var afterEvents int
			require.NoError(t, scanSingleRow(ctx, tx, "SELECT count(*) FROM scheduler_outbox WHERE account_id=$1", []any{account.ID}, &afterEvents))
			require.Equal(t, beforeEvents+1, afterEvents, "persistent stop must notify the scheduler")
			updates["codex_"+window+"_used_percent"] = 0.0
			updates["codex_"+window+"_reset_at"] = time.Now().Add(5 * time.Hour).Format(time.RFC3339)
			require.NoError(t, repo.UpdateExtra(ctx, account.ID, updates))
			require.False(t, load().Schedulable, "quota reset must not re-enable scheduling")
			require.NoError(t, repo.SetSchedulable(ctx, account.ID, true))
			blocked, err := repo.StopAccountAtSchedulingThreshold(ctx, account.ID)
			require.NoError(t, err)
			require.False(t, blocked, "old cached usage must not disable the fresh reset window")
			require.True(t, load().Schedulable)
			updates["codex_"+window+"_used_percent"] = 100.0
			require.NoError(t, repo.UpdateExtra(ctx, account.ID, updates))
			require.False(t, load().Schedulable)
		})
	}
}

func TestAccountSchedulingThresholdStopCommitsWithUsage(t *testing.T) {
	ctx := context.Background()
	cache := NewSchedulerCache(testRedis(t))
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, cache)
	a := &service.Account{Name: "threshold-committed", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true}
	require.NoError(t, repo.Create(ctx, a))
	t.Cleanup(func() { _ = repo.Delete(ctx, a.ID) })
	bucket := service.SchedulerBucket{GroupID: a.ID, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{*a}))
	for _, percent := range []float64{98.9, 99, 0} {
		require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{
			"codex_5h_used_percent": percent, "codex_5h_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}))
		stored, err := repo.GetByID(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, percent, stored.Extra["codex_5h_used_percent"], "usage-only transactions must commit too")
		require.Equal(t, percent == 98.9, stored.Schedulable)
		cached, err := cache.GetAccount(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, cached)
		require.Equal(t, stored.Schedulable, cached.Schedulable, "the scheduler must see the committed switch")
		candidates, hit, err := cache.GetSnapshot(ctx, bucket)
		require.NoError(t, err)
		require.True(t, hit)
		for _, candidate := range candidates {
			require.Equal(t, stored.Schedulable, candidate.Schedulable)
		}
	}
}

func TestAccountSchedulingThresholdStopOutboxFailureRollsBack(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	a := &service.Account{Name: "threshold-rollback", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true}
	require.NoError(t, repo.Create(ctx, a))
	_, err := tx.ExecContext(ctx, `CREATE FUNCTION pg_temp.reject_threshold_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test threshold outbox failure'; END $$;
		CREATE TRIGGER reject_threshold_event BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION pg_temp.reject_threshold_event();
		SAVEPOINT before_threshold`)
	require.NoError(t, err)
	err = repo.UpdateExtra(ctx, a.ID, map[string]any{"codex_5h_used_percent": 99.0,
		"codex_5h_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	require.ErrorContains(t, err, "test threshold outbox failure")
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT before_threshold")
	require.NoError(t, err)
	stored, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, stored.Schedulable)
	require.NotContains(t, stored.Extra, "codex_5h_used_percent")
}

func TestAccountSchedulingThresholdStopHonorsOverrideAndCooldown(t *testing.T) {
	for _, kind := range []string{"disabled", "platform-default", "threshold-cooldown", "error-cooldown", "identity-mismatch", "imported-full"} {
		t.Run(kind, func(t *testing.T) {
			tx := testEntTx(t)
			ctx := dbent.NewTxContext(context.Background(), tx)
			repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
			account := &service.Account{Name: kind, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Schedulable: true, Credentials: map[string]any{"account_scheduling_threshold": 99}}
			until := time.Now().Add(time.Hour)
			if kind == "disabled" {
				account.Credentials["account_scheduling_threshold"] = 100
			}
			if kind == "identity-mismatch" {
				account.Credentials["email"] = "owner@example.invalid"
			}
			updates := map[string]any{"codex_5h_used_percent": 99.0, "codex_5h_reset_at": until.Format(time.RFC3339)}
			if kind == "imported-full" {
				account.Extra = updates
			}
			require.NoError(t, repo.Create(ctx, account))
			if kind == "platform-default" {
				require.NoError(t, repo.UpdateCredentials(ctx, account.ID, map[string]any{}))
			}
			if kind == "threshold-cooldown" || kind == "error-cooldown" {
				reason := "unrelated upstream failure"
				if kind == "threshold-cooldown" {
					reason = service.BuildDetailedAccountSchedulingThresholdReason(service.AccountSchedulingThresholdReasonInput{
						Platform: service.PlatformOpenAI, Window: "5h", ThresholdPercent: 99, UsedPercent: 99, Until: until, Now: time.Now()})
				}
				require.NoError(t, repo.SetTempUnschedulable(ctx, account.ID, until, reason))
			}
			if kind == "identity-mismatch" {
				updates["email"] = "other@example.invalid"
			}
			require.NoError(t, repo.UpdateExtra(ctx, account.ID, updates))
			stored, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, kind == "disabled" || kind == "platform-default" || kind == "identity-mismatch", stored.Schedulable)
			if kind == "error-cooldown" {
				require.NotNil(t, stored.TempUnschedulableUntil)
				require.Equal(t, "unrelated upstream failure", stored.TempUnschedulableReason)
			} else {
				require.Nil(t, stored.TempUnschedulableUntil)
			}
		})
	}
}
