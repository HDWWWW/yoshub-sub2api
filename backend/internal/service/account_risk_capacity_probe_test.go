//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func riskCapacityDueJob(t *testing.T, s *AccountRiskService, now time.Time) *accountRiskJob {
	t.Helper()
	state, err := s.store.Read(context.Background())
	require.NoError(t, err)
	var job *accountRiskJob
	for sec := 0; sec <= state.Rules[7].Config.SustainSeconds; sec++ {
		job, err = s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
		require.NoError(t, err)
		if sec < state.Rules[7].Config.SustainSeconds {
			require.Nil(t, job)
		}
	}
	require.NotNil(t, job)
	require.Equal(t, accountRiskProbeCapacity, job.pending.Kind)
	return job
}

func TestAccountRiskCapacityProbeUsesConfiguredMessageAndGuards(t *testing.T) {
	for _, scenario := range []string{"pass", "mismatch", "network", "incomplete", "busy", "credentials changed", "pair changed", "disabled", "reconfigured"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, loads, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.Model, r.Config.ReasoningEffort = "gpt-6-astra", "medium"
			r.Config.ProbePrompt, r.Config.ExpectedText = "custom operator message", "EXPECTED"
			job := riskCapacityDueJob(t, s, now)
			require.Empty(t, store.stopped)
			probe := s.probe.(*riskTestProbe)
			probe.result = AccountRiskProbeResult{Completed: true, Text: "private wrong answer"}
			switch scenario {
			case "pass":
				probe.result.Text = "answer EXPECTED"
			case "network":
				probe.err = errors.New("upstream failure")
			case "incomplete":
				probe.result.Completed = false
			case "busy":
				loads.busy = true
			case "credentials changed":
				accounts.pool[0].Credentials = map[string]any{"access_token": "updated"}
			case "pair changed":
				accounts.pool[1].ID = 9
			case "disabled":
				r.Config.Enabled = false
			case "reconfigured":
				r.Revision++
			}
			s.runProbe(context.Background(), job)
			require.Nil(t, store.state.Pending)
			if scenario == "pass" || scenario == "mismatch" || scenario == "network" || scenario == "incomplete" {
				require.Equal(t, 1, probe.calls)
				require.Equal(t, int64(1), probe.lastAccountID)
				require.Equal(t, job.config, probe.lastConfig, "capacity probes reuse all configured test parameters")
				require.Zero(t, loads.slots)
			} else {
				require.Zero(t, probe.calls)
			}
			if scenario == "mismatch" {
				require.Equal(t, []int64{1}, store.stopped)
				require.Equal(t, accountRiskProbeCapacity, r.Events[0].Kind)
				require.Contains(t, r.Events[0].Reason, "59.0%")
				require.NotContains(t, r.Events[0].Reason, probe.result.Text)
				require.NotContains(t, r.Events[0].Reason, job.config.ProbePrompt)
			} else {
				require.Empty(t, store.stopped)
				require.Empty(t, r.Events)
			}
		})
	}
}

func TestAccountRiskProbeNotifyOnlyActionsAreIndependent(t *testing.T) {
	for _, capacity := range []bool{false, true} {
		for _, notifyOnly := range []bool{false, true} {
			for _, stopNotice := range []bool{false, true} {
				s, store, accounts, _, now := riskHarness()
				r := store.state.Rules[7]
				r.Config.NotifyEnabled = stopNotice
				r.Config.CapacityNotifyOnly, r.Config.ProbeNotifyOnly = !notifyOnly, notifyOnly
				var job *accountRiskJob
				kind := accountRiskProbeSingle
				if capacity {
					r.Config.CapacityNotifyOnly, r.Config.ProbeNotifyOnly = notifyOnly, !notifyOnly
					kind = accountRiskProbeCapacity
					job = riskCapacityDueJob(t, s, now)
				} else {
					job = riskDueJob(t, s, accounts, now)
				}
				n := &riskNoticeStub{}
				s.notifier = n
				require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(301*time.Second)))
				require.Len(t, r.Events, 1)
				require.Equal(t, kind, r.Events[0].Kind)
				if notifyOnly {
					require.Empty(t, store.stopped)
					require.Equal(t, "notify_only", r.Events[0].Action)
				} else {
					require.Equal(t, []int64{1}, store.stopped)
					require.Equal(t, "stop", r.Events[0].Action)
				}
				require.NoError(t, s.deliverNotification(context.Background(), now.Add(302*time.Second)))
				if notifyOnly || stopNotice {
					require.Len(t, n.messages, 1)
					if notifyOnly {
						require.Contains(t, n.messages[0], "仅通知")
						require.NotContains(t, n.messages[0], "已关闭此账号调度")
					}
				} else {
					require.Empty(t, n.messages)
				}
			}
		}
	}
}

