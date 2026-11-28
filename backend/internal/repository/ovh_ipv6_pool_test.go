//go:build unit

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
	"github.com/stretchr/testify/require"
)

func bindTestPool(ctx context.Context, r *proxyRepository, account int64, mode string) error {
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	txctx := dbent.NewTxContext(ctx, tx)
	id, err := r.SelectAccountProxy(txctx, mode)
	if err != nil {
		return err
	}
	_, err = tx.Client().ExecContext(txctx, `INSERT INTO accounts(id,proxy_id) VALUES($1,$2)
		ON CONFLICT(id) DO UPDATE SET proxy_id=EXCLUDED.proxy_id`, account, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func TestOvhPoolAlternatesUniqueAddressesAndRollsBackFailedSwitch(t *testing.T) {
	db, r, main := ovhRepoFixture(t)
	var standby *ovhTestHost
	r.ipv6Backup, standby = newTestPoolHost(t, "2001:db8:2::/64", 11980)
	ctx := ovhipv6.AccountEdit(context.Background())
	backupCtx := ovhipv6.WithPool(ctx, ovhipv6.Backup)
	s, err := r.GetOVHIPv6(backupCtx)
	require.NoError(t, err)
	require.False(t, s.Enabled)
	require.Error(t, bindTestPool(ctx, r, 1, "ovh_ipv6_next"))
	_, err = r.SetOVHIPv6(backupCtx, true)
	require.NoError(t, err)
	require.NoError(t, bindTestPool(ctx, r, 1, "ovh_ipv6_next"))
	standby.mu.Lock()
	standby.fail = true
	standby.mu.Unlock()
	require.Error(t, bindTestPool(ctx, r, 1, "ovh_ipv6_next"))
	var kept string
	require.NoError(t, db.QueryRow(`SELECT lease_id FROM yoshub_ovh_ipv6_leases WHERE owner_account_id=1 AND state='bound'`).Scan(&kept))
	require.Equal(t, "1", kept)
	standby.mu.Lock()
	standby.fail = false
	standby.mu.Unlock()
	require.NoError(t, bindTestPool(ctx, r, 2, "ovh_ipv6_next"))
	require.NoError(t, bindTestPool(ctx, r, 3, "ovh_ipv6_next"))
	var count, addresses int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT address) FROM yoshub_ovh_ipv6_leases WHERE state='bound'`).Scan(&count, &addresses))
	require.Equal(t, 3, count)
	require.Equal(t, 3, addresses)
	var b string
	require.NoError(t, db.QueryRow(`SELECT lease_id FROM yoshub_ovh_ipv6_leases WHERE owner_account_id=2`).Scan(&b))
	require.Equal(t, "backup:1", b)
	// Deliberately overlap local sequence 1 across the two agents.
	require.NoError(t, bindTestPool(ctx, r, 2, "ovh_ipv6"))
	require.NoError(t, r.SyncOVHIPv6(ctx))
	main.mu.Lock()
	primaryStillActive := main.active["1"]
	main.mu.Unlock()
	require.True(t, primaryStillActive)
	h, err := r.ListIPv6Retirements(ctx, 0)
	require.NoError(t, err)
	require.Len(t, h, 1)
	require.Equal(t, "2001:db8:2::1", h[0].Address)
	require.Equal(t, ovhipv6.Backup, h[0].Pool)
	oldPage, err := r.ListIPv6Retirements(ctx, h[0].ID)
	require.NoError(t, err)
	require.Empty(t, oldPage)
	require.NoError(t, r.SyncOVHIPv6(ctx))
	h, err = r.ListIPv6Retirements(ctx, 0)
	require.NoError(t, err)
	require.Len(t, h, 1)
	s, err = r.SetOVHIPv6(backupCtx, false)
	require.NoError(t, err)
	require.False(t, s.Enabled)
	require.NoError(t, bindTestPool(ctx, r, 4, "ovh_ipv6_backup"))
	require.Error(t, bindTestPool(context.Background(), r, 5, "ovh_ipv6_next"))
}
func TestOvhPoolConcurrentEditsHaveSeparateAddresses(t *testing.T) {
	db, r, _ := ovhRepoFixture(t)
	r.ipv6Backup, _ = newTestPoolHost(t, "2001:db8:2::/64", 11980)
	ctx := ovhipv6.AccountEdit(context.Background())
	_, err := r.SetOVHIPv6(ovhipv6.WithPool(ctx, ovhipv6.Backup), true)
	require.NoError(t, err)
	var tasks sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		tasks.Add(1)
		go func(i int) {
			defer tasks.Done()
			if err := bindTestPool(ctx, r, int64(i+10), "ovh_ipv6_next"); err != nil {
				errs <- fmt.Errorf("edit %d: %w", i, err)
			}
		}(i)
	}
	tasks.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var a, b, ips int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FILTER(WHERE lease_id LIKE 'backup:%'),COUNT(*) FILTER(WHERE lease_id NOT LIKE 'backup:%'),COUNT(DISTINCT address) FROM yoshub_ovh_ipv6_leases`).Scan(&b, &a, &ips))
	require.Equal(t, 4, a)
	require.Equal(t, 4, b)
	require.Equal(t, 8, ips)
}

func TestOvhPoolRefillRulesRotateAtCommitAndManualEditsHaveTheirOwnTurn(t *testing.T) {
	db, r, _ := ovhRepoFixture(t)
	r.ipv6Backup, _ = newTestPoolHost(t, "2001:db8:2::/64", 11980)
	ctx := context.Background()
	first := ovhipv6.ForRefillRule(ctx, 7)
	second := ovhipv6.ForRefillRule(ctx, 8)
	require.ErrorContains(t, bindTestPool(first, r, 1, "ovh_ipv6_backup"), "备用池自动分配")
	_, err := r.SetOVHIPv6(ovhipv6.WithPool(ctx, ovhipv6.Backup), true)
	require.NoError(t, err)
	bind := func(ctx context.Context, account int64, mode string, pool ovhipv6.Pool) {
		t.Helper()
		require.NoError(t, bindTestPool(ctx, r, account, mode))
		var stored string
		require.NoError(t, db.QueryRow("SELECT lease_id FROM yoshub_ovh_ipv6_leases WHERE owner_account_id=$1 AND state='bound'", account).Scan(&stored))
		got, _ := ovhipv6.SplitID(stored)
		require.Equal(t, pool, got)
	}
	bind(first, 1, "ovh_ipv6_next", ovhipv6.Primary)
	bind(second, 2, "ovh_ipv6_next", ovhipv6.Primary)
	bind(ovhipv6.AccountEdit(ctx), 3, "ovh_ipv6_next", ovhipv6.Primary)
	// Fail after address allocation, before the account can persist its binding.
	tx, err := r.client.Tx(first)
	require.NoError(t, err)
	_, err = r.SelectAccountProxy(dbent.NewTxContext(first, tx), "ovh_ipv6_next")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	bind(first, 4, "ovh_ipv6_next", ovhipv6.Backup)
	bind(first, 5, "ovh_ipv6_backup", ovhipv6.Backup)
	bind(first, 6, "ovh_ipv6_next", ovhipv6.Primary)
	var unique int
	require.NoError(t, db.QueryRow("SELECT COUNT(DISTINCT address) FROM yoshub_ovh_ipv6_leases WHERE state='bound'").Scan(&unique))
	require.Equal(t, 6, unique)
}
