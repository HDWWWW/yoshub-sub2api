//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func expiryCapacityLoads(s *AccountRefillService, accounts *refillTestAccounts, inUse int) map[int64]*AccountLoadInfo {
	for i := range accounts.accounts {
		accounts.accounts[i].Concurrency = 65
	}
	loads := map[int64]*AccountLoadInfo{1: {CurrentConcurrency: inUse}, 2: {}}
	s.loads = refillTestLoads{loads}
	return loads
}

func observeExpiryCapacity(t *testing.T, s *AccountRefillService, store *refillTestStore, seconds int) {
	t.Helper()
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.NoError(t, s.evaluate(context.Background(), &state, time.Now()))
	feedRefillForecast(s, state.Config, state.Revision, state.Evaluation, time.Now(), seconds)
}

func TestRefillExpirySufficientCapacityNeverPurchases(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		inUse    int
		forecast bool
	}{
		{"zero_load", "capacity", 0, true},
		{"production_twenty_of_sixty_five", "capacity", 20, true},
		{"just_below_threshold", "capacity", 55, true},
		{"forecast_disabled", "capacity", 56, false},
		{"fixed_reserve", "fixed", 56, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, p, accounts := expiryRefillHarness(t)
			expiryCapacityLoads(s, accounts, tc.inUse)
			store.mutate(t, func(state *accountRefillState) {
				state.Config.Mode = tc.mode
				state.Config.ForecastEnabled = &tc.forecast
				// An unresolved receipt saved by the previous version is context,
				// not permission to keep buying after upgrading or restarting.
				state.ExpiryDemands = []accountRefillExpiryDemand{{AccountRefillExpiryTarget: AccountRefillExpiryTarget{
					AccountID: 1, SourceExpiresAt: accounts.accounts[0].Extra[AccountSourceExpiryExtraKey].(int64),
				}}}
				state.Evaluation.Needed = 1
				state.Evaluation.Reason = "账号临期，补充接替账号"
			})
			for range 2 {
				refillDue(t, store)
				require.NoError(t, s.run(context.Background(), false))
				require.Empty(t, p.orders, "a supplier deadline alone cannot authorize a paid order")
				state, err := decodeAccountRefillState(store.raw)
				require.NoError(t, err)
				require.Zero(t, state.Evaluation.Needed)
				require.Equal(t, "储备充足", state.Evaluation.Reason)
				require.Empty(t, state.Usage)
				require.Zero(t, state.RotationCursor)
				require.Len(t, state.ExpiryDemands, 1)
				s = &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
			}
		})
	}
}

func TestRefillExpiryCapacityRequiresFullObservation(t *testing.T) {
	s, store, p, accounts := expiryRefillHarness(t)
	expiryCapacityLoads(s, accounts, 56)
	ctx := context.Background()
	for _, seconds := range []int{0, 9, 10} {
		refillDue(t, store)
		observeExpiryCapacity(t, s, store, seconds)
		require.NoError(t, s.run(ctx, false))
		if seconds < 10 {
			require.Empty(t, p.orders, "expiry cannot bypass the forecast observation window")
		}
	}
	require.Len(t, p.orders, 1)
	job := p.orders[0]
	require.Equal(t, refillForecastReason, job.Reason)
	require.Contains(t, job.ReasonDetail, "运行中并发 56")
	require.Contains(t, job.ReasonDetail, "预计利用率 86.15%")
	require.Contains(t, job.ReasonDetail, "持续观察已满 10 秒")
	require.Contains(t, job.ReasonDetail, "接替账号 #1")
	require.NotNil(t, job.ExpiryTarget)
	require.Equal(t, int64(1), job.ExpiryTarget.AccountID)
	store.mutate(t, func(state *accountRefillState) { state.Config.MaxInFlight = 3 })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1, "pending capacity fills the shortage even with room for more orders")
}

func TestRefillExpiryFailureWaitsForRenewedCapacityDemand(t *testing.T) {
	s, store, p, accounts := expiryRefillHarness(t)
	loads := expiryCapacityLoads(s, accounts, 56)
	ctx := context.Background()
	observeExpiryCapacity(t, s, store, 10)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1)
	first := p.orders[0]
	loads[1].CurrentConcurrency = 20
	p.states[first.ID] = "failed"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1, "a confirmed failure does not authorize buying without a current shortage")
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.Jobs[0].ReplacementPending)
	require.Equal(t, 1, state.RotationCursor)
	// After a restart, returned load must earn a new observation window.
	s = &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
	loads[1].CurrentConcurrency = 56
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1)
	observeExpiryCapacity(t, s, store, 10)
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2)
	replacement := p.orders[1]
	require.Equal(t, "pro5x", replacement.Plan)
	require.Equal(t, first.ID, replacement.ReplacesJobID)
	require.Equal(t, first.ExpiryTarget, replacement.ExpiryTarget)
	require.Equal(t, refillForecastReason, replacement.Reason)
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, 1, state.RotationCursor)
}

func TestRefillExpiryRechecksCapacityBeforeSpending(t *testing.T) {
	s, store, p, accounts := expiryRefillHarness(t)
	loads := expiryCapacityLoads(s, accounts, 56)
	observeExpiryCapacity(t, s, store, 10)
	p.afterCatalog = func() { loads[1].CurrentConcurrency = 20 }
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, p.orders)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, state.Jobs)
	require.Empty(t, state.Usage)
	require.Zero(t, state.RotationCursor)
}
