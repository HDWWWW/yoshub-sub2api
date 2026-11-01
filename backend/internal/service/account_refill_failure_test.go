//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failureRefillProvider struct {
	*manualRefillProvider
	orders       []AccountRefillJob
	states       map[string]string
	statusErr    error
	submitErr    error
	afterCatalog func()
	beforeSubmit func(AccountRefillJob)
}

func (p *failureRefillProvider) Submit(_ context.Context, _ string, j AccountRefillJob) (*RefillProviderJob, error) {
	if p.beforeSubmit != nil {
		p.beforeSubmit(j)
	}
	p.orders = append(p.orders, j)
	return &RefillProviderJob{ID: j.ID, Status: "queued", Price: j.Price}, p.submitErr
}

func (p *failureRefillProvider) Status(_ context.Context, _ string, id string) (*RefillProviderJob, error) {
	status := p.states[id]
	if status == "" {
		status = "processing"
	}
	return &RefillProviderJob{ID: id, Status: status, ResultReady: status == "succeeded"}, p.statusErr
}

func (p *failureRefillProvider) Catalog(ctx context.Context, token string) (*RefillCatalog, error) {
	if p.afterCatalog != nil {
		p.afterCatalog()
	}
	return p.manualRefillProvider.Catalog(ctx, token)
}

func failureRefillHarness(t *testing.T) (*AccountRefillService, *refillTestStore, *failureRefillProvider, *refillTestAccounts) {
	t.Helper()
	s, store, base, accounts := refillHarness(t)
	p := &failureRefillProvider{manualRefillProvider: &manualRefillProvider{base, 500}, states: map[string]string{}}
	s.provider = p
	store.mutate(t, func(v *accountRefillState) {
		v.Config.MinReady, v.Config.MaxReady, v.Config.MaxInFlight = 1, 1, 1
		v.Config.RotationEnabled = true
		v.Config.Rotation = []AccountRefillRotationStep{{Plan: "pro20x", Count: 1}, {Plan: "pro5x", Count: 1}}
	})
	return s, store, p, accounts
}

func TestRefillFailureReplacementSharesDemandAndPreservesRotation(t *testing.T) {
	s, store, p, accounts := failureRefillHarness(t)
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	first := p.orders[0]
	require.Equal(t, "pro20x", first.Plan)
	for attempt := 1; attempt <= 2; attempt++ {
		previous := p.orders[len(p.orders)-1]
		p.states[previous.ID] = "failed"
		refillDue(t, store)
		// A restart has no process-local replacement queue to replay.
		s = &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
		p.beforeSubmit = func(j AccountRefillJob) {
			v, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Equal(t, previous.ID, j.ReplacesJobID)
			require.Equal(t, j.ID, v.Jobs[1].ReplacementJobID, "link is durable before a paid request")
			require.False(t, v.Jobs[1].ReplacementPending)
			require.Equal(t, 60.0, v.Usage[j.CreatedAt.In(refillTimezone).Format("2006-01-02")].Points)
		}
		require.NoError(t, s.run(ctx, false))
		require.Len(t, p.orders, attempt+1, "normal demand and replacement must reserve only one order")
		latest := p.orders[len(p.orders)-1]
		require.NotEqual(t, previous.ID, latest.ID)
		require.Equal(t, first.Plan, latest.Plan)
		require.Contains(t, latest.ReasonDetail, "不推进套餐轮转")
		v, err := s.View(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, v.RotationCursor)
		require.Equal(t, attempt+1, v.Daily.Count)
		require.Equal(t, 60.0, v.Daily.Points, "each failure releases only its own reservation")
		for tick := 0; tick < 3; tick++ {
			refillDue(t, store)
			require.NoError(t, s.run(ctx, false))
		}
		require.Len(t, p.orders, attempt+1, "the in-flight replacement already covers the shortage")
	}
	p.beforeSubmit = nil
	latest := p.orders[len(p.orders)-1]
	p.states[latest.ID] = "succeeded"
	for i := 0; i < 2; i++ {
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, p.orders, 3)
	require.Equal(t, 1, accounts.creates)
	// A later independent shortage resumes the next original rotation step.
	accounts.accounts[0].Schedulable = false
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 4)
	require.Equal(t, "pro5x", p.orders[3].Plan)
	require.Empty(t, p.orders[3].ReplacesJobID)
}

