package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// A separate transaction-scoped lock coordinates all application slots. No
// upstream request runs while this lock is held, and it never locks refill.
type accountRiskStore struct {
	db       *sql.DB
	accounts *accountRepository
}

// Keep the queue outside the legacy settings document: an old application slot
// may still update that document while its live requests drain during rollout.
const accountRiskCapacityPausesSettingKey = "account_risk_capacity_pauses_v1"

func readAccountRiskPauses(ctx context.Context, query func(context.Context, string, ...any) *sql.Row, state *service.AccountRiskState) error {
	var raw string
	err := query(ctx, "SELECT value FROM settings WHERE key=$1", accountRiskCapacityPausesSettingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		state.CapacityPauses = nil
		return nil
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal([]byte(raw), &state.CapacityPauses); err != nil {
		return err
	}
	for id, pause := range state.CapacityPauses {
		if pause == nil || id <= 0 || pause.AccountID != id || pause.GroupID <= 0 || pause.Token == "" {
			return errors.New("invalid capacity pause")
		}
	}
	return nil
}

func encodeLegacyAccountRiskState(state *service.AccountRiskState) ([]byte, error) {
	copy := *state
	copy.CapacityPauses = nil
	copy.ActiveProbes = nil
	return json.Marshal(&copy)
}

func NewAccountRiskStore(db *sql.DB, accounts service.AccountRepository) service.AccountRiskStore {
	repo, _ := accounts.(*accountRepository)
	return &accountRiskStore{db: db, accounts: repo}
}

// Read only routing metadata at receipt time. A temporary rate limit or OAuth
// cooldown does not turn off the account's explicit scheduling switch.
func (s *accountRiskStore) Lookup401AlertTarget(ctx context.Context, sourceID, ownerID int64) (service.Account401AlertTarget, error) {
	var target service.Account401AlertTarget
	err := s.db.QueryRowContext(ctx, `SELECT a.id, a.name, ARRAY(SELECT ag.group_id FROM account_groups ag WHERE ag.account_id=a.id)
		FROM accounts a WHERE a.id=$1 AND a.deleted_at IS NULL AND a.platform='openai' AND a.status='active' AND a.schedulable
		AND (a.id=$2 OR EXISTS (SELECT 1 FROM accounts owner WHERE owner.id=$2 AND owner.deleted_at IS NULL
			AND owner.platform='openai' AND owner.status='active' AND owner.schedulable))`, sourceID, ownerID).
		Scan(&target.AccountID, &target.Name, pq.Array(&target.GroupIDs))
	if errors.Is(err, sql.ErrNoRows) {
		return service.Account401AlertTarget{}, nil
	}
	return target, err
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
		if rule != nil {
			rule.Config.Normalize429()
		}
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
	state, err := decodeAccountRiskState(raw)
	if err != nil {
		return nil, err
	}
	if err = readAccountRiskPauses(ctx, s.db.QueryRowContext, state); err != nil {
		return nil, err
	}
	if err = readAccountRiskActiveProbes(ctx, s.db.QueryRowContext, state); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *accountRiskStore) Update(ctx context.Context, fn func(*service.AccountRiskState, service.AccountRiskScheduling) error) error {
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
	if err = readAccountRiskPauses(ctx, tx.QueryRowContext, state); err != nil {
		return err
	}
	if err = readAccountRiskActiveProbes(ctx, tx.QueryRowContext, state); err != nil {
		return err
	}
	before, _ := encodeLegacyAccountRiskState(state)
	probesBefore, _ := json.Marshal(state.ActiveProbes)
	pausesBefore, _ := json.Marshal(state.CapacityPauses)
	changed := []int64{}
	reduceConcurrency := func(groupID int64, a *service.Account, limit int) (bool, error) {
		// Nonpositive account concurrency means unlimited. Only lower a still-active
		// member's limit; keep its scheduling switch and any in-flight slots intact.
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET concurrency=$4, updated_at=NOW()
		 WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL AND platform='openai' AND status='active' AND schedulable
		 AND $4 > 0 AND (concurrency<=0 OR concurrency>$4)
		 AND EXISTS (SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id
		 WHERE ag.account_id=accounts.id AND ag.group_id=$3 AND g.deleted_at IS NULL AND g.status='active' AND g.platform='openai'
		 FOR SHARE OF ag,g)`, a.ID, a.UpdatedAt, groupID, limit)
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
	stop := func(groupID int64, a *service.Account) (bool, error) {
		// The rule and account mutation commit together. A concurrent admin edit,
		// credential refresh or membership change prevents a stale stop.
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET schedulable=false,
		 extra=COALESCE(extra,'{}'::jsonb)-'yoshub_risk_capacity_pause', updated_at=NOW()
		 WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL AND status='active'
		 AND (schedulable=true OR COALESCE(extra->>'yoshub_risk_capacity_pause','')<>'')
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
		delete(state.CapacityPauses, a.ID)
		return true, nil
	}
	pause := func(groupID int64, a *service.Account, token string) (bool, error) {
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET schedulable=false,
		 extra=jsonb_set(COALESCE(extra,'{}'::jsonb),'{yoshub_risk_capacity_pause}',to_jsonb($4::text)), updated_at=NOW()
		 WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL AND status='active' AND schedulable=true
		 AND EXISTS (SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id
		 WHERE ag.account_id=accounts.id AND ag.group_id=$3 AND g.deleted_at IS NULL AND g.status='active')`, a.ID, a.UpdatedAt, groupID, token)
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
	resume := func(groupID int64, a *service.Account, token string) (bool, error) {
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET schedulable=true,
		 extra=COALESCE(extra,'{}'::jsonb)-'yoshub_risk_capacity_pause', updated_at=NOW()
		 WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL AND status='active' AND schedulable=false
		 AND extra->>'yoshub_risk_capacity_pause'=$4
		 AND EXISTS (SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id
		 WHERE ag.account_id=accounts.id AND ag.group_id=$3 AND g.deleted_at IS NULL AND g.status='active')`, a.ID, a.UpdatedAt, groupID, token)
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
	apply401 := func(groupID int64, source, owner *service.Account, reason string, until *time.Time) (bool, error) {
		token, _ := source.Extra[service.AccountRiskCapacityPauseExtraKey].(string)
		// Lock the membership, source and credential owner through commit. A shadow
		// may use its parent's credentials without sharing the parent's group.
		result, err := tx.ExecContext(ctx, `WITH source AS (
          SELECT src.id FROM accounts src JOIN account_groups ag ON ag.account_id=src.id
          JOIN groups g ON g.id=ag.group_id
          WHERE src.id=$1 AND src.updated_at=$2 AND src.deleted_at IS NULL AND src.status='active'
          AND COALESCE(src.parent_account_id,src.id)=$3 AND ag.group_id=$5 AND g.deleted_at IS NULL AND g.status='active'
          AND ((src.schedulable AND $6='') OR (NOT src.schedulable AND $6<>'' AND src.extra->>'yoshub_risk_capacity_pause'=$6))
          FOR NO KEY UPDATE OF src FOR SHARE OF ag,g
        ) UPDATE accounts a SET
          status=CASE WHEN $7::timestamptz IS NULL THEN 'error' ELSE a.status END,
          schedulable=CASE WHEN $7::timestamptz IS NULL THEN false ELSE a.schedulable END,
          error_message=CASE WHEN $7::timestamptz IS NULL THEN $8 ELSE a.error_message END,
          extra=CASE WHEN $7::timestamptz IS NULL THEN COALESCE(a.extra,'{}'::jsonb)-'yoshub_risk_capacity_pause' ELSE a.extra END,
          temp_unschedulable_until=COALESCE($7,a.temp_unschedulable_until),
          temp_unschedulable_reason=CASE WHEN $7::timestamptz IS NULL THEN a.temp_unschedulable_reason ELSE $8 END, updated_at=NOW()
          WHERE EXISTS (SELECT 1 FROM source) AND a.id=$3 AND a.updated_at=$4 AND a.deleted_at IS NULL
          AND a.parent_account_id IS NULL AND a.status='active' AND (a.schedulable OR a.id=$1 AND $6<>'')
          AND ($7::timestamptz IS NULL OR a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=NOW())`,
			source.ID, source.UpdatedAt, owner.ID, owner.UpdatedAt, groupID, token, until, reason)
		if err != nil {
			return false, err
		}
		n, err := result.RowsAffected()
		if err != nil || n == 0 {
			return false, err
		}
		if err = enqueueSchedulerOutbox(ctx, tx, service.SchedulerOutboxEventAccountChanged, &owner.ID, nil, nil); err != nil {
			return false, err
		}
		if until == nil {
			delete(state.CapacityPauses, owner.ID)
		}
		changed = append(changed, owner.ID)
		return true, nil
	}
	if err = fn(state, service.AccountRiskScheduling{ApplyProbe401: apply401, ReduceConcurrency: reduceConcurrency, Stop: stop, Pause: pause, Resume: resume}); err != nil {
		return err
	}
	syncAccountRiskLegacyGuard(state)
	after, err := encodeLegacyAccountRiskState(state)
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
	pausesAfter, err := json.Marshal(state.CapacityPauses)
	if err != nil {
		return err
	}
	if string(pausesBefore) != string(pausesAfter) {
		_, err = tx.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) VALUES ($1,$2,NOW())
		 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,updated_at=NOW()`, accountRiskCapacityPausesSettingKey, string(pausesAfter))
		if err != nil {
			return err
		}
	}
	probesAfter, err := json.Marshal(state.ActiveProbes)
	if err != nil {
		return err
	}
	if string(probesBefore) != string(probesAfter) {
		_, err = tx.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) VALUES ($1,$2,NOW())
		 ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,updated_at=NOW()`, accountRiskActiveProbesSettingKey, string(probesAfter))
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
