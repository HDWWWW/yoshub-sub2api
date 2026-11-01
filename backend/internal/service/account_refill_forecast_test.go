//go:build unit

package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func refillForecastAccounts(now time.Time) []Account {
	a, b := refillTestAccount(now.Add(4*time.Hour)), refillTestAccount(now.Add(4*time.Hour))
	a.ID, b.ID = 1, 2
	a.Concurrency, b.Concurrency = 100, 100
	a.Extra = map[string]any{"codex_5h_used_percent": 85.0, "codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339)}
	return []Account{a, b}
}

func refillForecastLoads() map[int64]*AccountLoadInfo {
	return map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 45}, 2: {CurrentConcurrency: 45}}
}

func feedRefillForecast(s *AccountRefillService, c AccountRefillConfig, revision int64, e AccountRefillEvaluation, until time.Time, seconds int) {
	for i := seconds; i >= 0; i-- {
		s.recordCapacitySample(c, revision, e, until.Add(-time.Duration(i)*time.Second))
	}
}

func TestRefillForecastRetainsLiveCapacityAndAllDemand(t *testing.T) {
	now := time.Now()
	for _, trigger := range []string{"quota_at_floor", "quota_below_floor", "supplier_lease"} {
		t.Run(trigger, func(t *testing.T) {
			c := refillTestConfig()
			accounts := refillForecastAccounts(now)
			switch trigger {
			case "quota_below_floor":
				accounts[0].Extra["codex_5h_used_percent"] = 86.0
			case "supplier_lease":
				accounts[0].Extra = map[string]any{AccountSourceExpiryExtraKey: now.Add(30 * time.Minute).Unix(), AccountSourceExpiryOriginExtraKey: "supplier_api"}
			}
			before := append([]Account(nil), accounts...)
			e := evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now)
			require.Equal(t, 90, e.InUse, "departing requests remain demand after a capacity loss")
			require.Equal(t, 200, e.Capacity, "quota floors do not alter the live capacity")
			require.Equal(t, 100, e.ForecastCapacity)
			require.Equal(t, 1, e.ForecastAccounts)
			require.Equal(t, refillForecastReason, e.Reason)
			require.Equal(t, 1, e.Needed)
			require.False(t, refillCapacityHigh(c, e))
			require.True(t, refillForecastHigh(c, e))
			require.Equal(t, before, accounts, "forecasting must not change scheduling or account settings")
		})
	}
	accounts := refillForecastAccounts(now)
	accounts[0].Schedulable = false
	e := evaluateAccountRefill(refillTestConfig(), accounts, refillForecastLoads(), nil, now)
	require.Equal(t, 90, e.InUse, "already unavailable accounts may still have running requests")
	require.Equal(t, 100, e.Capacity)
	require.Zero(t, e.ForecastAccounts, "already unavailable capacity is not removed twice")
	accounts[0].Schedulable = true
	accounts[0].AutoPauseOnExpired = true
	accounts[0].Extra[AccountSourceExpiryExtraKey] = now.Add(time.Hour).Unix()
	localDeadline := now.Add(-time.Hour)
	accounts[0].ExpiresAt = &localDeadline
	e = evaluateAccountRefill(refillTestConfig(), accounts, refillForecastLoads(), nil, now)
	require.Equal(t, "unavailable", e.Accounts[0].State, "an earlier local pause deadline already removes live capacity")
	require.Zero(t, e.ForecastAccounts)
}

func TestRefillForecastBoundariesUnknownQuotaAndPlanOverrides(t *testing.T) {
	now := time.Now()
	c := refillTestConfig()
	accounts := refillForecastAccounts(now)
	c.PlanQuotaThresholds = map[string]AccountRefillQuotaThreshold{"pro20x": {MinRemaining5h: 10}}
	accounts[0].Credentials = map[string]any{"plan_type": "pro20x"}
	require.Zero(t, evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now).ForecastAccounts)
	accounts[0].Extra["codex_5h_used_percent"] = 90
	require.Equal(t, 1, evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now).ForecastAccounts)
	c.PlanQuotaThresholds["pro20x"] = AccountRefillQuotaThreshold{}
	accounts[0].Extra["codex_5h_used_percent"] = 100
	require.Zero(t, evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now).ForecastAccounts, "zero disables the window")
	c = refillTestConfig()
	for _, extra := range []map[string]any{
		nil,
		{"codex_5h_used_percent": math.NaN()},
		{"codex_5h_used_percent": 101},
		{"codex_5h_used_percent": 99, "codex_5h_reset_at": now.Add(-time.Minute).Format(time.RFC3339)},
	} {
		accounts[0].Extra = extra
		require.Zero(t, evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now).ForecastAccounts)
	}
	accounts[0].Extra = nil
	outside := now.Add(time.Duration(c.LeadMinutes)*time.Minute + time.Second)
	accounts[0].ExpiresAt = &outside
	require.Zero(t, evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now).ForecastAccounts)
	inside := now.Add(time.Duration(c.LeadMinutes) * time.Minute)
	accounts[0].ExpiresAt = &inside
	require.Equal(t, 1, evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now).ForecastAccounts)
	for _, mode := range []string{"fixed", "interval"} {
		c.Mode = mode
		e := evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now)
		require.Zero(t, e.Needed, "prediction must not add purchases in other modes")
	}
}

