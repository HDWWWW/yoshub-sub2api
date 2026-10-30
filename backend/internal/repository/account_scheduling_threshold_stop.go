package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.AccountSchedulingThresholdStopper = (*accountRepository)(nil)

func (r *accountRepository) StopAccountAtSchedulingThreshold(ctx context.Context, id int64) (bool, error) {
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if dbent.TxFromContext(ctx) == nil {
		var err error
		tx, err = client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return false, err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			client = tx.Client()
		}
	}
	blocked, changed, err := stopAccountAtSchedulingThreshold(ctx, client, id)
	if err != nil {
		return false, err
	}
	if changed {
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			return false, err
		}
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		if changed {
			r.syncSchedulerAccountSnapshot(ctx, id)
		}
	}
	return blocked, nil
}

// Caller owns the transaction and publishes the outbox event after any change.
// A fresh row lock prevents a stale scheduler snapshot from disabling a renewed
// window or overwriting a concurrently changed account override.
func stopAccountAtSchedulingThreshold(ctx context.Context, client *dbent.Client, id int64) (blocked, changed bool, err error) {
	stored, err := client.Account.Query().Where(dbaccount.IDEQ(id), dbaccount.DeletedAtIsNil()).ForUpdate().Only(ctx)
	if err != nil {
		return false, false, translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}
	account := accountEntityToService(stored)
	if !account.Schedulable {
		return true, false, nil
	}
	if !service.ShouldStopAccountAtSchedulingThreshold(account, time.Now().UTC()) {
		return false, false, nil
	}
	update := client.Account.UpdateOneID(id).SetSchedulable(false)
	// Convert only a previous threshold pause; preserve unrelated error cooldowns.
	if service.IsAccountSchedulingThresholdReason(account.TempUnschedulableReason) {
		update.ClearTempUnschedulableUntil().ClearTempUnschedulableReason()
	}
	if _, err := update.Save(ctx); err != nil {
		return false, false, err
	}
	return true, true, nil
}

func hasSchedulingThresholdUsageUpdates(updates map[string]any) bool {
	for key := range updates {
		if key == "codex_usage_updated_at" {
			return true
		}
		for _, prefix := range []string{"codex_5h_", "codex_7d_", "passive_usage_", "session_window_", "grok_sched_", "kimi_", "zhipu_", "minimax_", "opencode_", "command_code_"} {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}
	}
	return false
}
