//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillTimeoutNoticeAtCustomDeadlineAndRetrySurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, p, _ := failureRefillHarness(t)
		created := time.Now()
		seedStalledRefill(t, store, "review", created)
		n := &refillNoticeStub{err: errors.New("robot unavailable")}
		s.notifier = n
		store.mutate(t, func(v *accountRefillState) {
			minutes := 17
			v.Config.TimeoutRetryMinutes = &minutes
			v.EncryptedWebhook = "encrypted-fixture"
			v.NextCheckAt = created.Add(time.Hour)
			v.Config.Notifications.RefillStarted = false
		})
		time.Sleep(17*time.Minute - time.Nanosecond)
		require.NoError(t, s.run(context.Background(), false))
		require.Empty(t, n.messages)
		time.Sleep(time.Nanosecond)
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, p.orders, 1, "failed robot does not block rescue")
		require.Len(t, n.messages, 1)
		require.Contains(t, n.messages[0], "超时阈值：17 分钟")
		require.Contains(t, n.messages[0], "已创建超时补单")
		require.Contains(t, n.messages[0], "stalled")
		require.Contains(t, n.messages[0], "review")
		s = &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: s.accounts, groups: s.groups, loads: s.loads, notifier: n}
		time.Sleep(5*time.Minute - time.Nanosecond)
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, n.messages, 1)
		time.Sleep(time.Nanosecond)
		n.err = nil
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, n.messages, 2)
		require.Equal(t, n.messages[0], n.messages[1])
		require.Len(t, p.orders, 1)
		time.Sleep(5 * time.Minute)
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, n.messages, 2, "successful notice is durable")
	})
}

func TestRefillTimeoutNotifiesBlockedResultOnceAndCanBeDisabled(t *testing.T) {
	for _, scenario := range []string{"budget", "balance", "backoff", "disabled", "no webhook", "healthy", "preview"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, accounts := failureRefillHarness(t)
			seedStalledRefill(t, store, "review", time.Now().Add(-20*time.Minute))
			n := &refillNoticeStub{}
			s.notifier = n
			store.mutate(t, func(v *accountRefillState) {
				v.EncryptedWebhook = "encrypted-fixture"
				switch scenario {
				case "budget":
					v.Config.DailyMaxPoints = 60
				case "balance":
					p.available = 1
				case "backoff":
					v.ProviderRetryAt = time.Now().Add(time.Hour)
				case "disabled":
					enabled := false
					v.Config.TimeoutRetryNotify = &enabled
				case "no webhook":
					v.EncryptedWebhook = ""
				}
			})
			if scenario == "healthy" {
				accounts.accounts = []Account{refillTestAccount(time.Now().Add(3 * time.Hour))}
			}
			for i := 0; i < 3; i++ {
				require.NoError(t, s.run(context.Background(), scenario == "preview"))
			}
			if scenario == "budget" || scenario == "balance" || scenario == "backoff" {
				require.Len(t, n.messages, 1)
				require.Contains(t, n.messages[0], "尚未创建新订单")
				if scenario == "budget" {
					require.Contains(t, n.messages[0], "预算")
				}
				if scenario == "balance" {
					require.Contains(t, n.messages[0], "积分不足")
				}
				if scenario == "backoff" {
					require.Contains(t, n.messages[0], "退避")
				}
				require.Empty(t, p.orders)
			} else {
				require.Empty(t, n.messages)
			}
		})
	}
}

func seedStalledRefill(t *testing.T, store *refillTestStore, status string, created time.Time) {
	t.Helper()
	store.mutate(t, func(v *accountRefillState) {
		v.Jobs = []AccountRefillJob{{ID: "stalled", SupplierID: "stalled", Status: status, SupplierStatus: "review",
			Plan: "pro20x", Price: 60, GroupID: v.Config.GroupID, DeliveryGroupIDs: v.Config.DeliveryGroupIDs,
			Mode: v.Config.Mode, CreatedAt: created, UpdatedAt: time.Now(), NextAttemptAt: time.Now().Add(24 * time.Hour)}}
		v.LastSubmitAt = created
		v.RotationCursor = 1
		v.Usage[created.In(refillTimezone).Format("2006-01-02")] = AccountRefillDailyUsage{Count: 1, Points: 60}
	})
}