func TestRefillForecastZeroCapacityAndFutureStockLimit(t *testing.T) {
	now := time.Now()
	c := refillTestConfig()
	c.MaxReady = 1
	accounts := refillForecastAccounts(now)[:1]
	loads := map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 1}}
	e := evaluateAccountRefill(c, accounts, loads, nil, now)
	require.Equal(t, 1, e.Ready, "existing reserve semantics at the exact floor remain unchanged")
	require.Zero(t, e.ForecastReady)
	require.Zero(t, e.ForecastCapacity)
	require.True(t, refillForecastHigh(c, e))
	require.Equal(t, refillForecastReason, e.Reason)
	require.Equal(t, 1, e.Needed, "a replacement may overlap with an account at the exact floor")
	s := &AccountRefillService{}
	feedRefillForecast(s, c, 1, e, now, 30)
	o := s.forecastObservation(c, 1, e, now)
	require.True(t, o.Ready)
	require.True(t, o.NoCapacity, "zero capacity must not render as a finite utilization")
	loads[1].CurrentConcurrency = 0
	e = evaluateAccountRefill(c, accounts, loads, nil, now)
	require.False(t, refillForecastHigh(c, e))
	require.Zero(t, e.Needed, "no demand cannot trigger a speculative concurrency purchase")
}

func TestRefillForecastCombinesLossesAndCreditsSavedPendingCapacity(t *testing.T) {
	now := time.Now()
	c := refillTestConfig()
	c.MaxInFlight = 5
	accounts := refillForecastAccounts(now)
	third := refillTestAccount(now.Add(10 * time.Minute))
	third.ID, third.Concurrency = 3, 100
	accounts = append(accounts, third)
	loads := refillForecastLoads()
	loads[3] = &AccountLoadInfo{CurrentConcurrency: 10}
	e := evaluateAccountRefill(c, accounts, loads, nil, now)
	require.Equal(t, 2, e.ForecastAccounts)
	require.Equal(t, 100, e.ForecastCapacity)
	require.Equal(t, 100, e.InUse)
	for _, tc := range []struct{ pending, needed int }{{10, 1}, {30, 0}, {0, 0}} {
		jobs := []AccountRefillJob{{Status: "queued", Concurrency: tc.pending}}
		e = evaluateAccountRefill(c, accounts, loads, jobs, now)
		require.Equal(t, tc.needed, e.Needed)
		if tc.pending > 0 {
			require.Equal(t, tc.pending, e.PendingCapacity, "credit saved order capacity instead of current import settings")
		} else {
			require.Equal(t, c.ImportConcurrency, e.PendingCapacity, "legacy pending orders retain a conservative fallback")
		}
	}
	// Once imported, replacement capacity joins the live pool and ceases to be pending.
	replacement := refillTestAccount(now.Add(4 * time.Hour))
	replacement.ID, replacement.Concurrency = 4, 60
	e = evaluateAccountRefill(c, append(accounts, replacement), loads, []AccountRefillJob{{Status: "imported", Concurrency: 60}}, now)
	require.Zero(t, e.PendingCapacity)
	require.Equal(t, 160, e.ForecastCapacity)
	require.Zero(t, e.Needed)
}

