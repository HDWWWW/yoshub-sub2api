//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type riskMismatchStep struct {
	text       string
	status     int
	incomplete bool
	delay      time.Duration
}

type riskMismatchProbe struct {
	steps []riskMismatchStep
	calls int
	after func(int)
}

func (p *riskMismatchProbe) RunRiskProbe(ctx context.Context, _ *Account, _ AccountRiskConfig) (AccountRiskProbeResult, error) {
	step := p.steps[min(p.calls, len(p.steps)-1)]
	p.calls++
	if step.delay > 0 {
		time.Sleep(step.delay)
	}
	if p.after != nil {
		p.after(p.calls)
	}
	if ctx.Err() != nil {
		return AccountRiskProbeResult{}, ctx.Err()
	}
	if step.status != 200 {
		return AccountRiskProbeResult{UpstreamStatus: step.status}, errors.New("probe failed, possibly 429")
	}
	return AccountRiskProbeResult{Completed: !step.incomplete, Text: step.text, UpstreamStatus: 200}, nil
}

func TestAccountRiskMismatchImmediateRetries(t *testing.T) {
	wrong := riskMismatchStep{text: "wrong", status: 200}
	pass := riskMismatchStep{text: "21", status: 200}
	for _, scope := range []string{"single", "all"} {
		for _, tc := range []struct {
			name                               string
			retries                            int
			steps                              []riskMismatchStep
			notifyOnly, retry429, stop, notify bool
			calls                              int
		}{
			{name: "disabled", retries: 0, steps: []riskMismatchStep{wrong, pass}, stop: true, calls: 1},
			{name: "first passes", retries: 2, steps: []riskMismatchStep{pass}, calls: 1},
			{name: "retry passes", retries: 2, steps: []riskMismatchStep{wrong, pass}, calls: 2},
			{name: "last retry passes", retries: 2, steps: []riskMismatchStep{wrong, wrong, pass}, calls: 3},
			{name: "all mismatch", retries: 2, steps: []riskMismatchStep{wrong}, stop: true, calls: 3},
			{name: "max retries", retries: 20, steps: []riskMismatchStep{wrong}, stop: true, calls: 21},
			{name: "empty reply", retries: 2, steps: []riskMismatchStep{{status: 200}}, stop: true, calls: 3},
			{name: "notify only", retries: 2, steps: []riskMismatchStep{wrong}, notifyOnly: true, notify: true, calls: 3},
			{name: "429 after mismatch", retries: 2, steps: []riskMismatchStep{wrong, {status: 429}}, stop: true, calls: 2},
			{name: "429 overrides notify only", retries: 2, steps: []riskMismatchStep{wrong, {status: 429}}, notifyOnly: true, stop: true, calls: 2},
			{name: "429 no extra 429 retries", retries: 2, steps: []riskMismatchStep{wrong, {status: 429}, pass}, retry429: true, stop: true, calls: 2},
			{name: "initial 429 retains disabled policy", retries: 2, steps: []riskMismatchStep{{status: 429}, wrong}, calls: 1},
			{name: "initial 429 retains enabled policy", retries: 2, steps: []riskMismatchStep{{status: 429}}, retry429: true, stop: true, calls: 4},
			{name: "429 then mismatch then pass", retries: 2, steps: []riskMismatchStep{{status: 429}, wrong, pass}, retry429: true, calls: 3},
			{name: "network ends cycle", retries: 2, steps: []riskMismatchStep{wrong, {status: 0}, pass}, calls: 2},
			{name: "500 ends cycle", retries: 2, steps: []riskMismatchStep{wrong, {status: 500}, pass}, calls: 2},
			{name: "incomplete ends cycle", retries: 2, steps: []riskMismatchStep{wrong, {status: 200, incomplete: true}, pass}, calls: 2},
		} {
			t.Run(scope+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s, store, accounts, loads, now := riskAllAccountsHarness()
					r := store.state.Rules[7]
					r.Config.ProbeScope = scope
					r.Config.ProbeMismatchMaxRetries = tc.retries
					r.Config.ProbeNotifyOnly = tc.notifyOnly
					r.Config.Probe429StopEnabled = tc.retry429
					r.Config.NotifyEnabled = true
					var job *accountRiskJob
					if scope == "all" {
						job = riskAllAccountsDueJobs(t, s, now)[0]
					} else {
						job = riskDueJob(t, s, accounts, now)
					}
					probe := &riskMismatchProbe{steps: tc.steps}
					s.probe = probe
					started := time.Now()
					s.runProbe(context.Background(), job)
					require.Equal(t, tc.calls, probe.calls)
					require.Equal(t, started, time.Now(), "no delay between retries")
					require.Equal(t, !tc.stop, accounts.pool[0].Schedulable)
					require.Zero(t, loads.slots)
					require.NotContains(t, store.state.ActiveProbes, int64(1))
					if tc.stop || tc.notify {
						require.Len(t, r.Events, 1)
						require.Equal(t, "pending", r.Events[0].Notification)
						if tc.notify {
							require.Equal(t, "notify_only", r.Events[0].Action)
						} else {
							require.Equal(t, "stop", r.Events[0].Action)
						}
						if tc.retries > 0 && tc.steps[min(probe.calls-1, len(tc.steps)-1)].status == 200 {
							require.Contains(t, r.Events[0].Reason, fmt.Sprintf("连续 %d 次均不匹配", tc.retries+1))
						}
					} else {
						require.Empty(t, r.Events)
					}
					if scope == "all" {
						require.True(t, accounts.pool[1].Schedulable)
						require.True(t, accounts.pool[2].Schedulable)
						require.Len(t, store.state.ActiveProbes, 2)
					}
				})
			})
		}
	}
}

