//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountAutoFastStoreAtomicFlagStateAndOutbox(t *testing.T) {
	for _, failure := range []string{"", "outbox", "state", "commit"} {
		t.Run(failure, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			at := time.Now()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WithArgs(int64(8362461180575)).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
			mock.ExpectQuery("SELECT value FROM settings").WithArgs(service.AccountAutoFastSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}))
			mock.ExpectExec("UPDATE accounts SET extra=COALESCE").WithArgs(`{"openai_force_fast":true}`, int64(19), at, int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
			if failure == "outbox" {
				mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnError(errors.New("outbox down"))
				mock.ExpectRollback()
			} else {
				mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(1, 1))
				if failure == "state" {
					mock.ExpectExec("INSERT INTO settings").WillReturnError(errors.New("state down"))
					mock.ExpectRollback()
				} else {
					mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(1, 1))
					if failure == "commit" {
						mock.ExpectCommit().WillReturnError(errors.New("commit failed"))
					} else {
						mock.ExpectCommit()
					}
				}
			}
			err = NewAccountAutoFastStore(db, nil).Update(context.Background(), func(state *service.AccountAutoFastState, apply service.AccountAutoFastApply) error {
				ok, err := apply(7, &service.Account{ID: 19, UpdatedAt: at}, true)
				if err != nil {
					return err
				}
				require.True(t, ok)
				state.Rules[7] = &service.AccountAutoFastRule{GroupID: 7, Config: service.DefaultAccountAutoFastConfig()}
				state.Managed[19] = service.AccountAutoFastManaged{GroupID: 7, Applied: true}
				return nil
			})
			if failure == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAccountAutoFastStoreBusyReadErrorAndCAS(t *testing.T) {
	for _, kind := range []string{"busy", "read", "cas"} {
		t.Run(kind, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(kind != "busy"))
			if kind == "read" {
				mock.ExpectQuery("SELECT value FROM settings").WillReturnError(errors.New("db error"))
			}
			if kind == "cas" {
				mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
				mock.ExpectExec("UPDATE accounts SET extra=COALESCE").WillReturnResult(sqlmock.NewResult(0, 0))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}
			called := false
			err = NewAccountAutoFastStore(db, nil).Update(context.Background(), func(_ *service.AccountAutoFastState, apply service.AccountAutoFastApply) error {
				called = true
				ok, err := apply(7, &service.Account{ID: 19}, true)
				require.False(t, ok)
				return err
			})
			require.Equal(t, kind == "cas", called)
			if kind == "cas" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAccountAutoFastStoreInvalidStateFailsClosed(t *testing.T) {
	for _, raw := range []string{`not json`, `{"rules":{"7":null}}`, `{"rules":{"7":{"group_id":8}}}`, `{"managed":{"1":{"group_id":7}}}`} {
		_, err := decodeAccountAutoFastState(raw)
		require.Error(t, err)
	}
	s, err := decodeAccountAutoFastState("")
	require.NoError(t, err)
	require.Empty(t, s.Rules)
	s.Rules[7] = &service.AccountAutoFastRule{GroupID: 7, Config: service.DefaultAccountAutoFastConfig()}
	s.Managed[1] = service.AccountAutoFastManaged{GroupID: 7, Original: true}
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	got, err := decodeAccountAutoFastState(string(raw))
	require.NoError(t, err)
	require.True(t, got.Managed[1].Original)
}
