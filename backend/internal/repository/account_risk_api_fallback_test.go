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

func TestRiskAPIFallbackSchedulingAndOwnershipCommitTogether(t *testing.T) {
	for _, scenario := range []string{"enable", "disable", "stale", "outbox failure", "state failure"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
			for _, key := range []string{service.AccountRiskSettingKey, accountRiskCapacityPausesSettingKey, accountRiskActiveProbesSettingKey} {
				mock.ExpectQuery("SELECT value FROM settings").WithArgs(key).WillReturnRows(sqlmock.NewRows([]string{"value"}))
			}
			at := time.Now()
			enabled := scenario != "disable"
			query := "UPDATE accounts SET schedulable=true,[\\s\\S]*updated_at=\\$2[\\s\\S]*type='apikey'[\\s\\S]*FOR SHARE OF ag,g"
			if !enabled {
				query = "UPDATE accounts SET schedulable=false,[\\s\\S]*updated_at=\\$2[\\s\\S]*extra->>'yoshub_risk_api_fallback'=\\$3"
			}
			update := mock.ExpectExec(query)
			if enabled {
				update.WithArgs(int64(99), at, "lease", int64(7))
			} else {
				update.WithArgs(int64(99), at, "lease")
			}
			rows := int64(1)
			if scenario == "stale" {
				rows = 0
			}
			update.WillReturnResult(sqlmock.NewResult(0, rows))
			if scenario != "stale" {
				outbox := mock.ExpectExec("INSERT INTO scheduler_outbox")
				if scenario == "outbox failure" {
					outbox.WillReturnError(errors.New("outbox unavailable"))
				} else {
					outbox.WillReturnResult(sqlmock.NewResult(1, 1))
					state := mock.ExpectExec("INSERT INTO settings").WithArgs(service.AccountRiskSettingKey, sqlmock.AnyArg())
					if scenario == "state failure" {
						state.WillReturnError(errors.New("state unavailable"))
					} else {
						state.WillReturnResult(sqlmock.NewResult(1, 1))
					}
				}
			}
			failed := scenario == "state failure" || scenario == "outbox failure"
			if failed {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}
			err = NewAccountRiskStore(db, nil).Update(context.Background(), func(state *service.AccountRiskState, scheduling service.AccountRiskScheduling) error {
				changed, err := scheduling.SetAPIFallback(7, &service.Account{ID: 99, UpdatedAt: at}, "lease", enabled)
				if err != nil {
					return err
				}
				if scenario == "stale" {
					require.False(t, changed)
					return nil
				}
				require.True(t, changed)
				state.APIFallbackLeases = map[int64]service.AccountRiskAPIFallbackLease{99: {Token: "lease"}}
				return nil
			})
			if failed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
