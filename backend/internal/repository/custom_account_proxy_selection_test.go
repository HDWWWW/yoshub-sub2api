//go:build unit

package repository

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountProxySelectionLargePoolAndTransactionalLeastUsed(t *testing.T) {
	db := scopedAdminKeyTestDB(t)
	_, err := db.Exec(`CREATE TABLE proxies(
 id BIGINT PRIMARY KEY,name TEXT DEFAULT '',protocol TEXT DEFAULT 'http',host TEXT DEFAULT '192.0.2.1',port INT DEFAULT 8080,
 username TEXT DEFAULT 'SECRET_USER',password TEXT DEFAULT 'SECRET_PASSWORD',status TEXT DEFAULT 'active',
 expires_at TIMESTAMPTZ,deleted_at TIMESTAMPTZ,created_at TIMESTAMPTZ DEFAULT NOW(),updated_at TIMESTAMPTZ DEFAULT NOW(),
 fallback_mode TEXT DEFAULT 'none',backup_proxy_id BIGINT,expiry_warn_days INT DEFAULT 7);
 CREATE TABLE accounts(id BIGINT PRIMARY KEY,proxy_id BIGINT,deleted_at TIMESTAMPTZ);
 INSERT INTO proxies(id) SELECT generate_series(1,20005);
 UPDATE proxies SET status='inactive' WHERE id=1;
 UPDATE proxies SET status='inactive' WHERE id=2;
 UPDATE proxies SET expires_at=NOW()-INTERVAL '1 hour' WHERE id=3;
 UPDATE proxies SET deleted_at=NOW() WHERE id=4;
 INSERT INTO accounts VALUES(1,5,NULL),(2,6,NULL),(3,7,NULL),(4,8,NOW());
`)
	require.NoError(t, err)
	installProxyBindingLimitSchema(t, db)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newProxyRepositoryWithSQL(client, db)
	// 真实 2 万条池只返回指定页；不用账户聚合管理列表。
	items, page, err := repo.ListOptions(context.Background(), pagination.PaginationParams{Page: 2, PageSize: 100}, "")
	require.NoError(t, err)
	require.Len(t, items, 100)
	require.EqualValues(t, 20002, page.Total)
	require.EqualValues(t, 19905, items[0].ID)
	_, err = repo.SelectAccountProxy(context.Background(), "random")
	require.Error(t, err)
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()
	ctx := dbent.NewTxContext(context.Background(), tx)
	selected, err := repo.SelectAccountProxy(ctx, "least_used")
	require.NoError(t, err)
	require.EqualValues(t, 8, selected, "软删账户不占用")
	_, err = tx.Client().ExecContext(ctx, `INSERT INTO accounts VALUES(10,$1,NULL)`, selected)
	require.NoError(t, err)
	// 第二个事务必须等待第一个事务绑定提交，随后观察到新的计数。
	started := make(chan struct{})
	done := make(chan struct {
		id  int64
		err error
	}, 1)
	go func() {
		other, err := client.Tx(context.Background())
		if err != nil {
			done <- struct {
				id  int64
				err error
			}{0, err}
			return
		}
		defer other.Rollback()
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c = dbent.NewTxContext(c, other)
		close(started)
		id, err := repo.SelectAccountProxy(c, "least_used")
		done <- struct {
			id  int64
			err error
		}{id, err}
	}()
	<-started
	select {
	case result := <-done:
		t.Fatalf("选择不应越过尚未提交的分配: %+v", result)
	case <-time.After(30 * time.Millisecond):
	}
	require.NoError(t, tx.Commit())
	result := <-done
	require.NoError(t, result.err)
	require.EqualValues(t, 9, result.id)
	// 候选查询不能通过用户名搜索探测认证字段。
	scopedCtx := adminaccess.WithPrincipal(context.Background(), &adminaccess.Principal{Policy: adminaccess.Policy{GlobalPermissions: []string{"proxies.read"}}})
	matches, meta, err := repo.ListOptions(scopedCtx, pagination.PaginationParams{Page: 1, PageSize: 100}, "SECRET_USER")
	require.NoError(t, err)
	require.Empty(t, matches)
	require.Zero(t, meta.Total)
	matches, meta, err = repo.ListOptions(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 100}, "SECRET_USER")
	require.NoError(t, err)
	require.Empty(t, matches)
	require.Zero(t, meta.Total)
	// 限定唯一可用候选，确定性验证随机排除过期、停用及已删除。
	_, err = db.Exec(`UPDATE proxies SET status='inactive' WHERE id>9;UPDATE proxies SET status='inactive' WHERE id IN (5,6,7,8)`)
	require.NoError(t, err)
	tx, err = client.Tx(context.Background())
	require.NoError(t, err)
	ctx = dbent.NewTxContext(context.Background(), tx)
	selected, err = repo.SelectAccountProxy(ctx, "random")
	require.NoError(t, err)
	require.EqualValues(t, 9, selected)
	require.NoError(t, tx.Rollback())
	// 所有启用代理都已绑定时，最空闲也不能复用。
	_, err = db.Exec(`UPDATE proxies SET status='active' WHERE id IN(5,6,7);UPDATE proxies SET status='inactive' WHERE id=9`)
	require.NoError(t, err)
	tx, err = client.Tx(context.Background())
	require.NoError(t, err)
	ctx = dbent.NewTxContext(context.Background(), tx)
	selected, err = repo.SelectAccountProxy(ctx, "least_used")
	require.NoError(t, err)
	require.Zero(t, selected)
	require.NoError(t, tx.Rollback())
	_, err = db.Exec(`UPDATE proxies SET status='inactive'`)
	require.NoError(t, err)
	tx, err = client.Tx(context.Background())
	require.NoError(t, err)
	ctx = dbent.NewTxContext(context.Background(), tx)
	selected, err = repo.SelectAccountProxy(ctx, "random")
	require.NoError(t, err)
	require.Zero(t, selected)
	require.NoError(t, tx.Rollback())
	require.NoError(t, db.Ping(), "测试结束连接可用")
}

