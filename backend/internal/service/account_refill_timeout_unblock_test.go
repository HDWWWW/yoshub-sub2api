//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillTimeoutResumesReserveWithHealthyAccountAtCustomDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, p, accounts := failureRefillHarness(t)
		created := time.Now()
		seedStalledRefill(t, store, "review", created)
		accounts.accounts = []Account{refillTestAccount(created.Add(3 * time.Hour))}
		store.mutate(t, func(v *accountRefillState) {
			minutes := 17
			v.Config.TimeoutRetryMinutes = &minutes
			v.Config.MinReady, v.Config.MaxReady = 2, 2
			v.NextCheckAt = created.Add(time.Hour)
		})
		time.Sleep(17*time.Minute - time.Nanosecond)
		require.NoError(t, s.run(context.Background(), false))
		require.Empty(t, p.orders)
		time.Sleep(time.Nanosecond)
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, p.orders, 1)
		require.Equal(t, "健康储备不足", p.orders[0].Reason)
		require.Equal(t, "pro5x", p.orders[0].Plan, "normal automation follows the next rotation step")
		v, err := s.View(context.Background())
		require.NoError(t, err)
		require.Equal(t, 1, v.Evaluation.Ready)
		require.Equal(t, 1, v.Evaluation.Pending)
		require.Equal(t, 1, v.Evaluation.TimedOutPending)
		require.Equal(t, "review", v.Jobs[1].Status)
		require.False(t, v.Jobs[1].BudgetReleased)
		require.Equal(t, 120.0, v.Daily.Points)
		for range 3 {
			time.Sleep(5 * time.Second)
			require.NoError(t, s.run(context.Background(), false))
		}
		require.Len(t, p.orders, 1, "the fresh order covers the remaining shortage")
	})
}

func TestRefillTimeoutEachUnfinishedStateLeavesDemandAndCapacity(t *testing.T) {
	for _, status := range []string{"submitting", "queued", "processing", "review", "blocked", "supplier_unknown", "awaiting_delivery", "awaiting_push", "importing", "import_failed"} {
		t.Run(status, func(t *testing.T) {
			c, now := refillTestConfig(), time.Now()
			c.MinReady, c.MaxReady, c.MaxInFlight = 2, 2, 1
			accounts := []Account{refillTestAccount(now.Add(time.Hour))}
			jobs := []AccountRefillJob{{ID: "old", Status: status, Plan: "pro5x", CreatedAt: now.Add(-10 * time.Minute), UpdatedAt: now, Concurrency: 9000}}
			before := evaluateAccountRefill(c, accounts, nil, jobs, now.Add(-time.Nanosecond))
			require.Zero(t, before.Needed)
			require.Equal(t, 1, before.Pending)
			after := evaluateAccountRefill(c, accounts, nil, jobs, now)
			require.Equal(t, 1, after.Needed)
			require.Zero(t, after.Pending)
			require.Zero(t, after.PendingCapacity)
			require.Equal(t, 1, after.TimedOutPending)
			disabled := false
			c.TimeoutRetryEnabled = &disabled
			require.Zero(t, evaluateAccountRefill(c, accounts, nil, jobs, now).Needed)
		})
	}
}

