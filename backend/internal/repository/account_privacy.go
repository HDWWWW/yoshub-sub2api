package repository

import (
	"context"
	"errors"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// UpdateOpenAIPrivacyMode publishes an upstream outcome only for the current
// saved preference. The JSON boolean false is the only opt-in value; absent or
// legacy values keep the privacy-enabled default used by the service.
func (r *accountRepository) UpdateOpenAIPrivacyMode(ctx context.Context, accountID int64, enabled bool, mode string) (bool, error) {
	if scoped := r.accountTransactionScope(ctx); scoped != r {
		return scoped.UpdateOpenAIPrivacyMode(ctx, accountID, enabled, mode)
	}
	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if contextTx == nil {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return false, err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}
	result, err := client.ExecContext(ctx, `UPDATE accounts
SET extra = COALESCE(extra, '{}'::jsonb) || jsonb_build_object('privacy_mode', $2::text), updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL AND platform = 'openai' AND type = 'oauth' AND parent_account_id IS NULL
AND ((extra -> 'openai_privacy_enabled') IS DISTINCT FROM 'false'::jsonb) = $3`, accountID, mode, enabled)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &accountID, nil, nil); err != nil {
		return false, err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return false, err
		}
	}
	if contextTx == nil {
		r.syncSchedulerAccountSnapshot(baseCtx, accountID)
	}
	return true, nil
}
