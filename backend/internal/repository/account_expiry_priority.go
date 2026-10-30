package repository

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.AccountExpiryPriorityRepository = (*accountRepository)(nil)

// SyncAccountExpiryPriorities ranks unexpired accounts first (earliest expiry wins),
// undated accounts next (preserving their relative manual priority), and expired
// accounts last. Equal expiry instants share a rank for normal load balancing.
// The update and outbox event commit together: failed notification rolls back
// the priorities, and unchanged scans neither write rows nor emit events.
func (r *accountRepository) SyncAccountExpiryPriorities(ctx context.Context, now time.Time) (int64, error) {
	var changed int64
	err := scanSingleRow(ctx, r.sql, `
		WITH ranked AS MATERIALIZED (
			SELECT id, expires_at, priority,
				DENSE_RANK() OVER (ORDER BY
					CASE WHEN expires_at > $1 THEN 0 WHEN expires_at IS NULL THEN 1 ELSE 2 END,
					CASE WHEN expires_at > $1 THEN expires_at END ASC NULLS LAST,
					CASE WHEN expires_at IS NULL THEN priority END ASC NULLS LAST
				)::integer AS new_priority
			FROM accounts
			WHERE deleted_at IS NULL
		), updated AS (
			UPDATE accounts AS a
			SET priority = r.new_priority, updated_at = NOW()
			FROM ranked AS r
			WHERE a.id = r.id AND a.deleted_at IS NULL
				AND a.priority = r.priority
				AND a.expires_at IS NOT DISTINCT FROM r.expires_at
				AND a.priority IS DISTINCT FROM r.new_priority
			RETURNING a.id
		), notified AS (
			INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
			SELECT $2, NULL, NULL, jsonb_build_object('account_ids', jsonb_agg(id ORDER BY id))
			FROM updated
			HAVING COUNT(*) > 0
			RETURNING id
		)
		SELECT COUNT(*) FROM updated
	`, []any{now, service.SchedulerOutboxEventAccountBulkChanged}, &changed)
	return changed, err
}