func TestRefillTimeoutFreshOrdersDoNotExtendOlderDeadline(t *testing.T) {
	s, store, p, accounts := failureRefillHarness(t)
	seedStalledRefill(t, store, "review", time.Now().Add(-20*time.Minute))
	accounts.accounts = []Account{refillTestAccount(time.Now().Add(3 * time.Hour))}
	store.mutate(t, func(v *accountRefillState) {
		v.Config.MinReady, v.Config.MaxReady, v.Config.MaxInFlight = 3, 4, 2
		v.NextCheckAt = time.Now().Add(time.Hour)
		v.LastSubmitAt = time.Now().Add(-time.Minute)
		v.Config.CooldownSeconds = 15
		v.Jobs = append(v.Jobs, AccountRefillJob{ID: "fresh", SupplierID: "fresh", Plan: "pro5x", Status: "processing", Concurrency: 60, CreatedAt: v.LastSubmitAt, NextAttemptAt: time.Now().Add(time.Hour)})
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.orders, 1, "only the fresh order still occupies a slot")
	v, err := s.View(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, v.Evaluation.Pending)
	require.Equal(t, 1, v.Evaluation.TimedOutPending)
}

func TestRefillTimeoutBacklogCannotStarveNewDelivery(t *testing.T) {
	c, now := refillTestConfig(), time.Now()
	jobs := make([]AccountRefillJob, 100)
	for i := range jobs {
		jobs[i] = AccountRefillJob{Status: "review", CreatedAt: now.Add(-time.Hour), NextAttemptAt: now.Add(-time.Minute)}
	}
	jobs = append(jobs, AccountRefillJob{Status: "awaiting_delivery", CreatedAt: now.Add(-time.Minute), NextAttemptAt: now})
	require.Equal(t, 100, nextAccountRefillJob(jobs, c, now))
	jobs[100].Status = "imported"
	require.Equal(t, 0, nextAccountRefillJob(jobs, c, now), "old orders continue reconciling after current work")
}

func TestRefillTimeoutResumesCapacityAndIntervalWithReserve(t *testing.T) {
	for _, mode := range []string{"capacity", "interval"} {
		t.Run(mode, func(t *testing.T) {
			s, store, p, accounts := failureRefillHarness(t)
			seedStalledRefill(t, store, "review", time.Now().Add(-20*time.Minute))
			accounts.accounts = []Account{refillTestAccount(time.Now().Add(3 * time.Hour))}
			store.mutate(t, func(v *accountRefillState) {
				v.Config.Mode, v.Config.MaxReady = mode, 3
				v.NextIntervalAt = time.Now().Add(-time.Minute)
				v.Config.IntervalQuantity = 1
			})
			if mode == "capacity" {
				s.loads = refillTestLoads{map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 60}}}
				require.NoError(t, s.run(context.Background(), false))
				require.Empty(t, p.orders, "timeout must not bypass the capacity observation window")
				v, err := decodeAccountRefillState(store.raw)
				require.NoError(t, err)
				feedRefillCapacity(s, v.Config, v.Revision, time.Now(), v.Config.ConcurrencySustainSeconds)
				store.mutate(t, func(v *accountRefillState) { v.NextCheckAt = time.Time{} })
			}
			require.NoError(t, s.run(context.Background(), false))
			require.Len(t, p.orders, 1)
			require.Empty(t, p.orders[0].TimeoutRetryOf, "purchase follows normal automation rather than the empty-pool fallback")
		})
	}
}

func TestRefillTimeoutLateFailureDoesNotRestoreIntervalAllowance(t *testing.T) {
	s, store, p, accounts := failureRefillHarness(t)
	seedStalledRefill(t, store, "review", time.Now().Add(-20*time.Minute))
	accounts.accounts = []Account{refillTestAccount(time.Now().Add(3 * time.Hour))}
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Mode, v.Jobs[0].Mode, v.Config.MaxReady = "interval", "interval", 3
		v.NextIntervalAt = time.Now().Add(-time.Minute)
		v.Config.IntervalQuantity = 1
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.orders, 1)
	p.states["stalled"] = "failed"
	store.mutate(t, func(v *accountRefillState) { v.Jobs[1].NextAttemptAt = time.Time{} })
	require.NoError(t, s.run(context.Background(), false))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Zero(t, v.IntervalRemaining)
	require.False(t, v.Jobs[1].ReplacementPending)
	require.True(t, v.Jobs[1].BudgetReleased)
	require.Len(t, p.orders, 1)
}

