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

func riskAllAccountsDueJobs(t *testing.T, s *AccountRiskService, now time.Time) []*accountRiskJob {
	t.Helper()
	jobs, err := s.tick(context.Background(), now)
	require.NoError(t, err)
	require.Empty(t, jobs, "wait a full interval after activation")
	jobs, err = s.tick(context.Background(), now.Add(10*time.Second))
	require.NoError(t, err)
	require.Len(t, jobs, 3, "all three due accounts are reserved in the same tick")
	for _, job := range jobs {
		require.Equal(t, accountRiskProbeAll, job.pending.Kind)
	}
	return jobs
}

func TestAccountRiskAllAccountsIndependentResultsAndTimers(t *testing.T) {
	for _, scenario := range []string{"pass", "mismatch", "empty reply", "notify only", "network", "incomplete", "429 exhausted", "429 incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskAllAccountsHarness()
			r := store.state.Rules[7]
			r.Config.Probe429StopEnabled, r.Config.NotifyEnabled = true, true
			jobs := riskAllAccountsDueJobs(t, s, now)
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
			require.NoError(t, s.finishProbe(context.Background(), jobs[0], result, probeErr, now.Add(11*time.Second)))
			require.Equal(t, !stop, accounts.pool[0].Schedulable)
			require.True(t, accounts.pool[1].Schedulable)
			require.True(t, accounts.pool[2].Schedulable)
			require.Len(t, store.state.ActiveProbes, 2)
			require.Equal(t, now.Add(21*time.Second), r.Observation.ScheduledChecks[1].NextProbeAt)
			firstResult := r.Observation.ScheduledChecks[1].LastProbeResult
			require.NotEmpty(t, firstResult)
			if stop {
				require.Equal(t, []int64{1}, store.stopped)
				require.Equal(t, "pending", r.Events[0].Notification)
			} else {
				require.Empty(t, store.stopped)
			}
			if scenario == "notify only" {
				require.Equal(t, "notify_only", r.Events[0].Action)
				require.Equal(t, "pending", r.Events[0].Notification)
				require.True(t, riskEventNotificationEnabled(r.Config, r.Events[0]))
			}
			// Results may arrive out of order; each releases only its own lease.
			require.NoError(t, s.finishProbe(context.Background(), jobs[2], AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(12*time.Second)))
			require.NoError(t, s.finishProbe(context.Background(), jobs[1], AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(13*time.Second)))
			require.Empty(t, store.state.ActiveProbes)
			require.Equal(t, firstResult, r.Observation.ScheduledChecks[1].LastProbeResult)
			require.Equal(t, now.Add(23*time.Second), r.Observation.ScheduledChecks[2].NextProbeAt)
			require.Equal(t, now.Add(22*time.Second), r.Observation.ScheduledChecks[3].NextProbeAt)
			require.True(t, accounts.pool[2].Schedulable)
		})
	}
}

func TestAccountRiskAllAccountsRetriesDoNotCarryAcrossAccounts(t *testing.T) {
	s, store, accounts, loads, now := riskAllAccountsHarness()
	r := store.state.Rules[7]
	r.Config.Probe429StopEnabled, r.Config.ProbeNotifyOnly = true, true
	jobs := riskAllAccountsDueJobs(t, s, now)
	probe := &riskProbeSequence{statuses: []int{429, 429, 429, 429, 429, 200, 200}}
	s.probe = probe
	s.runProbe(context.Background(), jobs[0])
	require.Equal(t, 4, probe.calls)
	require.Equal(t, []int64{1}, store.stopped)
	s.runProbe(context.Background(), jobs[1])
	s.runProbe(context.Background(), jobs[2])
	require.Equal(t, 7, probe.calls)
	require.Equal(t, []int64{1}, store.stopped)
	require.True(t, accounts.pool[1].Schedulable)
	require.True(t, accounts.pool[2].Schedulable)
	require.Zero(t, loads.slots)
	require.Empty(t, store.state.ActiveProbes)
}

