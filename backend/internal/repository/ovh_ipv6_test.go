//go:build unit

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
	"github.com/stretchr/testify/require"
)

type ovhTestHost struct {
	mu             sync.Mutex
	issued         int
	draining, fail bool
	active         map[string]bool
}

func ovhRepoFixture(t *testing.T) (*sql.DB, *proxyRepository, *ovhTestHost) {
	t.Helper()
	db := scopedAdminKeyTestDB(t)
	_, err := db.Exec(`CREATE TABLE proxies(id BIGSERIAL PRIMARY KEY,name TEXT NOT NULL,protocol TEXT NOT NULL,host TEXT NOT NULL,port INT NOT NULL,username TEXT,password TEXT,status TEXT DEFAULT 'active',expires_at TIMESTAMPTZ,deleted_at TIMESTAMPTZ,created_at TIMESTAMPTZ DEFAULT NOW(),updated_at TIMESTAMPTZ DEFAULT NOW(),fallback_mode TEXT DEFAULT 'none',backup_proxy_id BIGINT REFERENCES proxies(id) ON DELETE SET NULL,expiry_warn_days INT DEFAULT 7);
CREATE TABLE accounts(id BIGSERIAL PRIMARY KEY,proxy_id BIGINT REFERENCES proxies(id) ON DELETE SET NULL,parent_account_id BIGINT,deleted_at TIMESTAMPTZ,status TEXT DEFAULT 'active',schedulable BOOLEAN DEFAULT TRUE,expires_at TIMESTAMPTZ,updated_at TIMESTAMPTZ DEFAULT NOW());
INSERT INTO proxies(name,protocol,host,port) VALUES('manual','http','192.0.2.1',8080);`)
	require.NoError(t, err)
	for _, file := range []string{"250_yoshub_proxy_binding_limits.sql", "251_ovh_ipv6_egress.sql"} {
		source, err := os.ReadFile(filepath.Join("..", "..", "migrations", file))
		require.NoError(t, err)
		_, err = db.Exec(string(source))
		require.NoError(t, err)
	}
	host := &ovhTestHost{active: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host.mu.Lock()
		defer host.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-control" {
			w.WriteHeader(401)
			return
		}
		if host.fail {
			w.WriteHeader(503)
			return
		}
		var out any
		switch {
		case r.URL.Path == "/v1/status":
			out = ovhipv6.Status{Available: true, Prefix: "2604:2dc0:205:b900::/56", Issued: strconv.Itoa(host.issued)}
		case r.Method == "POST" && r.URL.Path == "/v1/leases":
			host.issued++
			id := strconv.Itoa(host.issued)
			addr, _ := ovhipv6.Address("2604:2dc0:205:b900::/56", id)
			host.active[id] = true
			out = ovhipv6.Lease{ID: id, Address: addr.String(), Username: "ovh6_" + id, Password: "test-only-password", State: "pending", Host: "127.0.0.1", Port: 11880}
		case strings.HasSuffix(r.URL.Path, "/activate"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/leases/"), "/activate")
			if !host.active[id] {
				w.WriteHeader(404)
				return
			}
			out = ovhipv6.Lease{ID: id, State: "active"}
		case r.Method == "DELETE":
			id := strings.TrimPrefix(r.URL.Path, "/v1/leases/")
			state := "retired"
			if host.draining {
				state = "retiring"
			} else {
				delete(host.active, id)
			}
			out = ovhipv6.Lease{ID: id, State: state}
		default:
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(server.Close)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	repo := newProxyRepositoryWithSQL(client, db)
	repo.ipv6Supported = true
	repo.ipv6 = &ovhipv6.Client{BaseURL: server.URL, Token: "test-control", HTTP: server.Client()}
	return db, repo, host
}

func allocateOvhAccount(t *testing.T, repo *proxyRepository, accountID int64) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := repo.client.Tx(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	txctx := dbent.NewTxContext(ctx, tx)
	id, err := repo.SelectAccountProxy(txctx, "random")
	require.NoError(t, err)
	_, err = tx.Client().ExecContext(txctx, `INSERT INTO accounts(id,proxy_id) VALUES($1,$2)`, accountID, id)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return id
}

func TestOVHIPv6LifecycleExclusiveSwitchDeleteAndNoHistoricalRows(t *testing.T) {
	db, r, host := ovhRepoFixture(t)
	ctx := context.Background()
	status, err := r.GetOVHIPv6(ctx)
	require.NoError(t, err)
	require.False(t, status.Enabled)
	_, err = r.SetOVHIPv6(ctx, true)
	require.NoError(t, err)
	available, err := r.HasFreeAccountProxy(ctx)
	require.NoError(t, err)
	require.True(t, available)
	p1 := allocateOvhAccount(t, r, 10)
	_, err = db.Exec(`INSERT INTO accounts(id,proxy_id) VALUES(11,$1)`, p1)
	require.Error(t, err, "second account must not share managed IP")
	_, err = db.Exec(`INSERT INTO accounts(id,proxy_id,parent_account_id) VALUES(12,$1,10)`, p1)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE accounts SET status='inactive',schedulable=false,expires_at=NOW()-INTERVAL '1 hour' WHERE id=10`)
	require.NoError(t, err)
	var state string
	require.NoError(t, db.QueryRow(`SELECT state FROM yoshub_ovh_ipv6_leases WHERE proxy_id=$1`, p1).Scan(&state))
	require.Equal(t, "bound", state)
	require.Error(t, r.SetProxyBindingLimits(ctx, []int64{p1}, 2))
	_, err = db.Exec(`UPDATE proxies SET fallback_mode='direct' WHERE id=$1`, p1)
	require.Error(t, err)

	tx, err := r.client.Tx(ctx)
	require.NoError(t, err)
	txctx := dbent.NewTxContext(ctx, tx)
	p2, err := r.SelectAccountProxy(txctx, "random")
	require.NoError(t, err)
	_, err = tx.Client().ExecContext(txctx, `UPDATE accounts SET proxy_id=$1 WHERE id=10`, p2)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var shadow int64
	require.NoError(t, db.QueryRow(`SELECT proxy_id FROM accounts WHERE id=12`).Scan(&shadow))
	require.Equal(t, p2, shadow)
	var visible bool
	require.NoError(t, db.QueryRow(`SELECT deleted_at IS NULL FROM proxies WHERE id=$1`, p1).Scan(&visible))
	require.False(t, visible)
	host.mu.Lock()
	host.draining = true
	host.mu.Unlock()
	require.NoError(t, r.SyncOVHIPv6(ctx))
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM yoshub_ovh_ipv6_leases WHERE proxy_id=$1`, p1).Scan(&count))
	require.Equal(t, 1, count)
	host.mu.Lock()
	host.draining = false
	host.mu.Unlock()
	_, err = db.Exec(`UPDATE yoshub_ovh_ipv6_leases SET next_attempt_at=NOW()`)
	require.NoError(t, err)
	require.NoError(t, r.SyncOVHIPv6(ctx))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM proxies WHERE id=$1`, p1).Scan(&count))
	require.Zero(t, count)
	_, err = db.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE id IN(10,12)`)
	require.NoError(t, err)
	require.NoError(t, r.SyncOVHIPv6(ctx))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM yoshub_ovh_ipv6_leases`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM proxies`).Scan(&count))
	require.Equal(t, 1, count, "manual proxy preserved")
	p3 := allocateOvhAccount(t, r, 20)
	require.Greater(t, p3, p2)
	var sequence string
	require.NoError(t, db.QueryRow(`SELECT lease_id FROM yoshub_ovh_ipv6_leases WHERE proxy_id=$1`, p3).Scan(&sequence))
	require.Equal(t, "3", sequence)
	_, err = r.SetOVHIPv6(ctx, false)
	require.NoError(t, err)
	tx, err = r.client.Tx(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	id, err := r.SelectAccountProxy(dbent.NewTxContext(ctx, tx), "random")
	require.NoError(t, err)
	require.EqualValues(t, 1, id)
}