func TestAccountRiskMismatchRetriesDiscardStaleResults(t *testing.T) {
	for _, scenario := range []string{"disabled", "probe disabled", "revision", "identity", "membership", "manual stop", "busy", "cancelled", "last identity", "last revision", "CAS"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, loads, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.ProbeMismatchMaxRetries = 2
			job := riskDueJob(t, s, accounts, now)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantCalls := 1
			if scenario == "last identity" || scenario == "last revision" || scenario == "CAS" {
				wantCalls = 3
			}
			probe := &riskMismatchProbe{steps: []riskMismatchStep{{text: "wrong", status: 200}}, after: func(n int) {
				if n != wantCalls {
					return
				}
				switch scenario {
				case "disabled":
					r.Config.Enabled = false
				case "probe disabled":
					r.Config.ProbeEnabled = false
				case "revision", "last revision":
					r.Revision++
				case "identity", "last identity":
					accounts.pool[0].Credentials = map[string]any{"access_token": "changed"}
				case "membership":
					accounts.pool[0].GroupIDs = nil
				case "manual stop":
					accounts.pool[0].Schedulable = false
				case "busy":
					loads.busy = true
				case "cancelled":
					cancel()
				case "CAS":
					store.cas = false
				}
			}}
			s.probe = probe
			s.runProbe(ctx, job)
			require.Equal(t, wantCalls, probe.calls)
			require.Empty(t, store.stopped)
			require.Empty(t, r.Events)
			require.Empty(t, store.state.ActiveProbes)
		})
	}
}

func TestAccountRiskMismatchRetriesHaveIndependentTimeoutsAndLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, _, loads, now := riskAllAccountsHarness()
		r := store.state.Rules[7]
		r.Config.ProbeMismatchMaxRetries = 2
		job := riskAllAccountsDueJobs(t, s, now)[0]
		probe := &riskMismatchProbe{steps: []riskMismatchStep{{text: "wrong", status: 200, delay: 60 * time.Second}, {text: "wrong", status: 200, delay: 60 * time.Second}, {text: "21", status: 200, delay: 60 * time.Second}}, after: func(int) {
			expireRiskProbes(store.state, time.Now())
			require.Contains(t, store.state.ActiveProbes, int64(1), "lease must survive the old 120-second limit")
			require.True(t, riskAccountProbeBusy(store.state, 1))
			require.Equal(t, 1, loads.slots)
		}}
		s.probe = probe
		started := time.Now()
		s.runProbe(context.Background(), job)
		require.Equal(t, 180*time.Second, time.Since(started))
		require.Equal(t, 3, probe.calls)
		require.Empty(t, store.stopped)
		require.Zero(t, loads.slots)
		require.Contains(t, r.Observation.ScheduledChecks[1].LastProbeResult, "检测通过")
		require.True(t, time.Now().Add(10*time.Second).Equal(r.Observation.ScheduledChecks[1].NextProbeAt))
	})
}

func TestAccountRiskMismatchRetryTimeoutDoesNotStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, accounts, _, now := riskHarness()
		store.state.Rules[7].Config.ProbeMismatchMaxRetries = 2
		job := riskDueJob(t, s, accounts, now)
		probe := &riskMismatchProbe{steps: []riskMismatchStep{{text: "wrong", status: 200}, {status: 429, delay: 91 * time.Second}}}
		s.probe = probe
		s.runProbe(context.Background(), job)
		require.Equal(t, 2, probe.calls)
		require.Empty(t, store.stopped)
		require.Empty(t, store.state.Rules[7].Events)
	})
}

func TestAccountRiskMismatchLeavesManualAndCapacityUnchanged(t *testing.T) {
	for _, manual := range []bool{true, false} {
		s, store, _, _, now := riskHarness()
		r := store.state.Rules[7]
		r.Config.ProbeMismatchMaxRetries = 2
		probe := &riskMismatchProbe{steps: []riskMismatchStep{{text: "wrong", status: 200}}}
		s.probe = probe
		if manual {
			out, err := s.TestNow(context.Background(), 7, 1, 1, r.Config)
			require.NoError(t, err)
			require.False(t, *out.Matched)
		} else {
			job := riskCapacityDueJob(t, s, now)
			s.runProbe(context.Background(), job)
		}
		require.Equal(t, 1, probe.calls)
	}
}

func TestAccountRiskMismatchRetryConfig(t *testing.T) {
	for _, n := range []int{-1, 21} {
		c := DefaultAccountRiskConfig()
		c.ProbeMismatchMaxRetries = n
		require.Error(t, c.Validate())
	}
	for _, n := range []int{0, 1, 2, 20} {
		c := DefaultAccountRiskConfig()
		c.ProbeMismatchMaxRetries = n
		require.NoError(t, c.Validate())
		raw, err := json.Marshal(c)
		require.NoError(t, err)
		var decoded AccountRiskConfig
		require.NoError(t, json.Unmarshal(raw, &decoded))
		require.Equal(t, n, decoded.ProbeMismatchMaxRetries)
	}
	var old AccountRiskConfig
	require.NoError(t, json.Unmarshal([]byte(`{"probe_enabled":true}`), &old))
	require.Zero(t, old.ProbeMismatchMaxRetries)
}
