//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func riskLeaseExtra(at time.Time) map[string]any {
	return map[string]any{AccountSourceExpiryOriginExtraKey: "supplier_api", AccountSourceExpiryExtraKey: at.Unix(), AccountImportExpiryGraceExtraKey: 3600}
}

func TestAccountRiskSupplierReclaimBoundary(t *testing.T) {
	cutoff := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name  string
		extra map[string]any
		known bool
	}{
		{"1208", riskLeaseExtra(cutoff), true},
		{"Muze", map[string]any{"import_source": "muze_api", AccountSourceExpiryOriginExtraKey: "supplier_api", AccountSourceExpiryExtraKey: fmt.Sprint(cutoff.Unix())}, true},
		{"legacy import", map[string]any{"import_original_expires_at": float64(cutoff.Unix())}, true},
		{"Muze pending", map[string]any{AccountSourceExpiryOriginExtraKey: "supplier_pending", "import_original_expires_at": cutoff.Unix()}, false},
		{"local only", nil, false},
		{"malformed", map[string]any{AccountSourceExpiryOriginExtraKey: "supplier_api", AccountSourceExpiryExtraKey: "bad"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, accounts, _, _ := riskHarness()
			accounts.pool = accounts.pool[:1]
			a := &accounts.pool[0]
			local := cutoff.Add(time.Hour)
			a.Extra, a.ExpiresAt, a.AutoPauseOnExpired = tc.extra, &local, true
			require.Equal(t, tc.known, SchedulingMetadata(a).SourceExpiresAt != nil)
			for _, at := range []time.Time{cutoff.Add(-time.Nanosecond), cutoff, cutoff.Add(time.Second)} {
				pool, err := s.candidates(context.Background(), 7, at)
				require.NoError(t, err)
				require.Equal(t, tc.known && !at.Before(cutoff), len(pool) == 0)
				ignored, err := s.risk401Reclaimed(context.Background(), a, 0, at)
				require.NoError(t, err)
				require.Equal(t, tc.known && !at.Before(cutoff), ignored)
			}
		})
	}
}

func TestAccountRiskSupplierReclaimProbe401(t *testing.T) {
	for _, stop := range []bool{true, false} {
		for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
			t.Run(fmt.Sprintf("stop=%v/offset=%s", stop, offset), func(t *testing.T) {
				s, store, accounts, _, now := riskHarness()
				r := store.state.Rules[7]
				r.Config.Probe401Enabled = stop
				r.Config.Notify401Enabled = true
				job := riskDueJob(t, s, accounts, now)
				cutoff := now.Truncate(time.Second).Add(310 * time.Second)
				// Deadline synchronized after the job had already started.
				accounts.pool[0].Extra = riskLeaseExtra(cutoff)
				at := cutoff.Add(offset)
				e := readRiskProbe401(&job.account, r.Config.Model, strings.NewReader(`{"error":{"code":"token_revoked"}}`))
				require.NotNil(t, e)
				e.receivedAt = at
				require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{UpstreamStatus: 401, auth401: e}, errors.New("rejected"), at))
				require.Empty(t, store.state.ActiveProbes)
				require.Equal(t, stop && offset < 0, !accounts.pool[0].Schedulable)
				require.Equal(t, offset < 0, len(r.Events) == 1)
				if offset >= 0 {
					require.Nil(t, accounts.pool[0].TempUnschedulableUntil)
					require.Contains(t, r.Observation.LastProbeResult, "回收")
				}
			})
		}
	}
}

type riskOutsideGroup struct{ *riskTestAccounts }

func (r riskOutsideGroup) ListByGroup(ctx context.Context, id int64) ([]Account, error) {
	rows, err := r.riskTestAccounts.ListByGroup(ctx, id)
	return rows[:1], err
}

type riskDeadlineProbe struct {
	calls    int
	deadline time.Time
}

func (p *riskDeadlineProbe) RunRiskProbe(ctx context.Context, _ *Account, _ AccountRiskConfig) (AccountRiskProbeResult, error) {
	p.calls++
	p.deadline, _ = ctx.Deadline()
	<-ctx.Done()
	return AccountRiskProbeResult{}, ctx.Err()
}

