package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Shared with automatic account allocation, including across blue/green slots.
const accountProxyAllocationLock = `SELECT pg_advisory_xact_lock(742319830125)`

func proxyLimitIDArgs(ids []int64) (string, []any) {
	placeholders, args := make([]string, len(ids)), make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	return strings.Join(placeholders, ","), args
}

func (r *proxyRepository) GetProxyBindingLimits(ctx context.Context, ids []int64) ([]service.ProxyBindingLimit, error) {
	if err := service.ValidateExtensionIDs(ids); err != nil {
		return nil, err
	}
	in, args := proxyLimitIDArgs(ids)
	rows, err := r.sql.QueryContext(ctx, `SELECT p.id, COALESCE(l.max_accounts,1),
      (SELECT COUNT(*) FROM accounts a WHERE a.proxy_id=p.id AND a.deleted_at IS NULL)
      FROM proxies p LEFT JOIN yoshub_proxy_binding_limits l ON l.proxy_id=p.id
      WHERE p.deleted_at IS NULL AND p.id IN (`+in+`) ORDER BY p.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]service.ProxyBindingLimit, 0, len(ids))
	for rows.Next() {
		var item service.ProxyBindingLimit
		if err := rows.Scan(&item.ProxyID, &item.MaxAccounts, &item.AccountCount); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *proxyRepository) SetProxyBindingLimits(ctx context.Context, ids []int64, maxAccounts int) error {
	if err := service.ValidateExtensionIDs(ids); err != nil {
		return err
	}
	if maxAccounts < 1 || maxAccounts > 100000 {
		return fmt.Errorf("invalid proxy binding limit")
	}
	if maxAccounts != 1 {
		managed, err := r.ManagedIPv6Addresses(ctx, ids)
		if err != nil {
			return err
		}
		if len(managed) > 0 {
			return fmt.Errorf("OVH IPv6 固定一号一 IP，绑定上限只能为 1")
		}
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	if _, err = client.ExecContext(ctx, accountProxyAllocationLock); err != nil {
		return err
	}
	in, args := proxyLimitIDArgs(ids)
	args = append(args, maxAccounts)
	result, err := client.ExecContext(ctx, `INSERT INTO yoshub_proxy_binding_limits(proxy_id,max_accounts)
      SELECT id,$`+fmt.Sprint(len(args))+` FROM proxies WHERE deleted_at IS NULL AND id IN (`+in+`)
      ON CONFLICT(proxy_id) DO UPDATE SET max_accounts=EXCLUDED.max_accounts,updated_at=NOW()`, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != int64(len(ids)) {
		return service.ErrProxyNotFound
	}
	return tx.Commit()
}
