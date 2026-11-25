//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRisk429ConcurrencyCapReachesScheduler(t *testing.T) {
	ctx := context.Background()
	client, rdb := testEntClient(t), testRedis(t)
	g := mustCreateGroup(t, client, &service.Group{Name: t.Name(), Platform: service.PlatformOpenAI})
	a := mustCreateAccount(t, client, &service.Account{Name: t.Name(), Platform: service.PlatformOpenAI, Concurrency: 5})
	mustBindAccountToGroup(t, client, a.ID, g.ID, 1)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id=$1", a.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id=$1", g.ID)
	})
	snapshot, slots := NewSchedulerCache(rdb), NewConcurrencyCache(rdb, 5, 300)
	accounts := NewAccountRepository(client, integrationDB, snapshot)
	store := NewAccountRiskStore(integrationDB, accounts)
	current, err := accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.NoError(t, snapshot.SetAccount(ctx, current))
	acquire := func(limit int, request string) bool {
		t.Helper()
		ok, err := slots.AcquireAccountSlot(ctx, a.ID, limit, request)
		require.NoError(t, err)
		return ok
	}
	require.True(t, acquire(current.Concurrency, "in-flight-1"))
	require.True(t, acquire(current.Concurrency, "in-flight-2"))
	apply := func(finishErr error) error {
		return store.Update(ctx, func(_ *service.AccountRiskState, scheduling service.AccountRiskScheduling) error {
			changed, err := scheduling.ReduceConcurrency(g.ID, current, 1)
			require.NoError(t, err)
			require.True(t, changed)
			return finishErr
		})
	}
	// A failure saving risk history must roll back the limit and not publish it.
	abort := errors.New("history write failed")
	require.ErrorIs(t, apply(abort), abort)
	stored, err := accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, 5, stored.Concurrency)
	cached, err := snapshot.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, 5, cached.Concurrency)

	require.NoError(t, apply(nil))
	stored, err = accounts.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, stored.Schedulable)
	require.Equal(t, 1, stored.Concurrency)
	cached, err = snapshot.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, cached.Schedulable)
	require.Equal(t, stored.Concurrency, cached.Concurrency)
	used, err := slots.GetAccountConcurrency(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, 2, used)
	require.False(t, acquire(cached.Concurrency, "new"))
	require.NoError(t, slots.ReleaseAccountSlot(ctx, a.ID, "in-flight-1"))
	require.False(t, acquire(cached.Concurrency, "new"))
	require.NoError(t, slots.ReleaseAccountSlot(ctx, a.ID, "in-flight-2"))
	require.True(t, acquire(cached.Concurrency, "new"))
	require.False(t, acquire(cached.Concurrency, "extra"))
}