func TestRefillForecastObservationSeparateAndResetsOnChangedDepartures(t *testing.T) {
	now := time.Now()
	c := refillTestConfig()
	e := evaluateAccountRefill(c, refillForecastAccounts(now), refillForecastLoads(), nil, now)
	s := &AccountRefillService{}
	feedRefillCapacity(s, c, 1, now, 30)
	require.True(t, s.capacityObservation(c, 1, now).Ready)
	require.False(t, s.forecastObservation(c, 1, e, now).Ready, "a current-capacity window cannot authorize a new forecast")
	feedRefillForecast(s, c, 1, e, now.Add(31*time.Second), 30)
	require.True(t, s.forecastObservation(c, 1, e, now.Add(31*time.Second)).Ready)
	require.False(t, s.capacityObservation(c, 1, now.Add(31*time.Second)).Ready)
	require.False(t, s.forecastObservation(c, 1, e, now.Add(34*time.Second)).Ready, "stale samples expire")
	e.Accounts[0].ID = 10
	require.Equal(t, "waiting", s.forecastObservation(c, 1, e, now.Add(31*time.Second)).Status)
	s.recordCapacitySample(c, 1, e, now.Add(32*time.Second))
	require.Zero(t, s.forecastObservation(c, 1, e, now.Add(32*time.Second)).SustainedSeconds)
	e.PendingCapacity = 100
	s.recordCapacitySample(c, 1, e, now.Add(33*time.Second))
	require.Equal(t, "below", s.forecastObservation(c, 1, e, now.Add(33*time.Second)).Status)
	e.PendingCapacity = 0
	s.recordCapacitySample(c, 1, e, now.Add(34*time.Second))
	require.False(t, s.forecastObservation(c, 1, e, now.Add(34*time.Second)).Ready)
	feedRefillForecast(s, c, 1, e, now.Add(65*time.Second), 30)
	require.True(t, s.forecastObservation(c, 1, e, now.Add(65*time.Second)).Ready)
	require.False(t, s.forecastObservation(c, 2, e, now.Add(65*time.Second)).Ready, "configuration revisions cannot reuse a forecast")
	c.MonitorEnabled = false
	require.Equal(t, "disabled", s.forecastObservation(c, 1, e, now.Add(65*time.Second)).Status)
}

func TestRefillForecastPurchasesOnlyAfterObservationAndAvoidsDuplicate(t *testing.T) {
	s, store, provider, accounts := refillHarness(t)
	now := time.Now()
	accounts.accounts = refillForecastAccounts(now)
	s.loads = refillTestLoads{refillForecastLoads()}
	store.mutate(t, func(v *accountRefillState) { v.Config.MaxInFlight = 5 })
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, provider.submitted)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	feedRefillForecast(s, state.Config, state.Revision, state.Evaluation, time.Now(), 30)
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, provider.submitted, 1)
	require.Equal(t, refillForecastReason, provider.submitted[0].Reason)
	require.Contains(t, provider.submitted[0].ReasonDetail, "保留全部运行中并发 90，容量由 200 预判为 100")
	require.Contains(t, provider.submitted[0].ReasonDetail, "持续观察已满 30 秒")
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, provider.submitted, 1, "pending replacement fills the projected gap even below the in-flight count limit")
	require.True(t, accounts.accounts[0].Schedulable)
	require.Equal(t, 100, accounts.accounts[0].Concurrency)
}

type refillForecastChangeProvider struct {
	*refillTestProvider
	change func()
}

func (p *refillForecastChangeProvider) Catalog(ctx context.Context, token string) (*RefillCatalog, error) {
	p.change()
	return p.refillTestProvider.Catalog(ctx, token)
}

func TestRefillForecastRechecksAfterProviderBeforeReservingMoney(t *testing.T) {
	for _, change := range []string{"quota_recovers", "demand_drops", "candidate_changes", "lease_renews"} {
		t.Run(change, func(t *testing.T) {
			s, store, provider, accounts := refillHarness(t)
			now := time.Now()
			accounts.accounts = refillForecastAccounts(now)
			loads := refillForecastLoads()
			s.loads = refillTestLoads{loads}
			if change == "lease_renews" {
				accounts.accounts[0].Extra = map[string]any{AccountSourceExpiryExtraKey: now.Add(20 * time.Minute).Unix()}
			}
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			e := evaluateAccountRefill(state.Config, accounts.accounts, loads, nil, now)
			feedRefillForecast(s, state.Config, state.Revision, e, time.Now(), 30)
			s.provider = &refillForecastChangeProvider{provider, func() {
				switch change {
				case "quota_recovers":
					accounts.accounts[0].Extra["codex_5h_used_percent"] = 10
				case "demand_drops":
					loads[1].CurrentConcurrency, loads[2].CurrentConcurrency = 0, 0
				case "candidate_changes":
					accounts.accounts[0].ID = 3
					loads[3] = loads[1]
				case "lease_renews":
					accounts.accounts[0].Extra[AccountSourceExpiryExtraKey] = now.Add(3 * time.Hour).Unix()
				}
			}}
			require.NoError(t, s.run(context.Background(), false))
			require.Empty(t, provider.submitted)
			after, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Empty(t, after.Jobs)
			require.Empty(t, after.Usage, "a resolved or unobserved forecast must not reserve paid work")
		})
	}
}

