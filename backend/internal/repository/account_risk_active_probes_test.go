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

func TestAccountRiskActiveProbesSurviveLegacyRewriteWithGuard(t *testing.T) {
	now := time.Now().UTC()
	state := &service.AccountRiskState{Rules: map[int64]*service.AccountRiskRule{}, ActiveProbes: map[int64]*service.AccountRiskPending{
		1: {ID: "first", GroupID: 7, AccountID: 1, Kind: "all_probe", ExpiresAt: now.Add(time.Minute)},
		2: {ID: "second", GroupID: 7, AccountID: 2, Kind: "all_probe", ExpiresAt: now.Add(2 * time.Minute)},
	}}
	syncAccountRiskLegacyGuard(state)
	require.Equal(t, service.AccountRiskConcurrentGuard, state.Pending.Kind)
	require.Equal(t, now.Add(2*time.Minute), state.Pending.ExpiresAt)
	legacy, err := encodeLegacyAccountRiskState(state)
	require.NoError(t, err)
	require.NotContains(t, string(legacy), "active_probes")
	// Re-encoding only known legacy fields cannot remove the separate leases.
	var old struct {
		Rules   map[int64]*service.AccountRiskRule `json:"rules"`
		Pending *service.AccountRiskPending        `json:"pending,omitempty"`
	}
	require.NoError(t, json.Unmarshal(legacy, &old))
	legacy, err = json.Marshal(old)
	require.NoError(t, err)
	leases, err := json.Marshal(state.ActiveProbes)
	require.NoError(t, err)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectQuery("SELECT value FROM settings").WithArgs(service.AccountRiskSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(legacy)))
	mock.ExpectQuery("SELECT value FROM settings").WithArgs(accountRiskCapacityPausesSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}))
	mock.ExpectQuery("SELECT value FROM settings").WithArgs(accountRiskActiveProbesSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(string(leases)))
	reloaded, err := NewAccountRiskStore(db, nil).Read(context.Background())
	require.NoError(t, err)
	require.Len(t, reloaded.ActiveProbes, 2)
	require.Equal(t, "second", reloaded.ActiveProbes[2].ID)
	delete(reloaded.ActiveProbes, 2)
	syncAccountRiskLegacyGuard(reloaded)
	require.True(t, now.Add(time.Minute).Equal(reloaded.Pending.ExpiresAt))
	delete(reloaded.ActiveProbes, 1)
	syncAccountRiskLegacyGuard(reloaded)
	require.Nil(t, reloaded.Pending)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountRiskActiveProbesAndLegacyGuardCommitAtomically(t *testing.T) {
	for _, fail := range []bool{false, true} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		mock.ExpectBegin()
		mock.ExpectQuery("SELECT pg_try_advisory_xact_lock").WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
		for _, key := range []string{service.AccountRiskSettingKey, accountRiskCapacityPausesSettingKey, accountRiskActiveProbesSettingKey} {
			mock.ExpectQuery("SELECT value FROM settings").WithArgs(key).WillReturnRows(sqlmock.NewRows([]string{"value"}))
		}
		mock.ExpectExec("INSERT INTO settings").WithArgs(service.AccountRiskSettingKey, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
		write := mock.ExpectExec("INSERT INTO settings").WithArgs(accountRiskActiveProbesSettingKey, sqlmock.AnyArg())
		if fail {
			write.WillReturnError(errors.New("disk full"))
			mock.ExpectRollback()
		} else {
			write.WillReturnResult(sqlmock.NewResult(1, 1))
			mock.ExpectCommit()
		}
		err = NewAccountRiskStore(db, nil).Update(context.Background(), func(state *service.AccountRiskState, _ service.AccountRiskScheduling) error {
			state.ActiveProbes = map[int64]*service.AccountRiskPending{1: {ID: "one", GroupID: 7, AccountID: 1, ExpiresAt: time.Now().Add(time.Minute), Kind: "all_probe"}}
			return nil
		})
		if fail {
			require.ErrorContains(t, err, "disk full")
		} else {
			require.NoError(t, err)
		}
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	}
}

func TestAccountRiskInvalidActiveProbesFailClosed(t *testing.T) {
	for _, raw := range []string{`not json`, `{"1":null}`, `{"1":{"id":"x","group_id":7,"account_id":2,"expires_at":"2030-01-01T00:00:00Z"}}`, `{"1":{"id":"x","group_id":7,"account_id":1}}`} {
		db, mock, err := sqlmock.New()
		require.NoError(t, err)
		mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
		mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}))
		mock.ExpectQuery("SELECT value FROM settings").WithArgs(accountRiskActiveProbesSettingKey).WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(raw))
		_, err = NewAccountRiskStore(db, nil).Read(context.Background())
		require.Error(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
		db.Close()
	}
}
