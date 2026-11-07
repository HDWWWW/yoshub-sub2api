//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type manualRefillProvider struct {
	*refillTestProvider
	available float64
}

func (p *manualRefillProvider) Catalog(context.Context, string) (*RefillCatalog, error) {
	return &RefillCatalog{Available: p.available, Items: []RefillQuote{
		{Service: "refill", Plan: "pro5x", Price: 60, PriceVersion: 1, Enabled: true},
		{Service: "refill", Plan: "pro20x", Price: 60, PriceVersion: 2, Enabled: true},
	}}, nil
}
func (p *manualRefillProvider) Identity(ctx context.Context, token string) (*RefillWallet, *RefillKeyInfo, error) {
	wallet, key, err := p.refillTestProvider.Identity(ctx, token)
	wallet.Available = p.available
	return wallet, key, err
}
func manualRequest() ManualAccountRefillRequest {
	return ManualAccountRefillRequest{RequestID: fmt.Sprintf("%d:%s", time.Now().UnixMilli(), uuid.NewString()), Plan: "pro20x", Revision: 1}
}
func TestManualForceRefillBypassesBlockedOrdersAndDeduplicates(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	seedStalledRefill(t, store, "review", time.Now())
	ctx := context.Background()
	r := manualRequest()
	require.Error(t, s.Manual(ctx, r))
	r.Force = true
	require.NoError(t, s.Manual(ctx, r))
	require.NoError(t, s.Manual(ctx, r))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 2)
	require.True(t, v.Jobs[0].Force)
	require.Equal(t, "强制手动补号", v.Jobs[0].Reason)
	require.Equal(t, "review", v.Jobs[1].Status)
	require.False(t, v.Jobs[1].BudgetReleased)
	require.Equal(t, 120.0, v.Usage[time.Now().In(refillTimezone).Format("2006-01-02")].Points)
	r.Force = false
	require.Error(t, s.Manual(ctx, r), "cannot change the intent of an accepted key")
	require.Empty(t, p.orders, "the endpoint only reserves; the worker purchases")
	r = manualRequest()
	r.Force = true
	store.mutate(t, func(v *accountRefillState) { v.Config.DailyMaxPoints = 120 })
	require.ErrorContains(t, s.Manual(ctx, r), "预算")
	require.Empty(t, p.orders)
}
func TestManualRefillUsesSavedSnapshotWithAutomationOffAndDeduplicates(t *testing.T) {
	s, store, provider, accounts := refillHarness(t)
	s.provider = &manualRefillProvider{provider, 500}
	ctx := context.Background()
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Enabled, v.Config.MonitorEnabled = false, false
		v.Config.ImportConcurrency = 77
		v.Config.DeliveryGroupIDs = []int64{2, 3}
		v.Config.ImportExpiryGraceMinutes = 30
		v.Config.Delivery.Priority = 8
		v.Config.Delivery.RateMultiplier = 1.5
		v.Config.Delivery.LoadFactor = 99
		v.Config.Delivery.NameTemplate = "manual-{plan_type}"
		v.Config.Delivery.ModelMapping = map[string]string{"a": "b"}
	})
	r := manualRequest()
	require.NoError(t, s.Manual(ctx, r))
	require.Empty(t, provider.submitted, "reservation endpoint must not purchase")
	require.NoError(t, s.Manual(ctx, r))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 1)
	j := v.Jobs[0]
	require.Equal(t, "手动补号", j.Reason)
	require.Contains(t, j.ReasonDetail, "管理员手动提交")
	require.Equal(t, "pro20x", j.Plan)
	require.Equal(t, "pro5x", v.Config.Plan, "manual choice must not rewrite the automatic plan")
	require.Equal(t, 77, j.Concurrency)
	require.Equal(t, []int64{2, 3}, j.DeliveryGroupIDs)
	require.Equal(t, 30, *j.ExpiryGraceMinutes)
	require.Equal(t, v.Config.Delivery, j.DeliveryConfig)
	require.Equal(t, 1, v.Daily.Count)
	// Future edits must not alter the accepted job or make its retry a new order.
	store.mutate(t, func(v *accountRefillState) {
		v.Revision++
		v.Config.ImportConcurrency = 3
		v.Config.Delivery.Priority = 99
	})
	require.NoError(t, s.Manual(ctx, r))
	r.Plan = "pro5x"
	require.Error(t, s.Manual(ctx, r))
	require.NoError(t, s.run(ctx, false))
	require.Len(t, provider.submitted, 1)
	provider.status = "succeeded"
	for i := 0; i < 2; i++ {
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
	}
	require.Equal(t, 1, accounts.creates)
	a := accounts.accounts[0]
	require.Equal(t, "manual-pro20x", a.Name)
	require.Equal(t, 77, a.Concurrency)
	require.Equal(t, 8, a.Priority)
	require.Equal(t, []int64{2, 3}, a.GroupIDs)
	require.True(t, a.Schedulable)
	require.Equal(t, StatusActive, a.Status)
	require.Len(t, provider.submitted, 1)
}
func TestManualRefillRejectsWithoutPurchase(t *testing.T) {
	for _, tc := range []struct {
		name              string
		mutate            func(*accountRefillState)
		request           func(*ManualAccountRefillRequest)
		balance           float64
		noProxy, failSave bool
	}{
		{name: "revision", balance: 500, request: func(r *ManualAccountRefillRequest) { r.Revision++ }},
		{name: "plan", balance: 500, request: func(r *ManualAccountRefillRequest) { r.Plan = "unknown" }},
		{name: "expired request", balance: 500, request: func(r *ManualAccountRefillRequest) {
			r.RequestID = fmt.Sprintf("%d:%s", time.Now().Add(-26*time.Hour).UnixMilli(), uuid.NewString())
		}},
		{name: "no balance", balance: 10},
		{name: "price limit", balance: 500, mutate: func(v *accountRefillState) { v.Config.MaxUnitPrice = 5 }},
		{name: "budget", balance: 500, mutate: func(v *accountRefillState) { v.Config.DailyMaxPoints = 5 }},
		{name: "count", balance: 500, mutate: func(v *accountRefillState) {
			v.Usage[time.Now().In(refillTimezone).Format("2006-01-02")] = AccountRefillDailyUsage{Count: v.Config.DailyMaxCount}
		}},
		{name: "push disabled", balance: 500, mutate: func(v *accountRefillState) { v.Config.PushEnabled = false }},
		{name: "in flight", balance: 500, mutate: func(v *accountRefillState) { v.Jobs = []AccountRefillJob{{ID: "old", Status: "processing"}} }},
		{name: "full proxy pool", balance: 500, noProxy: true},
		{name: "storage failure", balance: 500, failSave: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, p, _ := refillHarness(t)
			s.provider = &manualRefillProvider{p, tc.balance}
			if tc.mutate != nil {
				store.mutate(t, tc.mutate)
			}
			if tc.noProxy {
				s.proxies = &refillTestProxies{free: false}
				store.mutate(t, func(v *accountRefillState) { v.Config.Delivery.ProxyMode = "random" })
			}
			store.failSave = tc.failSave
			r := manualRequest()
			if tc.request != nil {
				tc.request(&r)
			}
			require.Error(t, s.Manual(context.Background(), r))
			require.Empty(t, p.submitted)
		})
	}
}
func TestManualRefillTimeoutRestartAndHistoryPruning(t *testing.T) {
	s, store, p, _ := refillHarness(t)
	s.provider = &manualRefillProvider{p, 500}
	r := manualRequest()
	require.NoError(t, s.Manual(context.Background(), r))
	p.submitError = errors.New("timeout after acceptance")
	require.NoError(t, s.run(context.Background(), false))
	require.NoError(t, s.Manual(context.Background(), r))
	refillDue(t, store)
	p.submitError = nil
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.submitted, 2)
	require.Equal(t, p.submitted[0].ID, p.submitted[1].ID)
	store.mutate(t, func(v *accountRefillState) {
		v.Jobs[0].Status = "imported"
		for i := 0; i < 205; i++ {
			v.Jobs = append([]AccountRefillJob{{ID: uuid.NewString(), Status: "imported"}}, v.Jobs...)
		}
		v.Jobs = trimAccountRefillJobs(v.Jobs, time.Now())
	})
	require.NoError(t, s.Manual(context.Background(), r))
	v, err := s.View(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, v.Daily.Count)
	require.Len(t, v.Jobs, 201, "recent receipt must survive the ordinary history cap")
}
