package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// A separate transaction-scoped lock coordinates all application slots. No
// upstream request runs while this lock is held, and it never locks refill.
type accountRiskStore struct {
	db       *sql.DB
	accounts *accountRepository
}

func NewAccountRiskStore(db *sql.DB, accounts service.AccountRepository) service.AccountRiskStore {
	repo, _ := accounts.(*accountRepository)
	return &accountRiskStore{db: db, accounts: repo}
}

func decodeAccountRiskState(raw string) (*service.AccountRiskState, error) {
	state := &service.AccountRiskState{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), state); err != nil {
			return nil, err
		}
	}
	if state.Rules == nil {
		state.Rules = map[int64]*service.AccountRiskRule{}
	}
	if len(state.Rules) > 32 {
		return nil, errors.New("risk rule limit exceeded")
	}
	for id, rule := range state.Rules {
		if rule == nil || rule.GroupID != id || rule.Config.Validate() != nil {
			return nil, errors.New("invalid risk rule")
		}
	}
	return state, nil
}

func (s *accountRiskStore) Read(ctx context.Context) (*service.AccountRiskState, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", service.AccountRiskSettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return decodeAccountRiskState(raw)
}

func (s *accountRiskStore) Update(ctx context.Context, fn func(*service.AccountRiskState, service.AccountRiskStop) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var locked bool
	if err = tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", int64(8362461180574)).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return service.ErrAccountRiskBusy
	}
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1 FOR UPDATE", service.AccountRiskSettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	state, err := decodeAccountRiskState(raw)
	if err != nil {
		return err
	}
	before, _ := json.Marshal(state)
	changed := []int64{}
	stop := func(groupID int64, a *service.Account) (bool, error) {
		// The rule and account mutation commit together. A concurrent admin edit,
		// credential refresh or membership change prevents a stale stop.
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET schedulable=false, updated_at=NOW()
		 WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL AND status='active' AND schedulable=true
		 AND EXISTS (SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id
		 WHERE ag.account_id=accounts.id AND ag.group_id=$3 AND g.deleted_at IS NULL AND g.status='active')`, a.ID, a.UpdatedAt, groupID)
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
	if err = fn(state, stop); err != nil {
		return err
	}
	after, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if string(before) != string(after) {
		_, err = tx.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) VALUES ($1,$2,NOW())
		 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,updated_at=NOW()`, service.AccountRiskSettingKey, string(after))
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
