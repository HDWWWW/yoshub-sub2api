//go:build unit

package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func expiryRefillHarness(t *testing.T) (*AccountRefillService, *refillTestStore, *failureRefillProvider, *refillTestAccounts) {
	t.Helper()
	s, store, provider, accounts := failureRefillHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		c := &state.Config
		c.Rotation = []AccountRefillRotationStep{{Plan: "pro5x", Count: 1}, {Plan: "pro20x", Count: 1}}
		c.MinReady, c.MaxReady, c.MaxInFlight = 1, 8, 1
		c.LeadMinutes, c.TargetConcurrency, c.ConcurrencySustainSeconds = 12, 85, 10
		c.CheckSeconds, c.CooldownSeconds = 5, 15
		c.MaxUnitPrice = 80
		c.PlanImportConcurrency = map[string]int{"pro5x": 55, "pro20x": 65}
	})
	now := time.Now()
	departing := refillTestAccount(now.Add(time.Hour))
	departing.Extra = map[string]any{AccountSourceExpiryExtraKey: now.Add(10 * time.Minute).Unix(), AccountSourceExpiryOriginExtraKey: "supplier_api"}
	remaining := refillTestAccount(now.Add(2 * time.Hour))
	remaining.ID = 2
	accounts.accounts = []Account{departing, remaining}
	return s, store, provider, accounts
}

func finishExpiryRefill(t *testing.T, s *AccountRefillService, store *refillTestStore, p *failureRefillProvider, id string) {
	t.Helper()
	p.states[id] = "succeeded"
	for range 2 {
		refillDue(t, store)
		require.NoError(t, s.run(context.Background(), false))
	}
}

func TestRefillExpiryFailureRetainsFiveXForReserveShortage(t *testing.T) {
	s, store, p, accounts := expiryRefillHarness(t)
	store.mutate(t, func(state *accountRefillState) { state.Config.MinReady = 2 })
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1, "a real minimum-reserve shortage can replenish without concurrency demand")
	first := p.orders[0]
	require.Equal(t, "pro5x", first.Plan)
	require.Equal(t, "账号临期，提前补充储备", first.Reason)
	require.Contains(t, first.ReasonDetail, "低于最低储备 2 个")
	require.Equal(t, int64(1), first.ExpiryTarget.AccountID)
	require.Contains(t, first.ReasonDetail, "接替账号 #1")
	p.states[first.ID] = "failed"
	store.mutate(t, func(state *accountRefillState) { state.Jobs[0].NextAttemptAt = time.Time{} })
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2, "confirmed failure retries the same lease while the reserve shortage remains")
	replacement := p.orders[1]
	require.Equal(t, "pro5x", replacement.Plan)
	require.Equal(t, first.ID, replacement.ReplacesJobID)
	require.Equal(t, first.ExpiryTarget, replacement.ExpiryTarget)
	finishExpiryRefill(t, s, store, p, replacement.ID)
	require.Equal(t, 1, accounts.creates)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.ExpiryDemands[0].Completed)
	require.Equal(t, 1, state.RotationCursor)
	// Reload durable state after the completed job has left the ordinary history.
	store.mutate(t, func(state *accountRefillState) { state.Jobs = nil })
	s = &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
	for range 3 {
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, p.orders, 2, "a successful lease replacement must not repeat while the original is still expiring")
	// A different expiring lease gets the next rotation slot.
	accounts.accounts[1].Extra = map[string]any{AccountSourceExpiryExtraKey: time.Now().Add(11 * time.Minute).Unix()}
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 3)
	require.Equal(t, "pro20x", p.orders[2].Plan)
	require.Equal(t, int64(2), p.orders[2].ExpiryTarget.AccountID)
}