func TestRefillTimeoutConfigDefaultsValidationAndLegacySave(t *testing.T) {
	s, store, _, _ := failureRefillHarness(t)
	ctx := context.Background()
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, refillTimeoutRetryEnabled(v.Config))
	require.Equal(t, 10*time.Minute, refillTimeoutRetryDuration(v.Config))
	for _, minutes := range []int{0, -1, 1441} {
		c := v.Config
		c.TimeoutRetryMinutes = &minutes
		require.Error(t, c.Validate())
	}
	minutes, enabled := 23, false
	c := v.Config
	c.TimeoutRetryMinutes, c.TimeoutRetryEnabled = &minutes, &enabled
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	v, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	c = v.Config
	c.TimeoutRetryMinutes, c.TimeoutRetryEnabled = nil, nil
	c.CooldownSeconds++
	require.NoError(t, s.Configure(ctx, c, "", v.Revision))
	restarted := &AccountRefillService{store: store}
	raw, err := restarted.store.Read(ctx)
	require.NoError(t, err)
	v, err = decodeAccountRefillState(raw)
	require.NoError(t, err)
	require.False(t, refillTimeoutRetryEnabled(v.Config))
	require.Equal(t, 23*time.Minute, refillTimeoutRetryDuration(v.Config))
	legacy, err := decodeAccountRefillState(`{"config":{"group_id":2}}`)
	require.NoError(t, err)
	require.True(t, refillTimeoutRetryEnabled(legacy.Config))
	require.Equal(t, 10*time.Minute, refillTimeoutRetryDuration(legacy.Config))
}

func TestRefillTimeoutRetriesAtConfiguredDeadlineAndSurvivesRestart(t *testing.T) {
	for _, minutes := range []int{1, 10, 25} {
		t.Run(time.Duration(minutes).String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, store, p, _ := failureRefillHarness(t)
				ctx := context.Background()
				created := time.Now()
				seedStalledRefill(t, store, "review", created)
				store.mutate(t, func(v *accountRefillState) {
					v.Config.TimeoutRetryMinutes = &minutes
					v.Config.CheckSeconds, v.Config.CooldownSeconds = 3600, 86400
					v.NextCheckAt = created.Add(time.Hour)
					v.Config.WindowEnabled = true
					// A window definitely excluding the frozen test clock.
					v.Config.WindowStart = created.In(refillTimezone).Add(2 * time.Hour).Format("15:04")
					v.Config.WindowEnd = created.In(refillTimezone).Add(3 * time.Hour).Format("15:04")
				})
				wait := time.Duration(minutes) * time.Minute
				time.Sleep(wait - time.Nanosecond)
				require.NoError(t, s.run(ctx, false))
				require.Empty(t, p.orders)
				time.Sleep(time.Nanosecond)
				// Read-only provider checks must not postpone the due rescue.
				require.NoError(t, s.Check(ctx))
				require.Empty(t, p.orders)
				require.NoError(t, s.run(ctx, false))
				require.Len(t, p.orders, 1)
				require.Equal(t, "stalled", p.orders[0].TimeoutRetryOf)
				require.Equal(t, "pro20x", p.orders[0].Plan)
				state, err := decodeAccountRefillState(store.raw)
				require.NoError(t, err)
				require.Equal(t, 1, state.RotationCursor, "rescue must preserve plan rotation")
				require.False(t, state.Jobs[1].BudgetReleased)
				require.Equal(t, 120.0, state.Usage[created.In(refillTimezone).Format("2006-01-02")].Points)
				// Restart, repeated ticks and status updates must not buy another
				// order before the newest reservation's own deadline.
				s = &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: s.accounts, admin: s.admin, groups: s.groups, loads: s.loads}
				for i := 0; i < 3; i++ {
					require.NoError(t, s.run(ctx, false))
				}
				time.Sleep(wait - time.Nanosecond)
				require.NoError(t, s.run(ctx, false))
				require.Len(t, p.orders, 1)
				time.Sleep(time.Nanosecond)
				require.NoError(t, s.run(ctx, false))
				require.Len(t, p.orders, 2)
				require.Equal(t, p.orders[0].ID, p.orders[1].TimeoutRetryOf)
				require.NotEqual(t, p.orders[0].ID, p.orders[1].ID)
			})
		})
	}
}

func TestRefillTimeoutHandlesEveryUnfinishedStateAndIntervalMode(t *testing.T) {
	for _, status := range []string{"submitting", "queued", "processing", "review", "blocked", "supplier_unknown", "awaiting_delivery", "awaiting_push", "importing", "import_failed"} {
		t.Run(status, func(t *testing.T) {
			s, store, p, _ := failureRefillHarness(t)
			seedStalledRefill(t, store, status, time.Now().Add(-11*time.Minute))
			store.mutate(t, func(v *accountRefillState) {
				v.Config.Mode = "interval"
				v.NextIntervalAt = time.Now().Add(time.Hour)
				v.IntervalRemaining = 0
			})
			require.NoError(t, s.run(context.Background(), false))
			require.Len(t, p.orders, 1)
			v, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Equal(t, status, v.Jobs[1].Status)
			require.Zero(t, v.IntervalRemaining)
			require.Equal(t, "stalled", v.Jobs[0].TimeoutRetryOf)
		})
	}
}

