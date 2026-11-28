package repository

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
)

func (r *proxyRepository) ipv6Client(pool ovhipv6.Pool) *ovhipv6.Client {
	if pool == ovhipv6.Backup {
		return r.ipv6Backup
	}
	return r.ipv6
}

func (r *proxyRepository) ListIPv6Retirements(ctx context.Context, before int64) ([]ovhipv6.Retirement, error) {
	rows, err := r.sql.QueryContext(ctx, `SELECT id,host(address),pool,retired_at FROM yoshub_ovh_ipv6_retirements WHERE ($1::bigint=0 OR id<$1) ORDER BY id DESC LIMIT 50`, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ovhipv6.Retirement, 0, 50)
	for rows.Next() {
		var item ovhipv6.Retirement
		if err := rows.Scan(&item.ID, &item.Address, &item.Pool, &item.RetiredAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// The cursor moves in the same transaction as the completed account edit.
func (r *proxyRepository) nextManualIPv6Pool(ctx context.Context) (ovhipv6.Pool, error) {
	tx := dbent.TxFromContext(ctx)
	if tx == nil {
		return "", fmt.Errorf("IPv6 轮转需要账号写事务")
	}
	var pool ovhipv6.Pool
	err := scanSingleRow(ctx, tx.Client(), `UPDATE yoshub_ovh_ipv6_settings
 SET manual_next_pool = CASE manual_next_pool WHEN 'primary' THEN 'backup' ELSE 'primary' END
 WHERE id=1 AND backup_enabled
 RETURNING CASE manual_next_pool WHEN 'primary' THEN 'backup' ELSE 'primary' END`, nil, &pool)
	if err != nil {
		return "", fmt.Errorf("主备轮转无法分配，请确认备用自动分配开关已开启: %w", err)
	}
	return pool, nil
}

func validateLocalLeaseID(id string) error {
	if id == "" || len(id) > 40 || id[0] == '0' {
		return fmt.Errorf("invalid IPv6 lease ID")
	}
	for _, d := range id {
		if d < '0' || d > '9' {
			return fmt.Errorf("invalid IPv6 lease ID")
		}
	}
	return nil
}