func TestRefillExpiryDoesNotAddDemandWhenReserveIsSufficient(t *testing.T) {
	for _, mode := range []string{"capacity", "fixed"} {
		t.Run(mode, func(t *testing.T) {
			s, store, p, accounts := expiryRefillHarness(t)
			store.mutate(t, func(state *accountRefillState) {
				state.Config.Mode = mode
				disabled := false
				state.Config.ForecastEnabled = &disabled
			})
			accounts.accounts[0].Extra["codex_5h_used_percent"] = 99.0
			accounts.accounts[0].Extra["codex_5h_reset_at"] = time.Now().Add(time.Hour).Format(time.RFC3339)
			require.NoError(t, s.run(context.Background(), false))
			require.Empty(t, p.orders, "a low-quota expiring account does not require replacement when the reserve is sufficient")
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Len(t, state.ExpiryDemands, 1, "retain the supplier deadline for a future authorized purchase")
			require.Zero(t, state.Evaluation.Needed)
		})
	}
}

func TestRefillExpiryHonorsPurchaseGuards(t *testing.T) {
	for _, guard := range []string{"disabled", "monitor", "push", "window", "cooldown", "healthy_limit", "budget", "wallet", "preview", "interval"} {
		t.Run(guard, func(t *testing.T) {
			s, store, p, _ := expiryRefillHarness(t)
			store.mutate(t, func(state *accountRefillState) {
				state.Config.MinReady = 2
				switch guard {
				case "disabled":
					state.Config.Enabled = false
				case "monitor":
					state.Config.MonitorEnabled = false
				case "push":
					state.Config.PushEnabled = false
				case "window":
					state.Config.WindowEnabled = true
					state.Config.WindowStart = time.Now().In(refillTimezone).Add(time.Hour).Format("15:04")
					state.Config.WindowEnd = time.Now().In(refillTimezone).Add(2 * time.Hour).Format("15:04")
				case "cooldown":
					state.LastSubmitAt = time.Now()
				case "healthy_limit":
					state.Config.MinReady, state.Config.MaxReady = 1, 1
				case "budget":
					state.Config.DailyMaxPoints = 59
				case "wallet":
					p.available = 59
				case "interval":
					state.Config.Mode = "interval"
				}
			})
			require.NoError(t, s.run(context.Background(), guard == "preview"))
			require.Empty(t, p.orders)
		})
	}
}

func TestRefillExpiryKeepsUnfilledDemandAfterDeadlineAndRenewsIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, p, accounts := expiryRefillHarness(t)
		store.mutate(t, func(state *accountRefillState) {
			state.Config.MinReady = 2
			state.Config.DailyMaxPoints = 59
		})
		require.NoError(t, s.run(context.Background(), false))
		require.Empty(t, p.orders)
		time.Sleep(11 * time.Minute)
		store.mutate(t, func(state *accountRefillState) { state.Config.DailyMaxPoints = 3000 })
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, p.orders, 1, "the reserve shortage remains after the deadline")
		finishExpiryRefill(t, s, store, p, p.orders[0].ID)
		// Renewed service on the same account is a different lease.
		accounts.accounts[0].Extra[AccountSourceExpiryExtraKey] = time.Now().Add(10 * time.Minute).Unix()
		refillDue(t, store)
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, p.orders, 1, "a renewed lease cannot buy while the reserve is sufficient")
		store.mutate(t, func(state *accountRefillState) { state.Config.MinReady = 3 })
		refillDue(t, store)
		require.NoError(t, s.run(context.Background(), false))
		require.Len(t, p.orders, 2)
		require.NotEqual(t, p.orders[0].ExpiryTarget.SourceExpiresAt, p.orders[1].ExpiryTarget.SourceExpiresAt)
	})
}

func TestRefillExpiryDoesNotBackfillHistoricalExpiredAccounts(t *testing.T) {
	s, _, p, accounts := expiryRefillHarness(t)
	accounts.accounts[0].Extra[AccountSourceExpiryExtraKey] = time.Now().Add(-time.Hour).Unix()
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, p.orders)
}

