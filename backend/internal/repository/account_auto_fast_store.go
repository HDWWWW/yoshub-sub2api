package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type accountAutoFastStore struct {
	db       *sql.DB
	accounts *accountRepository
}

func NewAccountAutoFastStore(db *sql.DB, accounts service.AccountRepository) service.AccountAutoFastStore {
	repo, _ := accounts.(*accountRepository)
	return &accountAutoFastStore{db: db, accounts: repo}
}

func decodeAccountAutoFastState(raw string) (*service.AccountAutoFastState, error) {
	state := &service.AccountAutoFastState{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), state); err != nil {
			return nil, err
		}
	}
	if state.Rules == nil {
		state.Rules = map[int64]*service.AccountAutoFastRule{}
	}
	if state.Managed == nil {
		state.Managed = map[int64]service.AccountAutoFastManaged{}
	}
	if len(state.Rules) > 32 || len(state.Managed) > 5000 {
		return nil, errors.New("auto fast state exceeds bounds")
	}
	for id, rule := range state.Rules {
		if id <= 0 || rule == nil || rule.GroupID != id || rule.Config.Validate() != nil || len(rule.Events) > 50 || len(rule.Accounts) > 200 {
			return nil, errors.New("invalid auto fast rule")
		}
	}
	for id, managed := range state.Managed {
		if id <= 0 || state.Rules[managed.GroupID] == nil {
			return nil, errors.New("invalid auto fast ownership")
		}
	}
	return state, nil
}

func (s *accountAutoFastStore) Read(ctx context.Context) (*service.AccountAutoFastState, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", service.AccountAutoFastSettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return decodeAccountAutoFastState(raw)
}

// One transaction coordinates every replica/blue-green slot. A scheduler
// outbox failure rolls back both the policy change and its ownership record.
func (s *accountAutoFastStore) Update(ctx context.Context, fn func(*service.AccountAutoFastState, service.AccountAutoFastApply) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var locked bool
	if err = tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", int64(8362461180575)).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return service.ErrAccountAutoFastBusy
	}
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", service.AccountAutoFastSettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	state, err := decodeAccountAutoFastState(raw)
	if err != nil {
		return err
	}
	before, _ := json.Marshal(state)
	changed := []int64{}
	apply := func(groupID int64, a *service.Account, enabled bool) (bool, error) {
		patch, err := json.Marshal(map[string]bool{service.OpenAIForceFastExtraKey: enabled})
		if err != nil {
			return false, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET extra=COALESCE(extra,'{}'::jsonb) || $1::jsonb, updated_at=NOW()
		 WHERE id=$2 AND updated_at=$3 AND deleted_at IS NULL AND platform='openai'
		 AND ($4::bigint=0 OR (status='active' AND EXISTS
		 (SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id
		 WHERE ag.account_id=accounts.id AND ag.group_id=$4 AND g.deleted_at IS NULL AND g.status='active' AND g.platform='openai')))`, string(patch), a.ID, a.UpdatedAt, groupID)
		if err != nil {
			return false, err
		}
		n, err := result.RowsAffected()
		if err != nil || n == 0 {
			return false, err
		}
		if err = enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &a.ID, nil, nil); err != nil {
			return false, err
		}
		changed = append(changed, a.ID)
		return true, nil
	}
	if err = fn(state, apply); err != nil {
		return err
	}
	after, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if string(before) != string(after) {
		_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,$2,NOW())
		 ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=NOW()`, service.AccountAutoFastSettingKey, string(after))
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if s.accounts != nil && len(changed) > 0 {
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		s.accounts.syncSchedulerAccountSnapshots(syncCtx, changed)
	}
	return nil
}
