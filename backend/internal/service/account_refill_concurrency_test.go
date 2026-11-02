//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillPlanConcurrencyValidationAndLegacyFallback(t *testing.T) {
	c := refillTestConfig()
	c.ImportConcurrency = 77
	require.Equal(t, 77, c.importConcurrencyForPlan("pro5x"))
	c.PlanImportConcurrency = map[string]int{"pro5x": 50, "pro20x": 60}
	require.NoError(t, c.Validate())
	require.Equal(t, 50, c.importConcurrencyForPlan("pro5x"))
	require.Equal(t, 60, c.importConcurrencyForPlan("pro20x"))
	delete(c.PlanImportConcurrency, "pro20x")
	require.Equal(t, 77, c.importConcurrencyForPlan("pro20x"))
	for _, overrides := range []map[string]int{{"unknown": 50}, {"pro5x": 0}, {"pro20x": -1}, {"pro5x": 10001}} {
		c.PlanImportConcurrency = overrides
		require.Error(t, c.Validate())
	}
}

func TestManualRefillPlanConcurrencySnapshotSurvivesConfigChange(t *testing.T) {
	for _, tc := range []struct {
		plan        string
		concurrency int
	}{{"pro5x", 50}, {"pro20x", 60}} {
		t.Run(tc.plan, func(t *testing.T) {
			s, store, provider, accounts := refillHarness(t)
			s.provider = &manualRefillProvider{provider, 500}
			store.mutate(t, func(state *accountRefillState) {
				state.Config.Enabled = false
				state.Config.PlanImportConcurrency = map[string]int{"pro5x": 50, "pro20x": 60}
			})
			request := manualRequest()
			request.Plan = tc.plan
			require.NoError(t, s.Manual(context.Background(), request))
			store.mutate(t, func(state *accountRefillState) {
				state.Config.PlanImportConcurrency[tc.plan] = 99
				state.Config.ImportConcurrency = 88
			})
			require.NoError(t, s.run(context.Background(), false))
			require.Equal(t, tc.concurrency, provider.submitted[0].Concurrency)
			provider.status = "succeeded"
			for i := 0; i < 2; i++ {
				refillDue(t, store)
				require.NoError(t, s.run(context.Background(), false))
			}
			require.Equal(t, 1, accounts.creates)
			require.Equal(t, tc.concurrency, accounts.accounts[0].Concurrency)
		})
	}
}

func TestRefillCapacityUsesUpcomingPlansAndSavedReservations(t *testing.T) {
	c := refillTestConfig()
	c.TargetConcurrency = 50
	c.PlanImportConcurrency = map[string]int{"pro5x": 50, "pro20x": 60}
	c.RotationEnabled = true
	c.Rotation = []AccountRefillRotationStep{{Plan: "pro20x", Count: 1}, {Plan: "pro5x", Count: 2}}
	state := accountRefillState{Config: c, RotationCursor: 1}
	plans := refillPlannedPurchasePlans(&state)
	require.Equal(t, []string{"pro5x", "pro5x", "pro20x"}, plans[:3])
	// A 104-slot deficit needs three 50/50/60 orders, not two assumed 60s.
	require.Equal(t, 3, refillCapacityGap(c, 102, 100, plans...))
	require.Equal(t, 1, state.RotationCursor)
	state.Jobs = []AccountRefillJob{{ID: "failed", Plan: "pro20x", Status: "failed", SupplierStatus: "failed", SupplierID: "supplier", BudgetReleased: true, ReplacementPending: true, GroupID: c.GroupID, Mode: c.Mode}}
	plans = refillPlannedPurchasePlans(&state)
	require.Equal(t, []string{"pro20x", "pro5x", "pro5x"}, plans[:3])
	require.Equal(t, 2, refillCapacityGap(c, 102, 100, plans...))
	require.True(t, state.Jobs[0].ReplacementPending)
	jobs := []AccountRefillJob{{Plan: "pro5x", Status: "queued", Concurrency: 35}}
	c.PlanImportConcurrency["pro5x"] = 99
	e := evaluateAccountRefill(c, nil, nil, jobs, time.Now())
	require.Equal(t, 35, e.PendingCapacity)
}
