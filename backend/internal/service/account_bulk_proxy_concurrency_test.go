//go:build unit

package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

type boundedIPv6Allocator struct {
	ProxyRepository
	mu                  sync.Mutex
	calls, active, peak int
	started             chan struct{}
	release             chan struct{}
}

func (r *boundedIPv6Allocator) SelectAccountProxy(ctx context.Context, mode string) (int64, error) {
	if dbent.TxFromContext(ctx) == nil || mode != "ovh_ipv6" {
		return 0, fmt.Errorf("allocation escaped the account transaction")
	}
	r.mu.Lock()
	r.calls++
	id := int64(700 + r.calls)
	r.active++
	r.peak = max(r.peak, r.active)
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.active--; r.mu.Unlock() }()
	r.started <- struct{}{}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-r.release:
		return id, nil
	}
}

func TestNativeIPv6BulkBoundsConcurrencyPreservesOrderAndStopsOnCancel(t *testing.T) {
	for _, cancelBatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelBatch), func(t *testing.T) {
			login, db := scopedAdminAccountTestDB(t)
			_, err := db.Exec(`CREATE TABLE account_groups(account_id BIGINT,group_id BIGINT); INSERT INTO accounts(id,name,platform,type,proxy_id) SELECT id,'original','openai','apikey',11 FROM generate_series(1001,1020) AS id`)
			require.NoError(t, err)
			r := &nativeIPv6AccountRepo{scopedSelectionAccountRepo: &scopedSelectionAccountRepo{&scopedAdminWriteTestRepo{login: login}}, failID: 1005}
			allocator := &boundedIPv6Allocator{started: make(chan struct{}, 20), release: make(chan struct{})}
			s := &adminServiceImpl{accountRepo: r, proxyRepo: allocator, entClient: dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
			ids := make([]int64, 20)
			for i := range ids {
				ids[i] = int64(1001 + i)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			type outcome struct {
				result *BulkUpdateAccountsResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := s.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: append(append([]int64{}, ids...), ids[0], ids[1]), ProxyMode: "ovh_ipv6", Name: "updated"})
				done <- outcome{result, err}
			}()
			for range automaticProxyBulkConcurrency {
				select {
				case <-allocator.started:
				case <-ctx.Done():
					t.Fatal("allocation workers did not start")
				}
			}
			if cancelBatch {
				cancel()
			} else {
				close(allocator.release)
			}
			var got outcome
			select {
			case got = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("bulk update did not stop")
			}
			require.NoError(t, got.err)
			require.Len(t, got.result.Results, 20, "duplicate IDs must not allocate again")
			allocator.mu.Lock()
			calls, peak, active := allocator.calls, allocator.peak, allocator.active
			allocator.mu.Unlock()
			require.Equal(t, automaticProxyBulkConcurrency, peak)
			require.Zero(t, active)
			if cancelBatch {
				require.Equal(t, automaticProxyBulkConcurrency, calls, "cancellation must not start the remaining allocations")
				require.Equal(t, 20, got.result.Failed)
				require.Zero(t, got.result.Success)
			} else {
				require.Equal(t, 20, calls)
				require.Equal(t, 1, got.result.Failed)
				require.Equal(t, 19, got.result.Success)
			}
			for i, entry := range got.result.Results {
				require.Equal(t, ids[i], entry.AccountID, "response order must match input order")
				stored, err := r.GetByID(context.Background(), entry.AccountID)
				require.NoError(t, err)
				if cancelBatch || entry.AccountID == r.failID {
					require.False(t, entry.Success)
					require.NotEmpty(t, entry.Error)
					require.Zero(t, entry.ProxyID)
					require.Empty(t, entry.ManagedIPv6)
					require.EqualValues(t, 11, *stored.ProxyID)
					require.Equal(t, "original", stored.Name)
				} else {
					require.True(t, entry.Success)
					require.Equal(t, *stored.ProxyID, entry.ProxyID)
					require.Equal(t, "updated", stored.Name)
				}
			}
		})
	}
}
