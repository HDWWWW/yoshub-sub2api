package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRefillCutoverPauseResumesWithoutDatabaseLock(t *testing.T) {
	pauseFile := filepath.Join(t.TempDir(), "new-slot.pause")
	t.Setenv("SUB2API_REFILL_PAUSE_FILE", pauseFile)
	require.NoError(t, os.WriteFile(pauseFile, []byte("waiting for old slot"), 0600))
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	store := NewAccountRefillStore(db)
	called := false
	update := func(raw string, save func(string) error) error {
		called = true
		require.Equal(t, "old", raw)
		return save("new")
	}
	require.ErrorIs(t, store.WithLock(context.Background(), update), service.ErrAccountRefillBusy)
	require.False(t, called)
	// Public reads continue while mutations and worker side effects are gated.
	mock.ExpectQuery("SELECT value FROM settings").WithArgs(service.AccountRefillSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("old"))
	raw, err := store.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, "old", raw)
	require.NoError(t, mock.ExpectationsWereMet())
	require.NoError(t, os.Remove(pauseFile))
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WithArgs(int64(8362461180561)).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT value FROM settings").WithArgs(service.AccountRefillSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("old"))
	mock.ExpectExec("INSERT INTO settings").WithArgs(service.AccountRefillSettingKey, "new").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("SELECT pg_advisory_unlock").WithArgs(int64(8362461180561)).WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	require.NoError(t, store.WithLock(context.Background(), update))
	require.True(t, called)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRefillStoreLockAndFailedSave(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{true: "busy", false: "failed save releases lock"}[busy], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			mock.ExpectQuery("SELECT pg_try_advisory_lock").WithArgs(int64(8362461180561)).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(!busy))
			if !busy {
				mock.ExpectQuery("SELECT value FROM settings").WithArgs(service.AccountRefillSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("old"))
				mock.ExpectExec("INSERT INTO settings").WithArgs(service.AccountRefillSettingKey, "new").WillReturnError(errors.New("disk unavailable"))
				mock.ExpectQuery("SELECT pg_advisory_unlock").WithArgs(int64(8362461180561)).WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
			}
			called := false
			err = NewAccountRefillStore(db).WithLock(context.Background(), func(raw string, save func(string) error) error {
				called = true
				require.Equal(t, "old", raw)
				return save("new")
			})
			require.Error(t, err)
			require.Equal(t, !busy, called)
			if busy {
				require.ErrorIs(t, err, service.ErrAccountRefillBusy)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAccountRefillGroupStoreSharesLockAndUsesSeparateKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	key := service.AccountRefillSettingKey + ":group:42"
	mock.ExpectQuery("SELECT pg_try_advisory_lock").WithArgs(int64(8362461180561)).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT value FROM settings").WithArgs(key).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("old-group"))
	mock.ExpectExec("INSERT INTO settings").WithArgs(key, "new-group").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery("SELECT pg_advisory_unlock").WithArgs(int64(8362461180561)).WillReturnRows(sqlmock.NewRows([]string{"released"}).AddRow(true))
	store := NewAccountRefillStore(db).(service.AccountRefillGroupStore)
	err = store.ForGroup(42).WithLock(context.Background(), func(raw string, save func(string) error) error {
		require.Equal(t, "old-group", raw)
		return save("new-group")
	})
	require.NoError(t, err)
	mock.ExpectQuery("SELECT key,value FROM settings").WithArgs(service.AccountRefillSettingKey, service.AccountRefillSettingKey+":group:").WillReturnRows(sqlmock.NewRows([]string{"key", "value"}).AddRow(service.AccountRefillSettingKey, "legacy").AddRow(key, "new-group"))
	rows, err := store.ListRules(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[int64]string{0: "legacy", 42: "new-group"}, rows)
	require.NoError(t, mock.ExpectationsWereMet())
}
