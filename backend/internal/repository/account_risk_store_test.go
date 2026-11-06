//go:build unit

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRiskStoreBusyAndReadFailure(t *testing.T) {
	for _, kind := range []string{"busy", "read failed"} {
		t.Run(kind, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(kind != "busy"))
			if kind == "read failed" {
				mock.ExpectQuery("SELECT value FROM settings").WillReturnError(errors.New("offline"))
			}
			mock.ExpectRollback()
			called := false
			err = NewAccountRiskStore(db, nil).Update(context.Background(), func(*service.AccountRiskState, service.AccountRiskStop) error { called = true; return nil })
			require.Error(t, err)
			require.False(t, called)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAccountRiskStoreStopAndHistoryAreAtomic(t *testing.T) {
	for _, saveFailed := range []bool{false, true} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
		mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
		mock.ExpectExec("UPDATE accounts SET schedulable=false").WithArgs(int64(19), sqlmock.AnyArg(), int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(1, 1))
		if saveFailed {
			mock.ExpectExec("INSERT INTO settings").WillReturnError(errors.New("disk full"))
			mock.ExpectRollback()
		} else {
			mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()
		}
		err = NewAccountRiskStore(db, nil).Update(context.Background(), func(state *service.AccountRiskState, stop service.AccountRiskStop) error {
			ok, err := stop(7, &service.Account{ID: 19, UpdatedAt: time.Now()})
			require.True(t, ok)
			if err != nil {
				return err
			}
			state.Rules[7] = &service.AccountRiskRule{GroupID: 7, Config: service.DefaultAccountRiskConfig(), Events: []service.AccountRiskEvent{{AccountID: 19, Reason: "test stop"}}}
			return nil
		})
		if saveFailed {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	}
}

func TestAccountRiskStoreStaleAccountDoesNotPublishStop(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
	mock.ExpectExec("UPDATE accounts SET schedulable=false").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	err = NewAccountRiskStore(db, nil).Update(context.Background(), func(_ *service.AccountRiskState, stop service.AccountRiskStop) error {
		changed, err := stop(7, &service.Account{ID: 19})
		require.False(t, changed)
		return err
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRiskStoreInvalidStateFailsClosed(t *testing.T) {
	for _, raw := range []string{`not json`, `{"rules":{"7":null}}`, `{"rules":{"7":{"group_id":8}}}`} {
		_, err := decodeAccountRiskState(raw)
		require.Error(t, err)
	}
	state, err := decodeAccountRiskState("")
	require.NoError(t, err)
	require.Empty(t, state.Rules)
}
