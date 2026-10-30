//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountExpiryPriorityLifecycle(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	repo := newAccountRepositoryWithSQL(nil, tx, nil)
	now := time.Date(2026, 10, 30, 7, 0, 0, 0, time.UTC)
	soon, later, past := now.Add(time.Hour), now.Add(2*time.Hour), now.Add(-time.Hour)
	insert := func(name string, priority int, expiry *time.Time, deleted bool) int64 {
		var id int64
		err := scanSingleRow(ctx, tx, `INSERT INTO accounts
			(name,platform,type,credentials,priority,expires_at,deleted_at)
			VALUES ($1,'openai','oauth','{}',$2,$3,CASE WHEN $4 THEN NOW() END) RETURNING id`,
			[]any{name, priority, expiry, deleted}, &id)
		require.NoError(t, err)
		return id
	}
	first := insert("soon", 30, &soon, false)
	tie := insert("same-instant-other-timezone", 1, func() *time.Time { v := soon.In(time.FixedZone("CST", 8*3600)); return &v }(), false)
	second := insert("later", 2, &later, false)
	undated := insert("undated", 0, nil, false)
	undatedBackup := insert("undated-backup", 10, nil, false)
	expired := insert("expired", 0, &past, false)
	deleted := insert("deleted", 99, &soon, true)
	priority := func(id int64) int {
		var p int
		require.NoError(t, scanSingleRow(ctx, tx, `SELECT priority FROM accounts WHERE id=$1`, []any{id}, &p))
		return p
	}
	eventCount := func() int {
		var n int
		require.NoError(t, scanSingleRow(ctx, tx, `SELECT COUNT(*) FROM scheduler_outbox WHERE event_type=$1`, []any{service.SchedulerOutboxEventAccountBulkChanged}, &n))
		return n
	}
	beforeEvents := eventCount()
	changed, err := repo.SyncAccountExpiryPriorities(ctx, now)
	require.NoError(t, err)
	require.EqualValues(t, 4, changed)
	require.Equal(t, 1, priority(first))
	require.Equal(t, 1, priority(tie))
	require.Equal(t, 2, priority(second))
	require.Equal(t, 3, priority(undated))
	require.Equal(t, 4, priority(undatedBackup))
	require.Equal(t, 5, priority(expired))
	require.Equal(t, 99, priority(deleted))
	require.Equal(t, beforeEvents+1, eventCount())
	var raw []byte
	require.NoError(t, scanSingleRow(ctx, tx, `SELECT payload FROM scheduler_outbox WHERE event_type=$1 ORDER BY id DESC LIMIT 1`, []any{service.SchedulerOutboxEventAccountBulkChanged}, &raw))
	var payload struct {
		AccountIDs []int64 `json:"account_ids"`
	}
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.ElementsMatch(t, []int64{first, undated, undatedBackup, expired}, payload.AccountIDs)

	changed, err = repo.SyncAccountExpiryPriorities(ctx, now)
	require.NoError(t, err)
	require.Zero(t, changed)
	require.Equal(t, beforeEvents+1, eventCount(), "no event on an unchanged scan")

	// Newly imported accounts must enter their expiry position automatically.
	earlier := now.Add(30 * time.Minute)
	newID := insert("new-import", 50, &earlier, false)
	_, err = repo.SyncAccountExpiryPriorities(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 1, priority(newID))
	require.Equal(t, 2, priority(first))

	// Renewal moves the account back; removing the date moves it behind dated accounts.
	renewed := later.Add(time.Hour)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET expires_at=$1 WHERE id=$2`, renewed, newID)
	require.NoError(t, err)
	_, err = repo.SyncAccountExpiryPriorities(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 1, priority(first))
	require.Equal(t, 3, priority(newID))
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET expires_at=NULL WHERE id=$1`, first)
	require.NoError(t, err)
	_, err = repo.SyncAccountExpiryPriorities(ctx, now)
	require.NoError(t, err)
	require.Greater(t, priority(first), priority(newID))

	// The passage of time alone removes urgency at the exact expiry boundary.
	_, err = repo.SyncAccountExpiryPriorities(ctx, later)
	require.NoError(t, err)
	require.Equal(t, 1, priority(newID))
	require.Greater(t, priority(second), priority(undated))
	require.Equal(t, priority(second), priority(expired))
	require.Equal(t, 99, priority(deleted))
	changed, err = repo.SyncAccountExpiryPriorities(ctx, later)
	require.NoError(t, err)
	require.Zero(t, changed)
}

func TestAccountExpiryPriorityOutboxFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	repo := newAccountRepositoryWithSQL(nil, tx, nil)
	var id int64
	require.NoError(t, scanSingleRow(ctx, tx, `INSERT INTO accounts
		(name,platform,type,credentials,priority,expires_at) VALUES ('rollback','openai','oauth','{}',99,NOW()+INTERVAL '1 day') RETURNING id`, nil, &id))
	_, err := tx.ExecContext(ctx, `CREATE FUNCTION pg_temp.reject_expiry_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test outbox failure'; END $$;
		CREATE TRIGGER reject_expiry_event BEFORE INSERT ON scheduler_outbox FOR EACH ROW EXECUTE FUNCTION pg_temp.reject_expiry_event();
		SAVEPOINT before_sync`)
	require.NoError(t, err)
	_, err = repo.SyncAccountExpiryPriorities(ctx, time.Now())
	require.ErrorContains(t, err, "test outbox failure")
	_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT before_sync`)
	require.NoError(t, err)
	var priority int
	require.NoError(t, scanSingleRow(ctx, tx, `SELECT priority FROM accounts WHERE id=$1`, []any{id}, &priority))
	require.Equal(t, 99, priority)
}