func TestAccountProxySelectionConcurrentRandomAndLeastUsedExclusive(t *testing.T) {
	db := scopedAdminKeyTestDB(t)
	_, err := db.Exec(`CREATE TABLE proxies(id BIGINT PRIMARY KEY,status TEXT DEFAULT 'active',expires_at TIMESTAMPTZ,deleted_at TIMESTAMPTZ);
 CREATE TABLE accounts(id BIGINT PRIMARY KEY,proxy_id BIGINT,deleted_at TIMESTAMPTZ,status TEXT DEFAULT 'inactive');
 INSERT INTO proxies(id) SELECT generate_series(1,8);`)
	require.NoError(t, err)
	installProxyBindingLimitSchema(t, db)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newProxyRepositoryWithSQL(client, db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := make(chan struct{})
	errors := make(chan error, 24)
	var wg sync.WaitGroup
	for i := 1; i <= 24; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			tx, e := client.Tx(ctx)
			if e != nil {
				errors <- e
				return
			}
			defer tx.Rollback()
			txctx := dbent.NewTxContext(ctx, tx)
			mode := "random"
			if id%2 == 0 {
				mode = "least_used"
			}
			proxyID, e := repo.SelectAccountProxy(txctx, mode)
			if e == nil && proxyID > 0 {
				_, e = tx.Client().ExecContext(txctx, `INSERT INTO accounts(id,proxy_id) VALUES($1,$2)`, id, proxyID)
			}
			if e == nil {
				e = tx.Commit()
			}
			errors <- e
		}(i)
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var total, distinct int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT proxy_id) FROM accounts`).Scan(&total, &distinct))
	require.Equal(t, 8, total)
	require.Equal(t, total, distinct, "随机、最空闲和补号共用的选择器不得重复分配，停用账号也占用")
	free, err := repo.HasFreeAccountProxy(ctx)
	require.NoError(t, err)
	require.False(t, free)
	// Rollback must release the lock and leave no reservation. Deleted accounts release their proxy.
	_, err = db.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE proxy_id=1`)
	require.NoError(t, err)
	free, err = repo.HasFreeAccountProxy(ctx)
	require.NoError(t, err)
	require.True(t, free)
	for i := 0; i < 2; i++ {
		tx, err := client.Tx(ctx)
		require.NoError(t, err)
		id, err := repo.SelectAccountProxy(dbent.NewTxContext(ctx, tx), "random")
		require.NoError(t, err)
		require.EqualValues(t, 1, id)
		require.NoError(t, tx.Rollback())
	}
}

func installProxyBindingLimitSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "250_yoshub_proxy_binding_limits.sql"))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
}