func TestAccountRiskAllAccountsRestartAndExpiredLeaseDoNotDuplicatePeers(t *testing.T) {
	s, store, _, _, now := riskAllAccountsHarness()
	jobs := riskAllAccountsDueJobs(t, s, now)
	raw, err := json.Marshal(store.state)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &store.state))
	other := *s
	other.worker = "replacement"
	batch, err := other.tick(context.Background(), now.Add(11*time.Second))
	require.NoError(t, err)
	require.Empty(t, batch)
	// Only A's reservation expires. B/C remain in flight and cannot be replaced.
	store.state.ActiveProbes[2].ExpiresAt = now.Add(300 * time.Second)
	store.state.ActiveProbes[3].ExpiresAt = now.Add(300 * time.Second)
	batch, err = other.tick(context.Background(), now.Add(130*time.Second))
	require.NoError(t, err)
	require.Empty(t, batch)
	require.Equal(t, now.Add(140*time.Second), store.state.Rules[7].Observation.ScheduledChecks[1].NextProbeAt)
	batch, err = other.tick(context.Background(), now.Add(140*time.Second))
	require.NoError(t, err)
	require.Len(t, batch, 1)
	require.Equal(t, int64(1), batch[0].account.ID)
	require.NoError(t, s.finishProbe(context.Background(), jobs[0], AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(141*time.Second)))
	require.Equal(t, batch[0].pending.ID, store.state.ActiveProbes[1].ID)
	require.Len(t, store.state.ActiveProbes, 3)
	require.Empty(t, store.stopped)
}

func TestAccountRiskAllAccountsIdentityChangesAreIsolated(t *testing.T) {
	s, store, accounts, _, now := riskAllAccountsHarness()
	_, err := s.tick(context.Background(), now)
	require.NoError(t, err)
	accounts.pool[0].Credentials = map[string]any{"access_token": "rotated"}
	_, err = s.tick(context.Background(), now.Add(5*time.Second))
	require.NoError(t, err)
	require.Equal(t, now.Add(15*time.Second), store.state.Rules[7].Observation.ScheduledChecks[1].NextProbeAt)
	jobs, err := s.tick(context.Background(), now.Add(10*time.Second))
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	require.Equal(t, int64(2), jobs[0].account.ID)
	require.Equal(t, int64(3), jobs[1].account.ID)
	accounts.pool[1].Credentials = map[string]any{"access_token": "rotated again"}
	require.NoError(t, s.finishProbe(context.Background(), jobs[0], AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(11*time.Second)))
	require.Empty(t, store.stopped)
	require.Equal(t, jobs[1].pending.ID, store.state.ActiveProbes[3].ID)
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
				job, err := riskSingleTick(t, s, context.Background(), now.Add(time.Duration(sec)*time.Second))
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
			jobs := riskAllAccountsDueJobs(t, s, now)
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
			for _, job := range jobs {
				require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(11*time.Second)))
			}
			require.Empty(t, store.stopped)
			require.True(t, accounts.pool[0].Schedulable)
			job, err := riskSingleTick(t, s, context.Background(), now.Add(22*time.Second))
			require.NoError(t, err)
			require.Nil(t, job)
		})
	}
}

func TestAccountRiskAllAccountsCapacityAndScheduledProbesDoNotOverlapAccounts(t *testing.T) {
	s, store, accounts, loads, now := riskAllAccountsHarness()
	loads.first, loads.second = 59, 90
	var jobs []*accountRiskJob
	for sec := 0; sec <= 10; sec++ {
		var err error
		jobs, err = s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
		require.NoError(t, err)
		if sec < 10 {
			require.Empty(t, jobs)
		}
	}
	require.Len(t, jobs, 3)
	require.Equal(t, accountRiskProbeCapacity, jobs[0].pending.Kind)
	require.Equal(t, int64(1), jobs[0].account.ID)
	require.NotEmpty(t, jobs[0].pauseToken)
	require.NotContains(t, store.state.Rules[7].Observation.ScheduledChecks, int64(1))
	for _, job := range jobs[1:] {
		require.Equal(t, accountRiskProbeAll, job.pending.Kind)
	}
	require.NoError(t, s.finishProbe(context.Background(), jobs[0], AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(11*time.Second)))
	require.True(t, accounts.pool[0].Schedulable)
	require.Len(t, store.state.ActiveProbes, 2)
}

func TestAccountRiskAllAccountsDeduplicatesAccountsAcrossGroups(t *testing.T) {
	s, store, _, _, now := riskAllAccountsHarness()
	other := *store.state.Rules[7]
	other.GroupID = 8
	store.state.Rules[8] = &other
	_, err := s.tick(context.Background(), now)
	require.NoError(t, err)
	other.Observation.ScheduledChecks[3].NextProbeAt = now.Add(9 * time.Second)
	jobs, err := s.tick(context.Background(), now.Add(10*time.Second))
	require.NoError(t, err)
	require.Len(t, jobs, 3)
	require.Equal(t, int64(8), jobs[0].pending.GroupID)
	require.Equal(t, int64(3), jobs[0].account.ID)
	seen := map[int64]bool{}
	for _, job := range jobs {
		require.False(t, seen[job.account.ID])
		seen[job.account.ID] = true
	}
}
