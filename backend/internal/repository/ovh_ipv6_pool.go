package repository

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
)

func ipv6SettingsTable(ctx context.Context) string {
	if ovhipv6.SelectedPool(ctx) == ovhipv6.Backup {
		return "yoshub_ovh_ipv6_backup_settings"
	}
	return "yoshub_ovh_ipv6_settings"
}

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

// The switch, rotation cursor and new account binding share one transaction.
func (r *proxyRepository) selectStandbyIPv6(ctx context.Context, mode string) (int64, error) {
	ruleID, refill := ovhipv6.RefillRule(ctx)
	if !refill && !ovhipv6.IsAccountEdit(ctx) {
		return 0, fmt.Errorf("请在补号规则或单账号编辑中选择地址池")
	}
	tx := dbent.TxFromContext(ctx)
	if tx == nil {
		return 0, fmt.Errorf("IPv6 分配需要账号写事务")
	}
	if refill || mode == "ovh_ipv6_next" {
		var enabled bool
		if err := scanSingleRow(ctx, tx.Client(), `SELECT enabled FROM yoshub_ovh_ipv6_backup_settings WHERE id=1 FOR SHARE`, nil, &enabled); err != nil {
			return 0, err
		}
		if !enabled {
			return 0, fmt.Errorf("请先开启备用池自动分配")
		}
	}
	pool := ovhipv6.Backup
	if mode == "ovh_ipv6_next" {
		err := scanSingleRow(ctx, tx.Client(), `INSERT INTO yoshub_ovh_ipv6_pool_rotation AS cursor (rule_id, next_pool)
            VALUES ($1, 'backup') ON CONFLICT (rule_id) DO UPDATE
            SET next_pool = CASE cursor.next_pool WHEN 'primary' THEN 'backup' ELSE 'primary' END
            RETURNING CASE next_pool WHEN 'primary' THEN 'backup' ELSE 'primary' END`, []any{ruleID}, &pool)
		if err != nil {
			return 0, err
		}
	}
	return r.allocateOVHIPv6(ovhipv6.WithPool(ctx, pool))
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
