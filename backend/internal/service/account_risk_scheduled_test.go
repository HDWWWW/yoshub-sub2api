//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func riskAllAccountsHarness() (*AccountRiskService, *riskTestStore, *riskTestAccounts, *riskTestLoads, time.Time) {
	s, store, accounts, loads, now := riskHarness()
	r := store.state.Rules[7]
	r.Config.ProbeScope, r.Config.ProbeIntervalSeconds = "all", 10
	third := accounts.pool[1]
	third.ID, third.Priority = 3, 3
	accounts.pool = append(accounts.pool, third)
	loads.first, loads.second = 0, 0
	return s, store, accounts, loads, now
}

func riskAllAccountsDueJob(t *testing.T, s *AccountRiskService, now time.Time) *accountRiskJob {
	t.Helper()
	job, err := s.tick(context.Background(), now)
	require.NoError(t, err)
	require.Nil(t, job, "wait a full interval after activation")
	job, err = s.tick(context.Background(), now.Add(10*time.Second))
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, accountRiskProbeAll, job.pending.Kind)
	return job
}

func TestAccountRiskAllAccountsIndependentResultsAndTimers(t *testing.T) {
	for _, scenario := range []string{"pass", "mismatch", "empty reply", "notify only", "network", "incomplete", "429 exhausted", "429 incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskAllAccountsHarness()
			r := store.state.Rules[7]
			r.Config.Probe429StopEnabled, r.Config.NotifyEnabled = true, true
			job := riskAllAccountsDueJob(t, s, now)
			require.Equal(t, int64(1), job.account.ID)
			result := AccountRiskProbeResult{Completed: true, Text: "21"}
			var probeErr error
			stop := false
			switch scenario {
			case "mismatch", "empty reply":
				result.Text, stop = "", true
				if scenario == "mismatch" {
					result.Text = "wrong"
				}
			case "notify only":
				result.Text, r.Config.ProbeNotifyOnly = "wrong", true
			case "network":
				probeErr = errors.New("network unavailable")
			case "incomplete":
				result.Completed = false
			case "429 exhausted", "429 incomplete":
				result = AccountRiskProbeResult{UpstreamStatus: 429, Consecutive429: 4}
				probeErr, stop = errors.New("upstream 429"), true
				if scenario == "429 incomplete" {
					result.Consecutive429, stop = 3, false
				}
			}
			require.NoError(t, s.finishProbe(context.Background(), job, result, probeErr, now.Add(11*time.Second)))
			require.Equal(t, !stop, accounts.pool[0].Schedulable)
			require.True(t, accounts.pool[1].Schedulable)
			require.True(t, accounts.pool[2].Schedulable)
			require.Equal(t, now.Add(10*time.Second), r.Observation.ScheduledChecks[2].NextProbeAt)
			require.Equal(t, now.Add(21*time.Second), r.Observation.ScheduledChecks[1].NextProbeAt)
			require.NotEmpty(t, r.Observation.ScheduledChecks[1].LastProbeResult)
			if stop {
				require.Equal(t, []int64{1}, store.stopped)
				require.Equal(t, int64(1), r.Events[0].AccountID)
			} else {
				require.Empty(t, store.stopped)
			}
			if scenario == "notify only" {
				require.Equal(t, accountRiskProbeAll, r.Events[0].Kind)
				require.Equal(t, "notify_only", r.Events[0].Action)
				require.True(t, riskEventNotificationEnabled(r.Config, r.Events[0]))
			}
			// The store serializes on every transaction, and a new instance must
			// pick up the next account without restarting the whole group's timer.
			raw, err := json.Marshal(store.state)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &store.state))
			s.worker = "replacement"
			second, err := s.tick(context.Background(), now.Add(12*time.Second))
			require.NoError(t, err)
			require.NotNil(t, second)
			require.Equal(t, int64(2), second.account.ID)
			require.NoError(t, s.finishProbe(context.Background(), second, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(13*time.Second)))
			third, err := s.tick(context.Background(), now.Add(14*time.Second))
			require.NoError(t, err)
			require.NotNil(t, third)
			require.Equal(t, int64(3), third.account.ID, "continue even when only one account remains")
		})
	}
}

func TestAccountRiskAllAccountsRetriesDoNotCarryAcrossAccounts(t *testing.T) {
	s, store, accounts, loads, now := riskAllAccountsHarness()
	r := store.state.Rules[7]
	r.Config.Probe429StopEnabled, r.Config.ProbeNotifyOnly = true, true
	job := riskAllAccountsDueJob(t, s, now)
	probe := &riskProbeSequence{statuses: []int{429, 429, 429, 429, 429, 200}}
	s.probe = probe
	s.runProbe(context.Background(), job)
	require.Equal(t, 4, probe.calls)
	require.Equal(t, []int64{1}, store.stopped)
	require.Equal(t, "all_probe_429", r.Events[0].Kind)
	require.True(t, accounts.pool[1].Schedulable)
	second, err := s.tick(context.Background(), now.Add(11*time.Second))
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, int64(2), second.account.ID)
	s.runProbe(context.Background(), second)
	require.Equal(t, 6, probe.calls)
	require.Equal(t, []int64{1}, store.stopped)
	require.True(t, accounts.pool[1].Schedulable)
	require.Zero(t, loads.slots)
	require.Nil(t, store.state.Pending)
}

