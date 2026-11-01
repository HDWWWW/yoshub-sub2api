//go:build unit

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Run only against a disposable database. No supplier credentials or purchase
// endpoint is used: this exercises actual PostgreSQL session lock contention.
func TestRefillAutoFastSaveWaitsForPostgresWorkerLock(t *testing.T) {
	dsn := os.Getenv("SUB2_REFILL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set SUB2_REFILL_TEST_DATABASE_URL to a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.ExecContext(ctx, `CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`)
	require.NoError(t, err)
	store := NewAccountRefillStore(db)
	svc := service.NewAccountRefillService(store, nil, nil, nil, nil, nil, nil)
	c := service.DefaultAccountRefillConfig()
	c.GroupID, c.DeliveryGroupIDs, c.Enabled = 2, []int64{2}, false
	c.AutoFast = &service.AccountRefillAutoFastConfig{Enabled: true, StopMode: "follow", StartRemainingPercent: 40}
	require.NoError(t, svc.Configure(ctx, c, "", 0))
	var before map[string]any
	raw, err := store.Read(ctx)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(raw), &before))
	before["jobs"] = []any{map[string]any{"id": "accepted-pro20x", "supplier_id": "supplier-20x", "plan": "pro20x", "status": "queued"}}
	seed, err := json.Marshal(before)
	require.NoError(t, err)
	require.NoError(t, store.WithLock(ctx, func(_ string, save func(string) error) error { return save(string(seed)) }))

	worker, err := db.Conn(ctx)
	require.NoError(t, err)
	defer worker.Close()
	const lockID int64 = 8362461180561
	_, err = worker.ExecContext(ctx, "SELECT pg_advisory_lock($1)", lockID)
	require.NoError(t, err)
	defer worker.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", lockID)
	c.AutoFast.StopMode, c.AutoFast.StopUsed5hPercent, c.AutoFast.StopUsed7dPercent = "custom", 85, 90
	saved := make(chan error, 1)
	go func() { saved <- svc.Configure(ctx, c, "", 1) }()
	select {
	case err := <-saved:
		t.Fatalf("save rejected instead of waiting for the PostgreSQL worker lock: %v", err)
	case <-time.After(350 * time.Millisecond):
	}
	// GET and the worker's nonblocking path retain their original behavior.
	_, err = store.Read(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, store.WithLock(ctx, func(string, func(string) error) error { return nil }), service.ErrAccountRefillBusy)
	_, err = worker.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", lockID)
	require.NoError(t, err)
	require.NoError(t, <-saved)
	raw, err = store.Read(ctx)
	require.NoError(t, err)
	var after struct {
		Config   service.AccountRefillConfig `json:"config"`
		Revision int64                       `json:"revision"`
		Jobs     []service.AccountRefillJob  `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &after))
	require.Equal(t, int64(2), after.Revision)
	require.True(t, after.Config.AutoFast.Enabled)
	require.Equal(t, "custom", after.Config.AutoFast.StopMode)
	require.Equal(t, 85.0, after.Config.AutoFast.StopUsed5hPercent)
	require.Len(t, after.Jobs, 1)
	require.Equal(t, "accepted-pro20x", after.Jobs[0].ID)
	require.Equal(t, "queued", after.Jobs[0].Status)
}
