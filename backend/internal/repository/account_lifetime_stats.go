package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/lib/pq"
)

func (r *usageLogRepository) GetAccountLifetimeStatsBatch(ctx context.Context, ids []int64) (map[int64]*usagestats.AccountStats, error) {
	result := make(map[int64]*usagestats.AccountStats)
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := r.sql.QueryContext(ctx, `SELECT a.id, COALESCE(s.requests,0), COALESCE(s.tokens,0),
		COALESCE(s.cost,0), COALESCE(s.standard_cost,0), COALESCE(s.user_cost,0)
		FROM accounts a LEFT JOIN account_lifetime_stats s ON s.account_id=a.id
		WHERE a.id=ANY($1) AND a.deleted_at IS NULL`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		stats := &usagestats.AccountStats{}
		if err := rows.Scan(&id, &stats.Requests, &stats.Tokens, &stats.Cost, &stats.StandardCost, &stats.UserCost); err != nil {
			return nil, err
		}
		result[id] = stats
	}
	return result, rows.Err()
}