func TestRefillFailureReplacementKeepsUnknownSubmissionIdempotent(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	p.states[p.orders[0].ID] = "failed"
	p.submitErr = errors.New("connection closed after sending replacement")
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	replacement := p.orders[1]
	p.submitErr = nil
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 3)
	require.Equal(t, replacement.ID, p.orders[2].ID)
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 2)
	require.Equal(t, 2, v.Daily.Count)
	require.Equal(t, 1, v.RotationCursor)
}

func TestRefillFailureReplacementRequiresConfirmedFailure(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		err          error
	}{
		{"review", "review", nil},
		{"processing", "processing", nil},
		{"timeout", "failed", errors.New("timeout")},
		{"rejected", "", &refillProviderError{code: 409}},
		{"rate_limit", "", &refillProviderError{code: 429, retryAfter: time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, p, _ := failureRefillHarness(t)
			require.NoError(t, s.run(context.Background(), false))
			p.states[p.orders[0].ID], p.statusErr = tc.status, tc.err
			refillDue(t, store)
			require.NoError(t, s.run(context.Background(), false))
			require.Len(t, p.orders, 1)
			v, err := s.View(context.Background())
			require.NoError(t, err)
			require.False(t, v.Jobs[0].ReplacementPending)
			require.Equal(t, 60.0, v.Daily.Points)
		})
	}
}

func TestRefillFailureReplacementUsesExistingGuards(t *testing.T) {
	for _, guard := range []string{"disabled", "monitor", "push", "window", "cooldown", "count", "points", "price", "wallet", "preview", "pending"} {
		t.Run(guard, func(t *testing.T) {
			s, store, p, _ := failureRefillHarness(t)
			ctx := context.Background()
			require.NoError(t, s.run(ctx, false))
			p.states[p.orders[0].ID] = "failed"
			// Observe the failure without allowing a new reservation yet.
			store.mutate(t, func(v *accountRefillState) { v.Config.Enabled = false })
			refillDue(t, store)
			require.NoError(t, s.run(ctx, false))
			refillDue(t, store)
			store.mutate(t, func(v *accountRefillState) {
				v.Config.Enabled = true
				switch guard {
				case "disabled":
					v.Config.Enabled = false
				case "monitor":
					v.Config.MonitorEnabled = false
				case "push":
					v.Config.PushEnabled = false
				case "window":
					v.Config.WindowEnabled = true
					v.Config.WindowStart = time.Now().In(refillTimezone).Add(time.Hour).Format("15:04")
					v.Config.WindowEnd = time.Now().In(refillTimezone).Add(2 * time.Hour).Format("15:04")
				case "cooldown":
					v.LastSubmitAt = time.Now()
				case "count":
					v.Config.DailyMaxCount = 1
				case "points":
					v.Config.DailyMaxPoints = 59
				case "price":
					v.Config.MaxUnitPrice = 59
				case "wallet":
					p.available = 59
				case "pending":
					v.Jobs = append(v.Jobs, AccountRefillJob{ID: "other-order", Status: "queued", SupplierID: "other-order", NextAttemptAt: time.Now().Add(time.Hour)})
				}
			})
			require.NoError(t, s.run(ctx, guard == "preview"))
			require.Len(t, p.orders, 1)
			v, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.True(t, v.Jobs[0].ReplacementPending)
			require.Equal(t, 1, v.RotationCursor)
		})
	}
}

func TestRefillFailureReplacementCancelsWhenDemandRecovers(t *testing.T) {
	for _, slowCatalog := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-check", true: "during-provider-call"}[slowCatalog], func(t *testing.T) {
			s, store, p, accounts := failureRefillHarness(t)
			ctx := context.Background()
			require.NoError(t, s.run(ctx, false))
			p.states[p.orders[0].ID] = "failed"
			recoverPool := func() { accounts.accounts = []Account{refillTestAccount(time.Now().Add(2 * time.Hour))} }
			if slowCatalog {
				p.afterCatalog = recoverPool
			} else {
				recoverPool()
			}
			refillDue(t, store)
			require.NoError(t, s.run(ctx, false))
			require.Len(t, p.orders, 1)
			v, err := s.View(ctx)
			require.NoError(t, err)
			require.False(t, v.Jobs[0].ReplacementPending)
			p.afterCatalog, accounts.accounts = nil, nil
			refillDue(t, store)
			require.NoError(t, s.run(ctx, false))
			require.Equal(t, "pro5x", p.orders[1].Plan, "a later shortage is a new rotation slot")
		})
	}
}

