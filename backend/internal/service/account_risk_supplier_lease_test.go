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
				require.Len(t, r.Events, 1)
				if offset < 0 {
					require.Equal(t, "pending", r.Events[0].Notification)
				} else {
					require.Equal(t, "disabled", r.Events[0].Notification)
					require.Contains(t, r.Events[0].Reason, risk401ReclaimNote)
					require.Nil(t, accounts.pool[0].TempUnschedulableUntil)
					require.Contains(t, r.Observation.LastProbeResult, "回收")
				}
			})
		}
	}
}

func TestAccountRiskSupplierReclaimQueuedProbeKeepsReceiptTime(t *testing.T) {
	for _, stop := range []bool{false, true} {
		s, store, accounts, _, now := riskHarness()
		r := store.state.Rules[7]
		r.Config.Probe401Enabled, r.Config.Notify401Enabled = stop, true
		job := riskDueJob(t, s, accounts, now)
		cutoff := now.Truncate(time.Second).Add(310 * time.Second)
		accounts.pool[0].Extra = riskLeaseExtra(cutoff)
		e := readRiskProbe401(&job.account, r.Config.Model, strings.NewReader(`{"detail":"Unauthorized"}`))
		require.NotNil(t, e)
		e.receivedAt = cutoff.Add(-time.Nanosecond)
		ctx := context.Background()
		require.NoError(t, s.finishProbe(ctx, job, AccountRiskProbeResult{UpstreamStatus: 401, auth401: e}, errors.New("rejected"), cutoff))
		require.Len(t, r.Events, 1)
		require.Equal(t, e.receivedAt, r.Events[0].At)
		notifier := &riskNoticeStub{}
		s.notifier = notifier
		require.NoError(t, s.deliverNotification(ctx, cutoff.Add(time.Minute)))
		require.Len(t, notifier.messages, 1)
		require.True(t, accounts.pool[0].Schedulable)
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
		s.reporter401().observe(ctx, r.GroupID, &a, 401, "用量探测")
		ObserveAccountHTTPStatus(s.reporter401().withScope(ctx, a.ID, "账号凭据刷新"), 401)
		require.Len(t, r.Events, 6)
		for _, event := range r.Events[:5] {
			require.Equal(t, "disabled", event.Notification)
			require.Contains(t, event.Reason, risk401ReclaimNote)
		}
		require.Equal(t, "pending", r.Events[5].Notification)
		require.True(t, accounts.pool[0].Schedulable)
		require.NoError(t, s.deliverNotification(ctx, cutoff))
		require.NoError(t, s.deliverNotification(ctx, cutoff.Add(time.Minute)))
		require.Len(t, notifier.messages, 1)
		require.Equal(t, "sent", r.Events[5].Notification)
		// The cutoff is specific to 401 for business errors.
		gw.observeRiskHTTP(ctx, &a, &http.Response{StatusCode: 503})
		gw.observeRiskHTTP(ctx, &a, &http.Response{StatusCode: 503})
		require.Equal(t, []int64{a.ID}, store.stopped)
	})
}

// A queued/retried result can outlive the reservation and a supplier sync. Keep
// the original receipt time and the single audit event across both finishes.
func TestAccountRiskLate401ReclaimSync(t *testing.T) {
	for _, before := range []bool{true, false} {
		s, store, accounts, _, now := riskHarness()
		r := store.state.Rules[7]
		r.Config.Notify401Enabled = true
		job := riskDueJob(t, s, accounts, now)
		cutoff := now.Add(310 * time.Second).Truncate(time.Second)
		at := cutoff
		if before {
			at = at.Add(-time.Nanosecond)
		}
		ctx := context.Background()
		e := &riskProbe401Evidence{ownerID: job.account.ID, receivedAt: at}
		e.captureAlertTarget(ctx, s.reporter401(), job.account.ID)
		require.Nil(t, e.alertTarget.ReclaimAt)
		accounts.pool[0].Extra = riskLeaseExtra(cutoff)
		r.Revision++
		result := AccountRiskProbeResult{UpstreamStatus: 401, auth401: e}
		for range 2 {
			require.NoError(t, s.finishProbe(ctx, job, result, errors.New("rejected"), cutoff.Add(time.Minute)))
		}
		require.Len(t, r.Events, 1)
		require.Equal(t, at, r.Events[0].At)
		if before {
			require.Equal(t, "pending", r.Events[0].Notification)
		} else {
			require.Equal(t, "disabled", r.Events[0].Notification)
			require.Contains(t, r.Events[0].Reason, risk401ReclaimNote)
		}
		require.Empty(t, store.stopped)
	}
}

// ID-only usage/refresh transports must also honor the upstream owner's lease.
// Local retention, an unknown cutoff and a temporary OAuth cooldown are separate.
func TestAccountRiskMaintenance401SourceAndOwnerLease(t *testing.T) {
	for _, lease := range []string{"parent", "source", "legacy", "pending", "local only"} {
		t.Run(lease, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.Notify401Enabled = true
			source, owner := &accounts.pool[0], &accounts.pool[1]
			source.ParentAccountID = &owner.ID
			past, future := now.Add(-time.Minute), now.Add(time.Hour)
			source.TempUnschedulableUntil = &future
			source.ExpiresAt, owner.ExpiresAt = &past, &past
			switch lease {
			case "parent":
				source.Extra, owner.Extra = riskLeaseExtra(future), riskLeaseExtra(past)
			case "source":
				source.Extra, owner.Extra = riskLeaseExtra(past), riskLeaseExtra(future)
			case "legacy":
				owner.Extra = map[string]any{"import_original_expires_at": past.Unix()}
			case "pending":
				owner.Extra = map[string]any{AccountSourceExpiryOriginExtraKey: "supplier_pending", AccountSourceExpiryExtraKey: past.Unix()}
			}
			reporter := s.reporter401()
			ctx := reporter.withScope(context.Background(), source.ID, "用量查询")
			ObserveAccountHTTPStatus(ctx, 401)
			reporter.observe(ctx, 7, source, 401, "用量探测")
			require.Len(t, r.Events, 2)
			for i, event := range r.Events {
				require.Equal(t, source.ID, event.AccountID)
				if lease == "pending" || lease == "local only" {
					require.Equal(t, []string{"merged", "pending"}[i], event.Notification)
				} else {
					require.Equal(t, "disabled", event.Notification)
					require.Contains(t, event.Reason, risk401ReclaimNote)
				}
			}
		})
	}
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