func TestRefillTimeoutExpiredOrderReleasesLeaseClaimAndCanStillDeliver(t *testing.T) {
	s, store, p, accounts := expiryRefillHarness(t)
	seedStalledRefill(t, store, "review", time.Now().Add(-20*time.Minute))
	target := AccountRefillExpiryTarget{AccountID: 1, SourceExpiresAt: accounts.accounts[0].Extra[AccountSourceExpiryExtraKey].(int64)}
	store.mutate(t, func(v *accountRefillState) {
		v.Config.MinReady = 2
		v.ExpiryDemands = []accountRefillExpiryDemand{{AccountRefillExpiryTarget: target, JobID: "stalled", OwnerGroupID: 2, DeliveryGroupIDs: []int64{2}}}
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.orders, 1)
	require.Equal(t, target, *p.orders[0].ExpiryTarget, "an expired claim cannot hide the lease needing replacement")
	p.states["stalled"] = "succeeded"
	for range 2 {
		store.mutate(t, func(v *accountRefillState) { v.Jobs[1].NextAttemptAt = time.Time{} })
		require.NoError(t, s.run(context.Background(), false))
	}
	v, err := s.View(context.Background())
	require.NoError(t, err)
	require.Equal(t, "imported", v.Jobs[1].Status)
	require.False(t, v.Jobs[1].BudgetReleased)
	require.Equal(t, 1, accounts.creates)
}

func TestRefillTimeoutOverlappingGroupDropsOldReviewButCreditsNewOrder(t *testing.T) {
	s, base, p, accounts := failureRefillHarness(t)
	seedStalledRefill(t, base, "review", time.Now().Add(-20*time.Minute))
	accounts.accounts = []Account{refillTestAccount(time.Now().Add(3 * time.Hour))}
	accounts.accounts[0].GroupIDs = []int64{2, 3}
	base.mutate(t, func(v *accountRefillState) {
		v.Config.MinReady, v.Config.MaxReady = 2, 3
		v.Config.DeliveryGroupIDs, v.Jobs[0].DeliveryGroupIDs = []int64{2, 3}, []int64{2, 3}
	})
	grouped := &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	s.store = grouped
	ctx := context.Background()
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	v, err := child.View(ctx)
	require.NoError(t, err)
	v.Config.Enabled, v.Config.MinReady, v.Config.MaxReady = true, 2, 3
	v.Config.DeliveryGroupIDs = []int64{2, 3}
	require.NoError(t, child.Configure(ctx, v.Config, "", v.Revision))
	require.NoError(t, child.run(ctx, false))
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1)
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, v.Evaluation.Pending)
	require.Equal(t, 1, v.Evaluation.TimedOutPending)
}

func TestRefillTimeoutNotificationRetryKeepsOldAndNewTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, _, accounts := failureRefillHarness(t)
		seedStalledRefill(t, store, "review", time.Now().Add(-20*time.Minute))
		accounts.accounts = []Account{refillTestAccount(time.Now().Add(3 * time.Hour))}
		n := &refillNoticeStub{err: errors.New("offline")}
		s.notifier = n
		store.mutate(t, func(v *accountRefillState) { v.EncryptedWebhook = "encrypted-fixture" })
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, n.messages, 1)
		store.mutate(t, func(v *accountRefillState) {
			v.Jobs = append(v.Jobs, AccountRefillJob{ID: "second", SupplierID: "second", Status: "review", Plan: "pro5x", CreatedAt: time.Now().Add(-9 * time.Minute), NextAttemptAt: time.Now().Add(time.Hour)})
		})
		time.Sleep(time.Minute)
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, n.messages, 1)
		time.Sleep(4 * time.Minute)
		n.err = nil
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, n.messages, 3)
		require.Equal(t, n.messages[0], n.messages[1])
		require.Contains(t, n.messages[2], "second")
		s = &AccountRefillService{store: store, provider: s.provider, encryptor: s.encryptor, accounts: s.accounts, groups: s.groups, loads: s.loads, notifier: n}
		time.Sleep(5 * time.Minute)
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, n.messages, 3, "timeout receipts survive a restart")
	})
}
