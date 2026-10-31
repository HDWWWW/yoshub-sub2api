package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

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