func TestAccountRiskAllAccountsInterruptedProbeContinuesWithNextAccount(t *testing.T) {
	s, store, _, _, now := riskAllAccountsHarness()
	first := riskAllAccountsDueJob(t, s, now)
	other := *s
	other.worker = "other instance"
	job, err := other.tick(context.Background(), now.Add(11*time.Second))
	require.NoError(t, err)
	require.Nil(t, job, "only one probe across instances")
	job, err = other.tick(context.Background(), now.Add(130*time.Second))
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, int64(2), job.account.ID)
	require.Equal(t, now.Add(140*time.Second), store.state.Rules[7].Observation.ScheduledChecks[1].NextProbeAt)
	require.NoError(t, s.finishProbe(context.Background(), first, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(131*time.Second)))
	require.Equal(t, job.pending.ID, store.state.Pending.ID)
	require.Empty(t, store.stopped)
}

func TestAccountRiskAllAccountsIdentityChangesAreIsolated(t *testing.T) {
	s, store, accounts, _, now := riskAllAccountsHarness()
	_, err := s.tick(context.Background(), now)
	require.NoError(t, err)
	accounts.pool[0].Credentials = map[string]any{"access_token": "new"}
	fourth := accounts.pool[2]
	fourth.ID = 4
	accounts.pool = append(accounts.pool, fourth)
	job, err := s.tick(context.Background(), now.Add(5*time.Second))
	require.NoError(t, err)
	require.Nil(t, job)
	checks := store.state.Rules[7].Observation.ScheduledChecks
	require.Equal(t, now.Add(15*time.Second), checks[1].NextProbeAt)
	require.Equal(t, now.Add(15*time.Second), checks[4].NextProbeAt)
	require.Equal(t, now.Add(10*time.Second), checks[2].NextProbeAt)
	job, err = s.tick(context.Background(), now.Add(10*time.Second))
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, int64(2), job.account.ID)
	accounts.pool[1].Credentials = map[string]any{"access_token": "changed in flight"}
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(11*time.Second)))
	require.Empty(t, store.stopped)
	job, err = s.tick(context.Background(), now.Add(12*time.Second))
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, int64(3), job.account.ID)
}

func TestAccountRiskAllAccountsPeerExpiryRankDoesNotResetIdentity(t *testing.T) {
	a := Account{ID: 1, Priority: 10, schedulingPriority: 1}
	sig := riskScheduledAccountSignature(&a)
	a.schedulingPriority = 2
	require.Equal(t, sig, riskScheduledAccountSignature(&a))
	a.Priority = 20
	require.NotEqual(t, sig, riskScheduledAccountSignature(&a))
}

func TestAccountRiskScheduledSwitchDoesNotDisableCapacityVerification(t *testing.T) {
	for _, scope := range []string{"single", "all"} {
		t.Run(scope, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			r := store.state.Rules[7]
			r.Config.ProbeEnabled, r.Config.ProbeScope = false, scope
			job := riskCapacityDueJob(t, s, now)
			require.Equal(t, accountRiskProbeCapacity, job.pending.Kind)
			require.Equal(t, []int64{1}, store.stopped)
			require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(11*time.Second)))
			require.True(t, accounts.pool[0].Schedulable)
			accounts.pool = accounts.pool[:1]
			for _, sec := range []int{12, 500} {
				job, err := s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
				require.NoError(t, err)
				require.Nil(t, job)
			}
		})
	}
}

func TestAccountRiskScheduledConfigurationChangesDiscardInflightResults(t *testing.T) {
	for _, change := range []string{"disable", "scope", "global disable"} {
		t.Run(change, func(t *testing.T) {
			s, store, accounts, _, now := riskAllAccountsHarness()
			job := riskAllAccountsDueJob(t, s, now)
			r := store.state.Rules[7]
			c := r.Config
			switch change {
			case "disable":
				c.ProbeEnabled = false
			case "scope":
				c.ProbeScope = "single"
			case "global disable":
				c.Enabled = false
			}
			require.NoError(t, s.Configure(context.Background(), 7, r.Revision, c))
			require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(11*time.Second)))
			require.Empty(t, store.stopped)
			require.True(t, accounts.pool[0].Schedulable)
			job, err := s.tick(context.Background(), now.Add(22*time.Second))
			require.NoError(t, err)
			require.Nil(t, job)
		})
	}
}

func TestAccountRiskAllAccountsCapacityPausesKeepTheirOwnProbe(t *testing.T) {
	s, store, accounts, loads, now := riskAllAccountsHarness()
	loads.first, loads.second = 59, 90
	for sec := 0; sec <= 10; sec++ {
		job, err := s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
		require.NoError(t, err)
		if sec < 10 {
			require.Nil(t, job)
			continue
		}
		require.NotNil(t, job)
		require.Equal(t, accountRiskProbeCapacity, job.pending.Kind)
		require.Equal(t, int64(1), job.account.ID)
		require.NotEmpty(t, job.pauseToken)
		require.NotContains(t, store.state.Rules[7].Observation.ScheduledChecks, int64(1))
		require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(11*time.Second)))
	}
	require.True(t, accounts.pool[0].Schedulable)
	job, err := s.tick(context.Background(), now.Add(12*time.Second))
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, accountRiskProbeAll, job.pending.Kind)
	require.Equal(t, int64(2), job.account.ID)
}

func TestAccountRiskAllAccountsOldestDueAcrossGroups(t *testing.T) {
	s, store, _, _, now := riskAllAccountsHarness()
	other := *store.state.Rules[7]
	other.GroupID = 8
	store.state.Rules[8] = &other
	job := riskAllAccountsDueJob(t, s, now)
	require.Equal(t, int64(7), job.pending.GroupID)
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(11*time.Second)))
	// The other group's overdue account must precede even the next account in
	// group 7 if its deadline is older, regardless of group ID.
	other.Observation.ScheduledChecks[3].NextProbeAt = now.Add(9 * time.Second)
	next, err := s.tick(context.Background(), now.Add(12*time.Second))
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, int64(8), next.pending.GroupID)
	require.Equal(t, int64(3), next.account.ID)
}