func TestRefillForecastCreditsOtherGroupsDelivery(t *testing.T) {
	s, base, _, accounts := refillHarness(t)
	accounts.accounts = refillForecastAccounts(time.Now())
	s.loads = refillTestLoads{refillForecastLoads()}
	base.mutate(t, func(v *accountRefillState) { v.Config.MaxInFlight = 5 })
	other := &refillTestStore{}
	s.store = &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{3: other}}
	other.mutate(t, func(v *accountRefillState) {
		v.Config.GroupID = 3
		v.Jobs = []AccountRefillJob{{ID: "other-group-order", GroupID: 3, DeliveryGroupIDs: []int64{2, 3}, Status: "processing", Concurrency: 30}}
	})
	v, err := s.View(context.Background())
	require.NoError(t, err)
	require.Equal(t, 30, v.Evaluation.PendingCapacity)
	require.Zero(t, v.Evaluation.Needed, "a shared delivery satisfies the projected shortage")
	require.Empty(t, v.Jobs, "rule histories remain isolated")
	other.mutate(t, func(v *accountRefillState) { v.Jobs[0].DeliveryGroupIDs = []int64{3} })
	v, err = s.View(context.Background())
	require.NoError(t, err)
	require.Zero(t, v.Evaluation.PendingCapacity)
	require.Equal(t, 1, v.Evaluation.Needed)
}

func TestRefillForecastHonorsExistingPurchaseGates(t *testing.T) {
	for _, gate := range []string{"disabled", "monitor_disabled", "push_paused", "cooldown", "budget", "max_ready", "max_pending", "review"} {
		t.Run(gate, func(t *testing.T) {
			s, store, p, accounts := refillHarness(t)
			accounts.accounts = refillForecastAccounts(time.Now())
			s.loads = refillTestLoads{refillForecastLoads()}
			store.mutate(t, func(v *accountRefillState) {
				switch gate {
				case "disabled":
					v.Config.Enabled = false
				case "monitor_disabled":
					v.Config.MonitorEnabled = false
				case "push_paused":
					v.Config.PushEnabled = false
				case "cooldown":
					v.LastSubmitAt = time.Now()
				case "budget":
					v.Config.DailyMaxPoints = 1
				case "max_ready":
					v.Config.MaxReady = 1
				case "max_pending":
					v.Jobs = []AccountRefillJob{{ID: "existing", Status: "queued", Concurrency: 1, NextAttemptAt: time.Now().Add(time.Hour)}}
				case "review":
					v.Jobs = []AccountRefillJob{{ID: "review", Status: "review", NextAttemptAt: time.Now().Add(time.Hour)}}
				}
			})
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.NoError(t, s.evaluate(context.Background(), &state, time.Now()))
			feedRefillForecast(s, state.Config, state.Revision, state.Evaluation, time.Now(), 30)
			require.NoError(t, s.run(context.Background(), false))
			require.Empty(t, p.submitted)
		})
	}
}

func TestRefillForecastCannotBorrowAnotherTriggersAuthorization(t *testing.T) {
	for _, trigger := range []string{"minimum_reserve", "current_concurrency"} {
		t.Run(trigger, func(t *testing.T) {
			s, store, p, accounts := refillHarness(t)
			accounts.accounts = refillForecastAccounts(time.Now())
			loads := refillForecastLoads()
			s.loads = refillTestLoads{loads}
			store.mutate(t, func(v *accountRefillState) {
				v.Config.MaxInFlight = 10
				if trigger == "minimum_reserve" {
					v.Config.MinReady, v.Config.ImportConcurrency = 3, 10
					loads[1].CurrentConcurrency, loads[2].CurrentConcurrency = 75, 75
				} else {
					v.Config.ImportConcurrency = 20
					loads[1].CurrentConcurrency, loads[2].CurrentConcurrency = 90, 90
				}
			})
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.NoError(t, s.evaluate(context.Background(), &state, time.Now()))
			require.Greater(t, refillCapacityGap(state.Config, state.Evaluation.InUse, state.Evaluation.ForecastCapacity), 1)
			require.Equal(t, 1, state.Evaluation.Needed, "only the selected trigger's own gap may be reported")
			if trigger == "current_concurrency" {
				feedRefillCapacity(s, state.Config, state.Revision, time.Now(), 30)
			}
			require.NoError(t, s.run(context.Background(), false))
			require.Len(t, p.submitted, 1, "the independent ready trigger may buy its one required account")
			require.NotEqual(t, refillForecastReason, p.submitted[0].Reason)
			refillDue(t, store)
			require.NoError(t, s.run(context.Background(), false))
			require.Len(t, p.submitted, 1, "the larger remaining forecast gap must earn its own continuous observation")
			view, err := s.View(context.Background())
			require.NoError(t, err)
			require.Equal(t, refillForecastReason, view.Evaluation.Reason)
			require.False(t, view.ForecastObservation.Ready)
		})
	}
}
