//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type riskProbeSequence struct {
	statuses []int
	calls    int
	after    func(int)
}

func (p *riskProbeSequence) RunRiskProbe(context.Context, *Account, AccountRiskConfig) (AccountRiskProbeResult, error) {
	status := p.statuses[min(p.calls, len(p.statuses)-1)]
	p.calls++
	if p.after != nil {
		p.after(p.calls)
	}
	if status == http.StatusOK {
		return AccountRiskProbeResult{Completed: true, Text: "21", UpstreamStatus: status}, nil
	}
	return AccountRiskProbeResult{UpstreamStatus: status}, errors.New("upstream unavailable (possibly 429)")
}

func TestAccountRiskProbe429StopsAfterConfiguredImmediateRetries(t *testing.T) {
	for _, retries := range []int{1, 3, 5, 10, 20} {
		synctest.Test(t, func(t *testing.T) {
			s, store, accounts, loads, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.Probe429StopEnabled, r.Config.Probe429MaxRetries = true, retries
			r.Config.ProbeNotifyOnly = true      // This switch applies to reply mismatches.
			r.Config.RateLimit429Enabled = false // Ordinary rolling counts are independent.
			r.Config.NotifyEnabled = true
			job := riskDueJob(t, s, accounts, now)
			probe := &riskProbeSequence{statuses: []int{429}}
			s.probe = probe
			started := time.Now()
			s.runProbe(context.Background(), job)
			require.Equal(t, retries+1, probe.calls, "first request is not one of the retries")
			require.Equal(t, started, time.Now(), "429 retries must not sleep or back off")
			require.Equal(t, []int64{1}, store.stopped)
			require.False(t, accounts.pool[0].Schedulable)
			require.Empty(t, store.state.ActiveProbes)
			require.Zero(t, loads.slots)
			require.Equal(t, "single_probe_429", r.Events[0].Kind)
			require.Equal(t, "stop", r.Events[0].Action)
			require.Equal(t, "pending", r.Events[0].Notification)
			require.Contains(t, r.Events[0].Reason, "首次收到上游 429 后立即重试")
		})
	}
}

func TestAccountRiskProbe429DoesNotStopOnOtherOutcomes(t *testing.T) {
	for _, scenario := range []string{"success", "server error", "network error", "disabled", "local busy", "cancelled", "config changed", "identity changed", "new member", "final identity changed", "CAS failed"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, loads, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.Probe429StopEnabled, r.Config.Probe429MaxRetries = true, 3
			if scenario == "disabled" {
				r.Config.Probe429StopEnabled = false
			}
			job := riskDueJob(t, s, accounts, now)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probe := &riskProbeSequence{statuses: []int{429}}
			wantCalls := 1
			switch scenario {
			case "success":
				probe.statuses, wantCalls = []int{429, 429, 200}, 3
			case "server error":
				probe.statuses, wantCalls = []int{429, 500}, 2
			case "network error":
				probe.statuses, wantCalls = []int{429, 0}, 2
			case "local busy":
				loads.busy, wantCalls = true, 0
			case "cancelled":
				probe.after = func(int) { cancel() }
			case "config changed":
				probe.after = func(int) { r.Revision++ }
			case "identity changed":
				probe.after = func(int) { accounts.pool[0].Credentials = map[string]any{"access_token": "changed"} }
			case "new member":
				probe.after = func(int) { other := accounts.pool[0]; other.ID = 2; accounts.pool = append(accounts.pool, other) }
			case "final identity changed":
				wantCalls = 4
				probe.after = func(calls int) {
					if calls == 4 {
						accounts.pool[0].Credentials = map[string]any{"access_token": "changed"}
					}
				}
			case "CAS failed":
				wantCalls, store.cas = 4, false
			}
			s.probe = probe
			s.runProbe(ctx, job)
			require.Equal(t, wantCalls, probe.calls)
			require.Empty(t, store.stopped)
			require.Empty(t, r.Events)
			require.Empty(t, store.state.ActiveProbes)
		})
	}
}

func TestAccountRiskProbe429ResultSaveRetryDoesNotSendAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, accounts, _, now := riskHarness()
		store.state.Rules[7].Config.Probe429StopEnabled = true
		job := riskDueJob(t, s, accounts, now)
		probe := &riskProbeSequence{statuses: []int{429}, after: func(calls int) {
			if calls == 4 {
				store.busy = 2
			}
		}}
		s.probe = probe
		s.runProbe(context.Background(), job)
		require.Equal(t, 4, probe.calls)
		require.Equal(t, []int64{1}, store.stopped)
		require.Len(t, store.state.Rules[7].Events, 1)
	})
}

func TestAccountRiskProbe429LeavesManualAndCapacityProbesUnchanged(t *testing.T) {
	for _, manual := range []bool{false, true} {
		s, store, _, _, now := riskHarness()
		r := store.state.Rules[7]
		r.Config.Probe429StopEnabled, r.Config.CapacityNotifyOnly = true, true
		probe := &riskProbeSequence{statuses: []int{429}}
		s.probe = probe
		if manual {
			out, err := s.TestNow(context.Background(), 7, 1, 1, r.Config)
			require.NoError(t, err)
			require.False(t, out.Completed)
		} else {
			job := riskCapacityDueJob(t, s, now)
			s.runProbe(context.Background(), job)
		}
		require.Equal(t, 1, probe.calls)
		require.Empty(t, store.stopped)
	}
}

func TestAccountRiskProbe429TimeoutDoesNotCountAsRejection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, accounts, _, now := riskHarness()
		store.state.Rules[7].Config.Probe429StopEnabled = true
		job := riskDueJob(t, s, accounts, now)
		probe := &riskProbeSequence{statuses: []int{429}, after: func(int) { time.Sleep(91 * time.Second) }}
		s.probe = probe
		s.runProbe(context.Background(), job)
		require.Equal(t, 1, probe.calls)
		require.Empty(t, store.stopped)
	})
}

func TestAccountRiskProbe429ConfigValidation(t *testing.T) {
	for _, n := range []int{-1, 0, 21} {
		c := DefaultAccountRiskConfig()
		c.Probe429StopEnabled, c.Probe429MaxRetries = true, n
		require.Error(t, c.Validate())
	}
	for _, n := range []int{1, 5, 10, 20} {
		c := DefaultAccountRiskConfig()
		c.Probe429StopEnabled, c.Probe429MaxRetries = true, n
		require.NoError(t, c.Validate())
	}
}