func TestNativeStaleAccountWritePreservesCurrentManualPriority(t *testing.T) {
	db := scopedAdminKeyTestDB(t)
	_, err := db.Exec(`CREATE TABLE accounts(id BIGINT PRIMARY KEY,platform TEXT,type TEXT,proxy_id BIGINT,
	 credentials JSONB DEFAULT '{}'::jsonb, extra JSONB DEFAULT '{}'::jsonb,deleted_at TIMESTAMPTZ);
	 INSERT INTO accounts(id,platform,type,extra) VALUES(1,'openai','oauth','{"yoshub_manual_scheduling_priority":0}');`)
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	tx, err := client.Tx(context.Background())
	require.NoError(t, err)
	defer tx.Rollback()
	stale := &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Extra: map[string]any{service.AccountManualPriorityExtraKey: 9, "supplier": true}}
	merged, err := lockAndMergeAccountProbeExtra(context.Background(), tx.Client(), stale, nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 0, merged[service.AccountManualPriorityExtraKey])
	require.Equal(t, true, merged["supplier"])
}

func TestProxyBindingLimitsCapacityAndConcurrentAllocation(t *testing.T) {
	db := scopedAdminKeyTestDB(t)
	_, err := db.Exec(`CREATE TABLE proxies(id BIGINT PRIMARY KEY,status TEXT DEFAULT 'active',expires_at TIMESTAMPTZ,deleted_at TIMESTAMPTZ);
 CREATE TABLE accounts(id BIGINT PRIMARY KEY,proxy_id BIGINT,deleted_at TIMESTAMPTZ);
 INSERT INTO proxies(id) VALUES(1),(2);`)
	require.NoError(t, err)
	installProxyBindingLimitSchema(t, db)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newProxyRepositoryWithSQL(client, db)
	ctx := context.Background()
	limits, err := repo.GetProxyBindingLimits(ctx, []int64{1, 2})
	require.NoError(t, err)
	require.Equal(t, 1, limits[0].MaxAccounts)
	require.NoError(t, repo.SetProxyBindingLimits(ctx, []int64{1, 2}, 3))
	require.Error(t, repo.SetProxyBindingLimits(ctx, []int64{1, 999}, 9), "missing IDs must rollback whole batch")
	limits, err = repo.GetProxyBindingLimits(ctx, []int64{1})
	require.NoError(t, err)
	require.Equal(t, 3, limits[0].MaxAccounts)
	require.Error(t, repo.SetProxyBindingLimits(ctx, []int64{1}, 0))
	var wg sync.WaitGroup
	failures := make(chan error, 32)
	for i := 1; i <= 32; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tx, e := client.Tx(ctx)
			if e != nil {
				failures <- e
				return
			}
			defer tx.Rollback()
			c := dbent.NewTxContext(ctx, tx)
			mode := "random"
			if id%2 == 0 {
				mode = "least_used"
			}
			proxy, e := repo.SelectAccountProxy(c, mode)
			if e == nil && proxy > 0 {
				_, e = tx.Client().ExecContext(c, `INSERT INTO accounts(id,proxy_id) VALUES($1,$2)`, id, proxy)
			}
			if e == nil {
				e = tx.Commit()
			}
			failures <- e
		}(i)
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		require.NoError(t, e)
	}
	limits, err = repo.GetProxyBindingLimits(ctx, []int64{1, 2})
	require.NoError(t, err)
	for _, limit := range limits {
		require.EqualValues(t, 3, limit.AccountCount)
	}
	free, err := repo.HasFreeAccountProxy(ctx)
	require.NoError(t, err)
	require.False(t, free)
	require.NoError(t, repo.SetProxyBindingLimits(ctx, []int64{1, 2}, 1))
	limits, err = repo.GetProxyBindingLimits(ctx, []int64{1, 2})
	require.NoError(t, err)
	require.EqualValues(t, 3, limits[0].AccountCount, "lowering never evicts existing accounts")
	free, err = repo.HasFreeAccountProxy(ctx)
	require.NoError(t, err)
	require.False(t, free)
	// Capacity expansion immediately makes exactly one additional slot available.
	require.NoError(t, repo.SetProxyBindingLimits(ctx, []int64{1}, 4))
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	id, err := repo.SelectAccountProxy(dbent.NewTxContext(ctx, tx), "least_used")
	require.NoError(t, err)
	require.EqualValues(t, 1, id)
}