func TestAccountRiskSupplierReclaimInFlightAndShadow(t *testing.T) {
	for _, startAfter := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			s, store, accounts, loads, now := riskHarness()
			src, owner := &accounts.pool[0], &accounts.pool[1]
			src.ParentAccountID = &owner.ID
			s.accounts = riskOutsideGroup{accounts}
			c := &store.state.Rules[7].Config
			c.Probe401Enabled, c.Notify401Enabled = true, true
			cutoff := now.Add(302 * time.Second)
			owner.Extra = riskLeaseExtra(cutoff)
			_, err := s.tick(context.Background(), now)
			require.NoError(t, err)
			time.Sleep(300 * time.Second)
			jobs, err := s.tick(context.Background(), time.Now())
			require.NoError(t, err)
			require.Len(t, jobs, 1)
			probe := &riskDeadlineProbe{}
			s.probe = probe
			if startAfter {
				time.Sleep(time.Until(cutoff))
			}
			s.runProbe(context.Background(), jobs[0])
			require.Equal(t, cutoff, time.Now().UTC())
			if startAfter {
				require.Zero(t, probe.calls)
			} else {
				require.Equal(t, 1, probe.calls)
				require.Equal(t, cutoff, probe.deadline)
			}
			require.Empty(t, store.state.ActiveProbes)
			require.Zero(t, loads.slots)
			require.Empty(t, store.state.Rules[7].Events)
			pool, err := s.candidates(context.Background(), 7, time.Now())
			require.NoError(t, err)
			require.Empty(t, pool)
			ignored, err := s.risk401Reclaimed(context.Background(), src, owner.ID, time.Now())
			require.NoError(t, err)
			require.True(t, ignored)
		})
	}
}

func TestAccountRiskSupplierReclaimLiveAndQueued401(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, accounts, _, now := riskHarness()
		r := store.state.Rules[7]
		r.Config.Notify401Enabled = true
		r.Config.CustomErrorEnabled = true
		r.Config.CustomErrorThreshold = 1
		r.Config.CustomErrorRules = []AccountRiskErrorMatch{{Status: 401}, {Keyword: "auth"}, {Status: 503}}
		counter := &risk429TestCounter{}
		s.counter429 = counter
		notifier := &riskNoticeStub{}
		s.notifier = notifier
		gw := &OpenAIGatewayService{accountRisk: s}
		ctx := WithHeaderOverrideRequestScope(context.Background(), 1, 1, &r.GroupID)
		a := accounts.pool[0]
		cutoff := now.Add(time.Second)
		// A live stream starts with a stale/pending snapshot before the lease sync.
		observer := gw.newAccountRiskModelObserver(ctx, nil, &a, "demo")
		accounts.pool[0].Extra = riskLeaseExtra(cutoff)
		gw.observeBusiness401(ctx, &a, 401, "HTTP")
		require.Len(t, r.Events, 1)
		time.Sleep(time.Second)
		gw.observeRiskHTTP(ctx, &a, &http.Response{StatusCode: 401})
		gw.observeRiskHandshake(ctx, &a, 401, []byte(`{"error":"auth"}`))
		observer.ObserveOpenAI([]byte(`{"type":"error","error":{"type":"authentication_error"}}`), "error")
		observer.ObserveOpenAI([]byte(`{"type":"response.failed","response":{"error":{"status":401}}}`), "response.failed")
		require.Empty(t, counter.counts)
		require.Len(t, r.Events, 1)
		require.True(t, accounts.pool[0].Schedulable)
		// A legacy pending 401 recorded at reclaim is silenced, but the queued
		// notification for a rejection before reclaim is still delivered.
		appendRisk401Event(r, a.ID, a.Name, cutoff, accountRiskBusiness401, "gateway", "legacy")
		require.NoError(t, s.deliverNotification(ctx, cutoff))
		require.NoError(t, s.deliverNotification(ctx, cutoff.Add(time.Minute)))
		require.Len(t, notifier.messages, 1)
		require.Equal(t, "cancelled", r.Events[0].Notification)
		require.Equal(t, "sent", r.Events[1].Notification)
		// The cutoff is specific to 401 for business errors.
		gw.observeRiskHTTP(ctx, &a, &http.Response{StatusCode: 503})
		gw.observeRiskHTTP(ctx, &a, &http.Response{StatusCode: 503})
		require.Equal(t, []int64{a.ID}, store.stopped)
	})
}

func TestAccountRiskSupplierReclaimCapacityAndRetry(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	job := riskCapacityDueJob(t, s, now)
	require.NotEmpty(t, job.pauseToken)
	cutoff := now.Truncate(time.Second)
	accounts.pool[0].Extra[AccountSourceExpiryExtraKey] = cutoff.Unix()
	accounts.pool[0].Extra[AccountSourceExpiryOriginExtraKey] = "supplier_api"
	s.runProbe(context.Background(), job)
	require.Zero(t, s.probe.(*riskTestProbe).calls)
	require.Empty(t, store.state.ActiveProbes)
	next, err := s.tick(context.Background(), now.Add(time.Hour))
	require.NoError(t, err)
	require.Empty(t, next)
	require.Empty(t, store.resumed)

	s, store, accounts, _, now = riskHarness()
	r := store.state.Rules[7]
	r.Config.Probe429StopEnabled = true
	job = riskDueJob(t, s, accounts, now)
	retry := &riskProbeSequence{statuses: []int{429}, after: func(_ int) { accounts.pool[0].Extra = riskLeaseExtra(now.Truncate(time.Second)) }}
	s.probe = retry
	s.runProbe(context.Background(), job)
	require.Equal(t, 1, retry.calls)
	require.Empty(t, r.Events)
}