func TestRefillExpiryRetainsDemandAndCompletionAcrossAccountErrors(t *testing.T) {
	s, store, p, accounts := expiryRefillHarness(t)
	ctx := context.Background()
	store.mutate(t, func(state *accountRefillState) {
		state.Config.MinReady = 2
		state.Config.Enabled = false
	})
	require.NoError(t, s.run(ctx, false))
	accounts.accounts[0].Status = StatusError
	store.mutate(t, func(state *accountRefillState) { state.Config.Enabled = true })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1, "an account error must not erase an existing expiry requirement")
	finishExpiryRefill(t, s, store, p, p.orders[0].ID)
	store.mutate(t, func(state *accountRefillState) { state.Jobs = nil })
	for _, status := range []string{StatusError, StatusActive} {
		accounts.accounts[0].Status = status
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
		require.Len(t, p.orders, 1, "recovering the same lease must not create a second replacement")
	}
}

func TestRefillExpiryCancelsRemovedOrRenewedLease(t *testing.T) {
	for _, change := range []string{"deleted", "removed_from_group", "renewed"} {
		t.Run(change, func(t *testing.T) {
			s, store, p, accounts := expiryRefillHarness(t)
			store.mutate(t, func(state *accountRefillState) { state.Config.Enabled = false })
			require.NoError(t, s.run(context.Background(), false))
			accounts.accounts[0].Status = StatusError
			switch change {
			case "deleted":
				accounts.accounts = accounts.accounts[1:]
			case "removed_from_group":
				accounts.accounts[0].GroupIDs = []int64{3}
			case "renewed":
				accounts.accounts[0].Extra[AccountSourceExpiryExtraKey] = time.Now().Add(time.Hour).Unix()
			}
			store.mutate(t, func(state *accountRefillState) { state.Config.Enabled = true })
			refillDue(t, store)
			require.NoError(t, s.run(context.Background(), false))
			require.Empty(t, p.orders)
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Empty(t, state.ExpiryDemands)
		})
	}
}

func TestRefillExpiryExistingDeliveriesCoverOnlyOneLeaseEach(t *testing.T) {
	s, store, p, accounts := expiryRefillHarness(t)
	second := refillTestAccount(time.Now().Add(11 * time.Minute))
	second.ID = 3
	accounts.accounts = append(accounts.accounts, second)
	store.mutate(t, func(state *accountRefillState) {
		minutes := 120 // This test covers delivery credits before their configured timeout.
		state.Config.TimeoutRetryMinutes = &minutes
		state.Config.MinReady = 3
		state.Config.MaxInFlight = 3
		// Even a delivery placed before the lead window covers one upcoming exit.
		state.Jobs = []AccountRefillJob{{ID: "legacy-delivery", GroupID: 2, Status: "queued", Plan: "pro20x", CreatedAt: time.Now().Add(-time.Hour), NextAttemptAt: time.Now().Add(time.Hour), DeliveryGroupIDs: []int64{2}}}
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.orders, 1)
	require.Equal(t, int64(3), p.orders[0].ExpiryTarget.AccountID)
	for range 3 {
		refillDue(t, store)
		store.mutate(t, func(state *accountRefillState) {
			for i := range state.Jobs {
				state.Jobs[i].NextAttemptAt = time.Now().Add(time.Hour)
			}
		})
		require.NoError(t, s.run(context.Background(), false))
	}
	require.Len(t, p.orders, 1, "two covered leases cannot keep ordering under MaxInFlight=3")
}

func TestRefillExpiryOverlappingGroupsShareFailureAndSuccess(t *testing.T) {
	s, base, p, accounts := expiryRefillHarness(t)
	ctx := context.Background()
	for i := range accounts.accounts {
		accounts.accounts[i].GroupIDs = []int64{2, 3}
	}
	base.mutate(t, func(state *accountRefillState) {
		state.Config.MinReady = 2
		state.Config.DeliveryGroupIDs = []int64{2, 3}
	})
	grouped := &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	s.store = grouped
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	v, err := child.View(ctx)
	require.NoError(t, err)
	c := v.Config
	c.Enabled, c.LeadMinutes, c.MinReady, c.MaxReady = true, 12, 1, 8
	c.DeliveryGroupIDs = []int64{2, 3}
	require.NoError(t, child.Configure(ctx, c, "", v.Revision))
	require.NoError(t, s.run(ctx, false))
	require.NoError(t, child.run(ctx, false))
	require.Len(t, p.orders, 1, "an overlapping group credits the same pending replacement")
	p.states[p.orders[0].ID] = "failed"
	base.mutate(t, func(state *accountRefillState) { state.Config.Enabled = false })
	refillDue(t, base)
	require.NoError(t, s.run(ctx, false))
	refillDue(t, grouped.children[3])
	require.NoError(t, child.run(ctx, false))
	require.Len(t, p.orders, 1, "a confirmed failure must be retried only by its owner")
	base.mutate(t, func(state *accountRefillState) { state.Config.Enabled = true })
	refillDue(t, base)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2)
	finishExpiryRefill(t, s, base, p, p.orders[1].ID)
	base.mutate(t, func(state *accountRefillState) { state.Jobs = nil })
	refillDue(t, grouped.children[3])
	require.NoError(t, child.run(ctx, false))
	require.Len(t, p.orders, 2, "a durable completion receipt must survive pruning and satisfy overlapping groups")
}

