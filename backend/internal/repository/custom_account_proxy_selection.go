package repository

import (
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
	"net/http"

	dbent "github.com/Wei-Shaw/sub2api/ent"
)

const freeAccountProxyPredicate = `p.deleted_at IS NULL AND p.status='active'
 AND (p.expires_at IS NULL OR p.expires_at>NOW())
 AND (SELECT COUNT(*) FROM accounts a WHERE a.proxy_id=p.id AND a.deleted_at IS NULL)
   < COALESCE((SELECT l.max_accounts FROM yoshub_proxy_binding_limits l WHERE l.proxy_id=p.id),1)`

func (r *proxyRepository) manualAccountProxyPredicate() string {
	predicate := freeAccountProxyPredicate
	if r.ipv6Supported {
		// Reserved IPv6 entries are explicit selections, never members of the
		// ordinary random/least-used pool, regardless of the global toggle.
		predicate += ` AND NOT EXISTS (SELECT 1 FROM yoshub_ovh_ipv6_leases l WHERE l.proxy_id=p.id)`
	}
	return predicate
}

// HasFreeAccountProxy is a read-only purchase preflight, not a reservation.
// Delivery always rechecks under SelectAccountProxy's transaction lock.
func (r *proxyRepository) HasFreeAccountProxy(ctx context.Context) (bool, error) {
	enabled, err := r.ovhIPv6Enabled(ctx)
	if err != nil {
		return false, err
	}
	if enabled {
		var status ovhipv6.Status
		err := r.ipv6.Call(ctx, http.MethodGet, "/v1/status", nil, &status)
		return err == nil && status.Available, err
	}
	var available bool
	err = scanSingleRow(ctx, r.sql, `SELECT EXISTS (SELECT 1 FROM proxies p WHERE `+r.manualAccountProxyPredicate()+`)`, nil, &available)
	return available, err
}

// SelectAccountProxy 在数据库内选出单个 ID，不把整个代理池或密码载入应用。
// 二开自动分配只使用未达绑定上限的代理。随机与最空闲共用事务锁，持有到账号绑定提交，
// 避免跨实例的推送和补号同时拿到同一个空闲代理；不改变原生显式绑定语义。
func (r *proxyRepository) SelectAccountProxy(ctx context.Context, mode string) (int64, error) {
	if mode == "ovh_ipv6" {
		return r.allocateOVHIPv6(ctx)
	}
	if mode == "random" {
		enabled, err := r.ovhIPv6Enabled(ctx)
		if err != nil {
			return 0, err
		}
		if enabled {
			return r.allocateOVHIPv6(ctx)
		}
	}
	tx := dbent.TxFromContext(ctx)
	if tx == nil {
		return 0, fmt.Errorf("代理选择需要写事务")
	}
	client := tx.Client()
	order := "random()"
	switch mode {
	case "random":
	case "least_used":
		order = "(SELECT COUNT(*) FROM accounts a WHERE a.proxy_id=p.id AND a.deleted_at IS NULL), p.id"
	default:
		return 0, fmt.Errorf("不支持的代理选择模式")
	}
	if _, err := client.ExecContext(ctx, accountProxyAllocationLock); err != nil {
		return 0, err
	}
	query := `SELECT p.id FROM proxies p WHERE ` + r.manualAccountProxyPredicate() + ` ORDER BY ` + order + ` LIMIT 1`
	rows, err := client.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	var id int64
	if rows.Next() {
		if err = rows.Scan(&id); err != nil {
			return 0, err
		}
	}
	return id, rows.Err()
}