func TestRefillFailureReplacementIntervalRestoresOnlyOneSlot(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	ctx := context.Background()
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Mode = "interval"
		v.NextIntervalAt = time.Now().Add(-time.Second)
	})
	require.NoError(t, s.run(ctx, false))
	p.states[p.orders[0].ID] = "failed"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2)
	require.Equal(t, "pro20x", p.orders[1].Plan)
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Zero(t, v.IntervalRemaining)
	require.Equal(t, 1, v.RotationCursor)
	for i := 0; i < 3; i++ {
		refillDue(t, store)
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, p.orders, 2)
}

func TestRefillFailureReplacementDoesNotReplayHistoricalOrManualFailures(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	ctx := context.Background()
	store.mutate(t, func(v *accountRefillState) {
		v.Jobs = []AccountRefillJob{{ID: "old-failure", Status: "failed", SupplierStatus: "failed", SupplierID: "old-failure", Plan: "pro5x", BudgetReleased: true}}
	})
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, "pro20x", p.orders[0].Plan)
	require.Empty(t, p.orders[0].ReplacesJobID)
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Enabled = false
		v.Jobs[0].ManualRequestID, v.Jobs[0].Reason = "manual-request", "手动补号"
	})
	p.states[p.orders[0].ID] = "failed"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.False(t, v.Jobs[0].ReplacementPending)
}

func TestRefillFailureReplacementReservationMustPersistAtomically(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	p.states[p.orders[0].ID] = "failed"
	store.mutate(t, func(v *accountRefillState) { v.Config.Enabled = false })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	refillDue(t, store)
	store.mutate(t, func(v *accountRefillState) { v.Config.Enabled = true })
	store.failSave = true
	require.Error(t, s.run(ctx, false))
	require.Len(t, p.orders, 1)
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, v.Jobs[0].ReplacementPending)
	require.Empty(t, v.Jobs[0].ReplacementJobID)
	store.failSave = false
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2)
	require.Equal(t, "pro20x", p.orders[1].Plan)
}

func TestRefillFailureReplacementStillRequiresCapacityObservation(t *testing.T) {
	s, store, p, accounts := failureRefillHarness(t)
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	p.states[p.orders[0].ID] = "failed"
	accounts.accounts = []Account{refillTestAccount(time.Now().Add(2 * time.Hour))}
	s.loads = refillTestLoads{map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 60}}}
	store.mutate(t, func(v *accountRefillState) { v.Config.MaxReady = 2 })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1, "a failed order cannot bypass a new observation window")
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.True(t, v.Jobs[0].ReplacementPending)
	require.Equal(t, "pro20x", v.NextPlan)
	feedRefillCapacity(s, v.Config, v.Revision, time.Now(), v.Config.ConcurrencySustainSeconds)
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2)
	require.Equal(t, "pro20x", p.orders[1].Plan)
}

func TestRefillFailureReplacementHonorsCrossGroupPendingDelivery(t *testing.T) {
	s, base, p, _ := failureRefillHarness(t)
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	p.states[p.orders[0].ID] = "failed"
	store := &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	s.store = store
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	v, err := child.View(ctx)
	require.NoError(t, err)
	c := v.Config
	c.DeliveryGroupIDs = []int64{2, 3}
	require.NoError(t, child.Configure(ctx, c, "", 0))
	r := manualRequest()
	r.GroupID = 3
	require.NoError(t, child.Manual(ctx, r))
	refillDue(t, base)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 1, "an order delivering from another group already covers this shortage")
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, v.Evaluation.Pending)
	require.True(t, v.Jobs[0].ReplacementPending)
}
