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
			err = NewAccountRiskStore(db, nil).Update(context.Background(), func(*service.AccountRiskState, service.AccountRiskScheduling) error { called = true; return nil })
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
		err = NewAccountRiskStore(db, nil).Update(context.Background(), func(state *service.AccountRiskState, stop service.AccountRiskScheduling) error {
			ok, err := stop.Stop(7, &service.Account{ID: 19, UpdatedAt: time.Now()})
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
	mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
	mock.ExpectExec("UPDATE accounts SET schedulable=false").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	err = NewAccountRiskStore(db, nil).Update(context.Background(), func(_ *service.AccountRiskState, stop service.AccountRiskScheduling) error {
		changed, err := stop.Stop(7, &service.Account{ID: 19})
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

func TestAccountRiskStorePauseAndResumeAreAtomic(t *testing.T) {
	for _, action := range []string{"pause", "resume"} {
		for _, outcome := range []string{"commit", "stale", "rollback"} {
			t.Run(action+"/"+outcome, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer db.Close()
				mock.ExpectBegin()
				mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
				mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
				mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
				query := "UPDATE accounts SET schedulable=false,.*jsonb_set.*WHERE id=\\$1 AND updated_at=\\$2.*schedulable=true"
				if action == "resume" {
					query = "UPDATE accounts SET schedulable=true,.*extra=.*WHERE id=\\$1 AND updated_at=\\$2.*schedulable=false.*extra->>'yoshub_risk_capacity_pause'=\\$4"
				}
				rows := int64(1)
				if outcome == "stale" {
					rows = 0
				}
				mock.ExpectExec(query).WithArgs(int64(19), sqlmock.AnyArg(), int64(7), "owned-pause").WillReturnResult(sqlmock.NewResult(0, rows))
				if rows != 0 {
					mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(1, 1))
					if outcome == "rollback" {
						mock.ExpectExec("INSERT INTO settings").WillReturnError(errors.New("disk full"))
						mock.ExpectRollback()
					} else {
						mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(1, 1))
						mock.ExpectCommit()
					}
				} else {
					mock.ExpectCommit()
				}
				err = NewAccountRiskStore(db, nil).Update(context.Background(), func(state *service.AccountRiskState, scheduling service.AccountRiskScheduling) error {
					mutate := scheduling.Pause
					if action == "resume" {
						mutate = scheduling.Resume
					}
					changed, err := mutate(7, &service.Account{ID: 19, UpdatedAt: time.Now()}, "owned-pause")
					require.Equal(t, rows != 0, changed)
					if err != nil {
						return err
					}
					if changed {
						state.Rules[7] = &service.AccountRiskRule{GroupID: 7, Config: service.DefaultAccountRiskConfig()}
					}
					return nil
				})
				if outcome == "rollback" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}

func TestAccountRiskStoreQueueSurvivesLegacyStateRewrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	// An old slot only rewrites the legacy document, including when draining.
	legacy, err := encodeLegacyAccountRiskState(&service.AccountRiskState{
		Rules:          map[int64]*service.AccountRiskRule{},
		CapacityPauses: map[int64]*service.AccountRiskCapacityPause{19: {AccountID: 19, GroupID: 7, Token: "owned-pause"}},
	})
	require.NoError(t, err)
	require.NotContains(t, string(legacy), "capacity_pauses")
	mock.ExpectQuery("SELECT value FROM settings").WithArgs(service.AccountRiskSettingKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(legacy)))
	mock.ExpectQuery("SELECT value FROM settings").WithArgs(accountRiskCapacityPausesSettingKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(`{"19":{"account_id":19,"group_id":7,"token":"owned-pause"}}`))
	state, err := NewAccountRiskStore(db, nil).Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, "owned-pause", state.CapacityPauses[19].Token)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRiskStoreQueueWriteFailureRollsBackAccountPause(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
	mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
	mock.ExpectExec("UPDATE accounts SET schedulable=false").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO scheduler_outbox").WillReturnResult(sqlmock.NewResult(1, 1))
	pause := &service.AccountRiskCapacityPause{AccountID: 19, GroupID: 7, Token: "owned-pause"}
	queue, err := json.Marshal(map[int64]*service.AccountRiskCapacityPause{19: pause})
	require.NoError(t, err)
	mock.ExpectExec("INSERT INTO settings").WithArgs(accountRiskCapacityPausesSettingKey, string(queue)).WillReturnError(errors.New("disk full"))
	mock.ExpectRollback()
	err = NewAccountRiskStore(db, nil).Update(context.Background(), func(state *service.AccountRiskState, scheduling service.AccountRiskScheduling) error {
		changed, err := scheduling.Pause(7, &service.Account{ID: 19}, pause.Token)
		require.True(t, changed)
		if err != nil {
			return err
		}
		state.CapacityPauses = map[int64]*service.AccountRiskCapacityPause{19: pause}
		return nil
	})
	require.ErrorContains(t, err, "disk full")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRiskStoreInvalidQueueFailsClosed(t *testing.T) {
	for _, raw := range []string{`not json`, `{"19":null}`, `{"19":{"account_id":20,"group_id":7,"token":"x"}}`, `{"19":{"account_id":19,"group_id":7}}`} {
		t.Run(raw, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
			mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
			_, err = NewAccountRiskStore(db, nil).Read(context.Background())
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
