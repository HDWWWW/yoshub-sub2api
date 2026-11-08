//go:build integration

package repository

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestModerationEvidenceStoragePostgres(t *testing.T) {
	ctx := context.Background()
	db := integrationDB
	repo := NewContentModerationRepository(db)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "TRUNCATE content_moderation_logs")
		_, _ = db.ExecContext(ctx, "DELETE FROM settings WHERE key = 'content_moderation_config'")
		_, _ = db.ExecContext(ctx, "UPDATE content_moderation_storage SET max_bytes = 200000000000 WHERE id = 1")
	})
	checkBudget := func() int64 {
		t.Helper()
		var used, actual, limit int64
		require.NoError(t, db.QueryRowContext(ctx, `SELECT used_bytes, max_bytes,
			(SELECT COALESCE(SUM(storage_bytes),0) FROM content_moderation_logs)
			FROM content_moderation_storage WHERE id = 1`).Scan(&used, &limit, &actual))
		require.Equal(t, actual, used, "counter must match retained records exactly")
		require.LessOrEqual(t, used, limit)
		return used
	}

	legacy := &service.ContentModerationLog{InputExcerpt: "历史摘要"}
	require.NoError(t, repo.CreateLog(ctx, legacy))
	old, err := repo.GetLogInput(ctx, legacy.ID)
	require.NoError(t, err)
	require.Nil(t, old.Evidence)

	full := &service.ContentModerationLog{Action: "bio_policy", Flagged: true, InputEvidence: &service.ContentModerationInputEvidence{
		Policy: &service.ContentModerationPolicyEvidence{Code: "bio_policy", RequestBody: strings.Repeat("完整输入🎬", 5000), UpstreamBody: strings.Repeat("官方响应", 2000), UpstreamStatus: 200},
	}}
	require.NoError(t, repo.CreateLog(ctx, full))
	detail, err := repo.GetLogInput(ctx, full.ID)
	require.NoError(t, err)
	require.Equal(t, full.InputEvidence, detail.Evidence)
	logs, _, err := repo.ListLogs(ctx, service.ContentModerationLogFilter{})
	require.NoError(t, err)
	for _, log := range logs {
		require.Nil(t, log.InputEvidence, "list queries must not load large evidence")
	}
	checkBudget()

	// Reapplying the additive migration must preserve old and new evidence.
	migration, err := migrations.FS.ReadFile("257_content_moderation_input_evidence.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	checkBudget()
	detail, err = repo.GetLogInput(ctx, full.ID)
	require.NoError(t, err)
	require.Equal(t, full.InputEvidence, detail.Evidence)

	// Use a small byte budget to exercise real eviction without allocating GBs.
	_, err = db.ExecContext(ctx, "UPDATE content_moderation_storage SET max_bytes = 8000 WHERE id = 1; SELECT content_moderation_storage_trim()")
	require.NoError(t, err)
	checkBudget()
	_, err = repo.GetLogInput(ctx, legacy.ID)
	require.Error(t, err, "oldest log must be evicted first")

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- repo.CreateLog(ctx, &service.ContentModerationLog{InputExcerpt: strings.Repeat("并发日志", 80)})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	used := checkBudget()
	require.Positive(t, used)
	var count int
	var first, last int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*), MIN(id), MAX(id) FROM content_moderation_logs").Scan(&count, &first, &last))
	require.Less(t, count, 32)
	require.Equal(t, int64(count), last-first+1, "retained records must be the newest suffix")

	err = repo.CreateLog(ctx, &service.ContentModerationLog{InputExcerpt: strings.Repeat("oversize", 2000)})
	require.ErrorContains(t, err, "single moderation log exceeds")
	require.Equal(t, used, checkBudget(), "an oversized record must not delete existing logs")
	require.NoError(t, repo.UpdateLogEmailSent(ctx, last, true))
	checkBudget()

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "INSERT INTO content_moderation_logs (input_excerpt) VALUES ('rollback evidence')")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	checkBudget()
	_, err = db.ExecContext(ctx, "DELETE FROM content_moderation_logs WHERE id = $1", first)
	require.NoError(t, err)
	checkBudget()

	_, err = db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('content_moderation_config', '{"log_storage_limit_gb":1}')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	require.NoError(t, err)
	var limit int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT max_bytes FROM content_moderation_storage WHERE id = 1").Scan(&limit))
	require.Equal(t, int64(1000000000), limit)
	// Retention cleanup must release the budget across multiple delete batches.
	_, err = db.ExecContext(ctx, `INSERT INTO content_moderation_logs (flagged, created_at)
		SELECT true, NOW() - INTERVAL '10 days' FROM generate_series(1, 1005)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO content_moderation_logs (flagged, created_at)
		SELECT false, NOW() - INTERVAL '2 days' FROM generate_series(1, 3)`)
	require.NoError(t, err)
	cleanup, err := repo.CleanupExpiredLogs(ctx, time.Now().Add(-5*24*time.Hour), time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1005), cleanup.DeletedHit)
	require.Equal(t, int64(3), cleanup.DeletedNonHit)
	checkBudget()
	_, err = db.ExecContext(ctx, "TRUNCATE content_moderation_logs")
	require.NoError(t, err)
	require.Zero(t, checkBudget())
}