func TestRefillTimeoutDoesNotPurchaseWithoutEligibilityOrFunds(t *testing.T) {
	for _, kind := range []string{"disabled", "automation off", "monitor off", "push off", "healthy", "new pending", "budget", "price", "balance", "backoff", "preview", "save failure", "recovered during quote"} {
		t.Run(kind, func(t *testing.T) {
			s, store, p, accounts := failureRefillHarness(t)
			seedStalledRefill(t, store, "review", time.Now().Add(-20*time.Minute))
			store.mutate(t, func(v *accountRefillState) {
				disabled := false
				switch kind {
				case "disabled":
					v.Config.TimeoutRetryEnabled = &disabled
				case "automation off":
					v.Config.Enabled = false
				case "monitor off":
					v.Config.MonitorEnabled = false
				case "push off":
					v.Config.PushEnabled = false
				case "budget":
					v.Config.DailyMaxPoints = 60
				case "price":
					v.Config.MaxUnitPrice = 1
				case "balance":
					p.available = 1
				case "backoff":
					v.ProviderRetryAt = time.Now().Add(time.Hour)
				case "new pending":
					v.Jobs = append(v.Jobs, AccountRefillJob{ID: "fresh", Status: "review", Plan: "pro5x", CreatedAt: time.Now(), NextAttemptAt: time.Now().Add(time.Hour)})
				}
			})
			if kind == "healthy" {
				accounts.accounts = []Account{refillTestAccount(time.Now().Add(3 * time.Hour))}
			}
			if kind == "recovered during quote" {
				p.afterCatalog = func() { accounts.accounts = []Account{refillTestAccount(time.Now().Add(3 * time.Hour))} }
			}
			store.failSave = kind == "save failure"
			err := s.run(context.Background(), kind == "preview")
			if kind == "save failure" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, p.orders)
		})
	}
}

func TestRefillTimeoutConcurrentWorkersReserveOneOrder(t *testing.T) {
	s, base, p, _ := failureRefillHarness(t)
	seedStalledRefill(t, base, "review", time.Now().Add(-20*time.Minute))
	store := &refillTimingStore{raw: base.raw}
	s.store = store
	other := &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: s.accounts, admin: s.admin, groups: s.groups, loads: s.loads}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, worker := range []*AccountRefillService{s, other} {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- worker.run(context.Background(), false) }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.Len(t, p.orders, 1)
}

func TestRefillTimeoutLateFailureDoesNotQueueAnotherReplacement(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	seedStalledRefill(t, store, "review", time.Now().Add(-20*time.Minute))
	require.NoError(t, s.run(context.Background(), false))
	p.states["stalled"] = "failed"
	store.mutate(t, func(v *accountRefillState) { v.Jobs[1].NextAttemptAt = time.Time{} })
	require.NoError(t, s.run(context.Background(), false))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.False(t, v.Jobs[1].ReplacementPending)
	require.True(t, v.Jobs[1].BudgetReleased)
	require.Len(t, p.orders, 1)
}

func TestRefillTimeoutOverlappingGroupsShareFreshReservation(t *testing.T) {
	s, base, p, _ := failureRefillHarness(t)
	seedStalledRefill(t, base, "review", time.Now().Add(-20*time.Minute))
	base.mutate(t, func(v *accountRefillState) {
		v.Config.DeliveryGroupIDs = []int64{2, 3}
		v.Jobs[0].DeliveryGroupIDs = []int64{2, 3}
	})
	store := &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	s.store = store
	ctx := context.Background()
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	v, err := child.View(ctx)
	require.NoError(t, err)
	v.Config.Enabled = true
	require.NoError(t, child.Configure(ctx, v.Config, "", v.Revision))
	require.NoError(t, s.run(ctx, false))
	require.NoError(t, child.run(ctx, false))
	require.Len(t, p.orders, 1, "the new order will deliver to both pools")
	minutes, enabled := 7, false
	v, err = child.View(ctx)
	require.NoError(t, err)
	v.Config.TimeoutRetryMinutes, v.Config.TimeoutRetryEnabled = &minutes, &enabled
	require.NoError(t, child.Configure(ctx, v.Config, "", v.Revision))
	rootView, err := s.View(ctx)
	require.NoError(t, err)
	require.True(t, refillTimeoutRetryEnabled(rootView.Config))
	require.Equal(t, 10*time.Minute, refillTimeoutRetryDuration(rootView.Config))
}
