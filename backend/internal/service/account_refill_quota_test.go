//go:build unit

package service

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillPlanQuotaUsesEachAccountAndPreservesFallback(t *testing.T) {
	now := time.Now()
	c := refillTestConfig()
	c.MinRemaining5h, c.MinRemaining7d = 8, 8
	c.Plan = "pro20x"
	c.PlanQuotaThresholds = map[string]AccountRefillQuotaThreshold{
		"pro5x":  {MinRemaining5h: 15, MinRemaining7d: 16},
		"pro20x": {MinRemaining5h: 10, MinRemaining7d: 11},
	}
	accounts := []Account{}
	for i, plan := range []string{"pro5x", "pro20x", "unknown", "Pro Lite", "chatgptpro"} {
		a := refillTestAccount(now.Add(2 * time.Hour))
		a.ID = int64(i + 1)
		a.Credentials = map[string]any{"plan_type": plan}
		a.Extra = map[string]any{"codex_5h_used_percent": 88, "codex_7d_used_percent": 70,
			"codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339), "codex_7d_reset_at": now.Add(time.Hour).Format(time.RFC3339)}
		accounts = append(accounts, a)
	}
	e := evaluateAccountRefill(c, accounts, nil, nil, now)
	require.Equal(t, []string{"quota_low", "ready", "ready", "quota_low", "ready"}, []string{e.Accounts[0].State, e.Accounts[1].State, e.Accounts[2].State, e.Accounts[3].State, e.Accounts[4].State})
	require.Equal(t, 15.0, e.Accounts[0].MinRemaining5h)
	require.Equal(t, 10.0, e.Accounts[1].MinRemaining5h)
	require.Equal(t, 8.0, e.Accounts[2].MinRemaining5h)
	// The 7d window is independent, with the same plan's override.
	accounts[1].Extra["codex_7d_used_percent"] = 90
	e = evaluateAccountRefill(c, accounts, nil, nil, now)
	require.Equal(t, "quota_low", e.Accounts[1].State)
	c.MinReady = 5
	e = evaluateAccountRefill(c, accounts, nil, nil, now)
	detail := accountRefillReasonDetail(c, e)
	require.Contains(t, detail, "pro5x 额度低于下限（5h 15.00% / 7d 16.00%）")
	require.Contains(t, detail, "pro20x 额度低于下限（5h 10.00% / 7d 11.00%）")
	delete(c.PlanQuotaThresholds, "pro5x")
	e = evaluateAccountRefill(c, accounts, nil, nil, now)
	require.Equal(t, "ready", e.Accounts[0].State)
	require.Equal(t, 8.0, e.Accounts[0].MinRemaining5h)
}

func TestRefillPlanQuotaPurchaseIdentityBoundaryAndZero(t *testing.T) {
	now := time.Now()
	a := refillTestAccount(now.Add(2 * time.Hour))
	a.Credentials = map[string]any{"plan_type": "pro"}
	a.Extra = map[string]any{"refill_job_id": "original-job", "codex_5h_used_percent": 85,
		"codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339)}
	jobs := []AccountRefillJob{{ID: "original-job", AccountID: a.ID, Plan: "pro5x", Status: "imported"}}
	require.Equal(t, "pro5x", refillAccountPlan(&a, jobs))
	jobs[0].AccountID++
	require.Equal(t, "pro20x", refillAccountPlan(&a, jobs))
	a.Extra["refill_plan"] = "pro5x"
	require.Equal(t, "pro5x", refillAccountPlan(&a, nil))
	c := refillTestConfig()
	c.PlanQuotaThresholds = map[string]AccountRefillQuotaThreshold{"pro5x": {MinRemaining5h: 15}}
	require.Equal(t, "ready", evaluateAccountRefill(c, []Account{a}, nil, nil, now).Accounts[0].State, "equal to the lower bound is still ready")
	a.Extra["codex_5h_used_percent"] = 85.1
	require.Equal(t, "quota_low", evaluateAccountRefill(c, []Account{a}, nil, nil, now).Accounts[0].State)
	c.PlanQuotaThresholds["pro5x"] = AccountRefillQuotaThreshold{}
	a.Extra["codex_5h_used_percent"] = 100
	require.Equal(t, "ready", evaluateAccountRefill(c, []Account{a}, nil, nil, now).Accounts[0].State)
}

func TestRefillPlanQuotaValidation(t *testing.T) {
	for _, value := range []float64{-1, 101, math.NaN(), math.Inf(1)} {
		for _, threshold := range []AccountRefillQuotaThreshold{{MinRemaining5h: value}, {MinRemaining7d: value}} {
			c := refillTestConfig()
			c.PlanQuotaThresholds = map[string]AccountRefillQuotaThreshold{"pro5x": threshold}
			require.Error(t, c.Validate())
		}
	}
	c := refillTestConfig()
	c.PlanQuotaThresholds = map[string]AccountRefillQuotaThreshold{"unknown": {}}
	require.Error(t, c.Validate())
	c.PlanQuotaThresholds = map[string]AccountRefillQuotaThreshold{"pro5x": {MinRemaining5h: 0, MinRemaining7d: 100}, "pro20x": {MinRemaining5h: 10.5, MinRemaining7d: 15}}
	require.NoError(t, c.Validate())
}