func TestAccountRiskCapacityProbeCooldownSurvivesRecovery(t *testing.T) {
	s, store, _, loads, now := riskHarness()
	r := store.state.Rules[7]
	r.Config.SustainSeconds, r.Config.ProbeIntervalSeconds = 2, 10
	r.Config.CapacityNotifyOnly = true
	job := riskCapacityDueJob(t, s, now)
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(3*time.Second)))
	require.Equal(t, now.Add(13*time.Second), *r.Observation.NextProbeAt)
	for sec := 4; sec < 13; sec++ {
		loads.first = 59
		if sec == 4 {
			loads.first = 100
		}
		next, err := s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
		require.NoError(t, err)
		require.Nil(t, next, "must not test every second or reset the interval after a recovered sample")
	}
	next, err := s.tick(context.Background(), now.Add(13*time.Second))
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Empty(t, store.stopped)
	require.Len(t, r.Events, 1)
}

func TestAccountRiskCapacityPassingProbeDiscardsChangedAccount(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	job := riskCapacityDueJob(t, s, now)
	accounts.pool[0].Credentials = map[string]any{"access_token": "replaced-after-dispatch"}
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: job.config.ExpectedText}, nil, now.Add(12*time.Second)))
	require.Equal(t, "账号或分组已变化，本次结果作废", store.state.Rules[7].Observation.LastProbeResult)
	require.Empty(t, store.stopped)
	require.Empty(t, store.state.Rules[7].Events)
}

func TestAccountRiskCapacityProbeReservationAndStaleResults(t *testing.T) {
	for _, change := range []string{"pair", "priority", "revision", "disabled", "CAS"} {
		t.Run(change, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			job := riskCapacityDueJob(t, s, now)
			s2 := &AccountRiskService{store: store, accounts: s.accounts, groups: s.groups, loads: s.loads, cfg: s.cfg, worker: "second"}
			next, err := s2.tick(context.Background(), now.Add(11*time.Second))
			require.NoError(t, err)
			require.Nil(t, next, "global reservation covers both probe kinds across instances")
			switch change {
			case "pair":
				accounts.pool = accounts.pool[:1]
			case "priority":
				accounts.pool[1].Priority = 0
			case "revision":
				store.state.Rules[7].Revision++
			case "disabled":
				store.state.Rules[7].Config.Enabled = false
			case "CAS":
				store.cas = false
			}
			require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(12*time.Second)))
			require.Empty(t, store.stopped)
			require.Empty(t, store.state.Rules[7].Events)
			require.Nil(t, store.state.Pending)
		})
	}
}

func TestAccountRiskProbeNotificationCancelledWhenOwnSwitchDisabled(t *testing.T) {
	for _, capacity := range []bool{false, true} {
		s, store, accounts, _, now := riskHarness()
		r := store.state.Rules[7]
		r.Config.CapacityNotifyOnly, r.Config.ProbeNotifyOnly = true, true
		var job *accountRiskJob
		if capacity {
			job = riskCapacityDueJob(t, s, now)
		} else {
			job = riskDueJob(t, s, accounts, now)
		}
		require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(301*time.Second)))
		if capacity {
			r.Config.CapacityNotifyOnly = false
		} else {
			r.Config.ProbeNotifyOnly = false
		}
		n := &riskNoticeStub{}
		s.notifier = n
		require.NoError(t, s.deliverNotification(context.Background(), now.Add(302*time.Second)))
		require.Empty(t, n.messages)
		require.Equal(t, "cancelled", r.Events[0].Notification)
		require.Empty(t, store.stopped)
	}
}