func TestOVHIPv6RollbackConcurrencyAndAgentOutage(t *testing.T) {
	db, r, host := ovhRepoFixture(t)
	ctx := context.Background()
	_, err := r.SetOVHIPv6(ctx, true)
	require.NoError(t, err)
	tx, err := r.client.Tx(ctx)
	require.NoError(t, err)
	_, err = r.SelectAccountProxy(dbent.NewTxContext(ctx, tx), "random")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM proxies`).Scan(&count))
	require.Equal(t, 1, count, "unbound candidates never enter pool")
	var wg sync.WaitGroup
	errors := make(chan error, 12)
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tx, e := r.client.Tx(ctx)
			if e != nil {
				errors <- e
				return
			}
			defer tx.Rollback()
			c := dbent.NewTxContext(ctx, tx)
			id, e := r.SelectAccountProxy(c, "random")
			if e == nil {
				_, e = tx.Client().ExecContext(c, `INSERT INTO accounts(id,proxy_id) VALUES($1,$2)`, 100+n, id)
			}
			if e == nil {
				e = tx.Commit()
			}
			errors <- e
		}(n)
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.NoError(t, db.QueryRow(`SELECT COUNT(DISTINCT address) FROM yoshub_ovh_ipv6_leases`).Scan(&count))
	require.Equal(t, 12, count)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM yoshub_ovh_ipv6_leases WHERE lease_id='1'`).Scan(&count))
	require.Zero(t, count, "rolled back reservation never reused")
	host.mu.Lock()
	host.fail = true
	host.mu.Unlock()
	free, err := r.HasFreeAccountProxy(ctx)
	require.Error(t, err)
	require.False(t, free)
	tx, err = r.client.Tx(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	_, err = r.SelectAccountProxy(dbent.NewTxContext(ctx, tx), "random")
	require.Error(t, err, "cannot fall back to manual pool when enabled")
	_, err = db.Exec(`UPDATE accounts SET deleted_at=NOW() WHERE id=100`)
	require.NoError(t, err)
	require.NoError(t, r.SyncOVHIPv6(ctx))
	var failure string
	require.NoError(t, db.QueryRow(`SELECT last_error FROM yoshub_ovh_ipv6_leases WHERE owner_account_id=100`).Scan(&failure))
	require.Contains(t, failure, "503")
	// Missing environment must fail closed even if the persisted switch is on.
	r.ipv6 = nil
	free, err = r.HasFreeAccountProxy(ctx)
	require.Error(t, err)
	require.False(t, free)
}

func TestOVHIPv6OneConnectionTransactionAndMissingProxyCleanup(t *testing.T) {
	db, r, _ := ovhRepoFixture(t)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := r.SetOVHIPv6(ctx, true)
	require.NoError(t, err)
	tx, err := r.client.Tx(ctx)
	require.NoError(t, err)
	defer tx.Rollback()
	c := dbent.NewTxContext(ctx, tx)
	id, err := r.SelectAccountProxy(c, "random")
	require.NoError(t, err, "selection must use the transaction connection")
	_, err = tx.Client().ExecContext(c, `INSERT INTO accounts(id,proxy_id) VALUES(40,$1)`, id)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = db.ExecContext(ctx, `UPDATE proxies SET backup_proxy_id=$1 WHERE id=1`, id)
	require.Error(t, err, "managed source must not be shared as another proxy's fallback")
	_, err = db.ExecContext(ctx, `UPDATE accounts SET deleted_at=NOW() WHERE id=40`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM proxies WHERE id=$1`, id)
	require.NoError(t, err)
	require.NoError(t, r.SyncOVHIPv6(ctx))
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM yoshub_ovh_ipv6_leases`).Scan(&count))
	require.Zero(t, count, "cleanup must finish even if the proxy row was already removed")
}