func TestRefillExpiryLegacyFailureRetainsItsLeaseWhenEarlierExitAppears(t *testing.T) {
	s, store, p, accounts := expiryRefillHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		minutes := 120 // Confirmed failures of non-expired orders retain their lease.
		state.Config.TimeoutRetryMinutes = &minutes
		state.Config.MinReady = 2
		state.RotationCursor = 1
		state.Jobs = []AccountRefillJob{{ID: "legacy-five", SupplierID: "legacy-five", GroupID: 2, Mode: state.Config.Mode,
			Status: "queued", Plan: "pro5x", CreatedAt: time.Now().Add(-time.Hour), NextAttemptAt: time.Now().Add(time.Hour), DeliveryGroupIDs: []int64{2}}}
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, p.orders)
	earlier := refillTestAccount(time.Now().Add(9 * time.Minute))
	earlier.ID = 3
	accounts.accounts = append(accounts.accounts, earlier)
	p.states["legacy-five"] = "failed"
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.orders, 1)
	require.Equal(t, "pro5x", p.orders[0].Plan)
	require.Equal(t, "legacy-five", p.orders[0].ReplacesJobID)
	require.Equal(t, int64(1), p.orders[0].ExpiryTarget.AccountID, "retry must keep the adopted lease instead of taking the new earlier exit")
}

func TestRefillExpiryReservationAndReceiptAreAtomic(t *testing.T) {
	s, store, p, _ := expiryRefillHarness(t)
	store.mutate(t, func(state *accountRefillState) { state.Config.MinReady = 2 })
	store.failSave = true
	require.Error(t, s.run(context.Background(), false))
	require.Empty(t, p.orders)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, state.ExpiryDemands)
	store.failSave = false
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.orders, 1)
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, p.orders[0].ID, state.ExpiryDemands[0].JobID)
}

func TestRefillRotationResetDoesNotReplayOldSlot(t *testing.T) {
	for _, failureBeforeReset := range []bool{false, true} {
		t.Run(map[bool]string{false: "in-flight", true: "already-failed"}[failureBeforeReset], func(t *testing.T) {
			s, store, p, _ := failureRefillHarness(t)
			ctx := context.Background()
			require.NoError(t, s.run(ctx, false))
			first := p.orders[0]
			if failureBeforeReset {
				p.states[first.ID] = "failed"
				store.mutate(t, func(state *accountRefillState) { state.Config.Enabled = false })
				refillDue(t, store)
				require.NoError(t, s.run(ctx, false))
			}
			v, err := s.View(ctx)
			require.NoError(t, err)
			c := v.Config
			c.Enabled = true
			c.Rotation = []AccountRefillRotationStep{{Plan: "pro5x", Count: 1}, {Plan: "pro20x", Count: 1}}
			require.NoError(t, s.Configure(ctx, c, "", v.Revision))
			p.states[first.ID] = "failed"
			refillDue(t, store)
			require.NoError(t, s.run(ctx, false))
			require.Len(t, p.orders, 2)
			require.Equal(t, "pro5x", p.orders[1].Plan)
			require.Empty(t, p.orders[1].ReplacesJobID)
		})
	}
}
