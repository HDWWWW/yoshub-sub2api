package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// The deadline and policy are one atomic edit. A plain JSON merge followed by a
// second UPDATE would race the expiry worker and native account edits.
func (r *accountRepository) updateAccountExpiryExtra(ctx context.Context, id int64, updates map[string]any) error {
	baseCtx := ctx
	contextTx := dbent.TxFromContext(ctx)
	client := clientFromContext(ctx, r.client)
	var tx *dbent.Tx
	if contextTx == nil {
		var err error
		tx, err = r.client.Tx(ctx)
		if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
			return err
		}
		if tx != nil {
			defer func() { _ = tx.Rollback() }()
			ctx = dbent.NewTxContext(ctx, tx)
			client = tx.Client()
		}
	}
	rows, err := client.QueryContext(ctx, "SELECT platform, type, expires_at, extra FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE", id)
	if err != nil {
		return err
	}
	a := &service.Account{ID: id}
	var expiry sql.NullTime
	var raw []byte
	if !rows.Next() {
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
		return service.ErrAccountNotFound
	}
	err = rows.Scan(&a.Platform, &a.Type, &expiry, &raw)
	_ = rows.Close()
	if err != nil {
		return err
	}
	if expiry.Valid {
		a.ExpiresAt = &expiry.Time
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a.Extra); err != nil {
			return err
		}
	}
	service.ApplyAccountExpiryExtra(a, updates)
	payload, err := json.Marshal(a.Extra)
	if err != nil {
		return err
	}
	if _, err := client.ExecContext(ctx, "UPDATE accounts SET extra=$1::jsonb, expires_at=$2, updated_at=NOW() WHERE id=$3 AND deleted_at IS NULL", string(payload), a.ExpiresAt, id); err != nil {
		return err
	}
	if hasSchedulingThresholdUsageUpdates(updates) {
		if _, _, err := stopAccountAtSchedulingThreshold(ctx, client, id); err != nil {
			return err
		}
	}
	if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		return err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if contextTx == nil {
		r.syncSchedulerAccountSnapshot(baseCtx, id)
	}
	return nil
}
