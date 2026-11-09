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

func riskThreeAccounts(t *testing.T) (*AccountRiskService, *riskTestStore, *riskTestAccounts, *riskTestLoads, time.Time) {
	t.Helper()
	s, store, accounts, loads, now := riskHarness()
	third := accounts.pool[1]
	third.ID, third.Priority = 3, 3
	accounts.pool = append(accounts.pool, third)
	loads.byID = map[int64]int{1: 30, 2: 40, 3: 100}
	store.state.Rules[7].Config.CapacityNotifyOnly = true
	store.state.Rules[7].Config.SustainSeconds = 2
	store.state.Rules[7].Config.SecondAtLeastPercent = 75
	return s, store, accounts, loads, now
}

func TestAccountRiskThirdAccountTriggersAllEarlierLowAccounts(t *testing.T) {
	for _, outcome := range []string{"pass", "mismatch", "notify only", "network", "incomplete"} {
		t.Run(outcome, func(t *testing.T) {
			s, store, _, _, now := riskThreeAccounts(t)
			job := riskCapacityDueJob(t, s, now)
			require.Equal(t, int64(1), job.account.ID)
			require.Equal(t, int64(3), job.referenceID)
			r := store.state.Rules[7]
			require.Len(t, r.Observation.CapacityChecks, 3)
			require.Equal(t, now, *r.Observation.CapacityChecks[2].AnomalySince)
			result := AccountRiskProbeResult{Completed: true, Text: r.Config.ExpectedText}
			var probeErr error
			switch outcome {
			case "mismatch", "notify only":
				result.Text = "wrong"
				r.Config.CapacityNotifyOnly = true
			case "network":
				probeErr = errors.New("network unavailable")
			case "incomplete":
				result.Completed = false
			}
			require.NoError(t, s.finishProbe(context.Background(), job, result, probeErr, now.Add(3*time.Second)))
			require.Empty(t, store.stopped)
			// Persist/reload between samples, as the real store does. The second
			// account keeps its own timer even after the first passes or stops.
			raw, err := json.Marshal(store.state)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &store.state))
			next, err := s.tick(context.Background(), now.Add(4*time.Second))
			require.NoError(t, err)
			require.NotNil(t, next)
			require.Equal(t, int64(2), next.account.ID)
			require.Equal(t, int64(3), next.referenceID)
			probe := s.probe.(*riskTestProbe)
			probe.result = AccountRiskProbeResult{Completed: true, Text: r.Config.ExpectedText}
			s.runProbe(context.Background(), next)
			require.Equal(t, int64(2), probe.lastAccountID, "non-first account must pass dispatch identity checks")
			require.Equal(t, 1, probe.calls)
		})
	}
}

func TestAccountRiskLaterTargetsAndPriorityTiers(t *testing.T) {
	for _, scenario := range []string{"first healthy", "equal first two", "same priority reference", "reverse order", "fourth full", "first boundary", "reference boundary", "reference below boundary", "unlimited first"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, loads, now := riskThreeAccounts(t)
			want, reference := int64(1), int64(3)
			switch scenario {
			case "first healthy", "first boundary":
				loads.byID[1] = 80
				if scenario == "first boundary" {
					loads.byID[1] = 60
				}
				want = 2
			case "equal first two":
				accounts.pool[1].Priority = 1
			case "same priority reference":
				accounts.pool[0].Priority, accounts.pool[1].Priority = 3, 3
				want = 0
			case "reverse order":
				s.groups.(*riskTestGroups).reverse = true
				loads.byID = map[int64]int{1: 100, 2: 40, 3: 30}
				want, reference = 3, 1
			case "fourth full":
				fourth := accounts.pool[2]
				fourth.ID, fourth.Priority = 4, 4
				accounts.pool = append(accounts.pool, fourth)
				loads.byID[3], loads.byID[4] = 20, 100
				reference = 4
			case "reference boundary":
				loads.byID[3] = 75
			case "reference below boundary":
				loads.byID[3] = 74
				want = 0
			case "unlimited first":
				accounts.pool[0].Concurrency = 0
				want = 2
			}
			var job *accountRiskJob
			for sec := 0; sec <= 2; sec++ {
				var err error
				job, err = s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
				require.NoError(t, err)
			}
			if want == 0 {
				require.Nil(t, job)
			} else {
				require.NotNil(t, job)
				require.Equal(t, want, job.account.ID)
				require.Equal(t, reference, job.referenceID)
			}
			require.Empty(t, store.stopped)
		})
	}
}

func TestAccountRiskMultiAccountContinuityAndIndependentCooldown(t *testing.T) {
	s, store, _, loads, now := riskThreeAccounts(t)
	job := riskCapacityDueJob(t, s, now)
	// Complete the first, then second target with the same result. Both retain
	// cooldowns across recovery and neither creates a per-second probe loop.
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(3*time.Second)))
	second, err := s.tick(context.Background(), now.Add(4*time.Second))
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, int64(2), second.account.ID)
	require.NoError(t, s.finishProbe(context.Background(), second, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(5*time.Second)))
	for sec := 6; sec <= 12; sec++ {
		loads.byID[1] = 30
		if sec == 6 {
			loads.byID[1] = 100
		}
		next, err := s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
		require.NoError(t, err)
		require.Nil(t, next)
	}
	require.Equal(t, now.Add(303*time.Second), *store.state.Rules[7].Observation.CapacityChecks[1].NextProbeAt)
	require.Equal(t, now.Add(305*time.Second), *store.state.Rules[7].Observation.CapacityChecks[2].NextProbeAt)
}

func TestAccountRiskMultiAccountRejectsUnknownAndStalePairs(t *testing.T) {
	for _, scenario := range []string{"missing third load", "negative third load", "third recovers", "target changes", "reference changes", "order changes", "sample gap", "new worker"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, loads, now := riskThreeAccounts(t)
			for sec := 0; sec < 2; sec++ {
				_, err := s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
				require.NoError(t, err)
			}
			at := now.Add(2 * time.Second)
			switch scenario {
			case "missing third load":
				delete(loads.byID, 3)
			case "negative third load":
				loads.byID[3] = -1
			case "third recovers":
				loads.byID[3] = 74
			case "target changes":
				accounts.pool[0].Credentials = map[string]any{"access_token": "rotated"}
				accounts.pool[1].Credentials = map[string]any{"access_token": "rotated"}
			case "reference changes":
				accounts.pool[2].Credentials = map[string]any{"access_token": "rotated"}
			case "order changes":
				accounts.pool[2].Priority = 0
			case "sample gap":
				at = now.Add(10 * time.Second)
			case "new worker":
				s.worker = "new"
			}
			job, err := s.tick(context.Background(), at)
			require.NoError(t, err)
			require.Nil(t, job)
			require.Empty(t, store.stopped)
		})
	}
	for _, change := range []string{"reference removed", "target removed", "reversed", "same priority"} {
		t.Run("inflight "+change, func(t *testing.T) {
			s, store, accounts, loads, now := riskThreeAccounts(t)
			loads.byID[1] = 80
			job := riskCapacityDueJob(t, s, now)
			require.Equal(t, int64(2), job.account.ID)
			switch change {
			case "reference removed":
				accounts.pool[2].Schedulable = false
			case "target removed":
				accounts.pool[1].Schedulable = false
			case "reversed":
				s.groups.(*riskTestGroups).reverse = true
			case "same priority":
				accounts.pool[1].Priority = 3
			}
			require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(3*time.Second)))
			require.Empty(t, store.stopped)
		})
	}
}

func TestAccountRiskMultiAccountReservationAndFairRetests(t *testing.T) {
	s, store, _, _, now := riskThreeAccounts(t)
	job := riskCapacityDueJob(t, s, now)
	other := *s
	other.worker = "second"
	next, err := other.tick(context.Background(), now.Add(3*time.Second))
	require.NoError(t, err)
	require.Nil(t, next)
	require.Equal(t, job.pending.ID, store.state.Pending.ID)
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(4*time.Second)))
	// Even if the first target's cooldown expires before a new one is tested,
	// the never-tested second target is selected first.
	store.state.Rules[7].Observation.CapacityChecks[1].NextProbeAt = nil
	for sec := 5; sec <= 7; sec++ {
		next, err = s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
		require.NoError(t, err)
	}
	require.NotNil(t, next)
	require.Equal(t, int64(2), next.account.ID)
}

func TestAccountRiskNonFirstMismatchStopsOnlyItsTarget(t *testing.T) {
	s, store, _, loads, now := riskThreeAccounts(t)
	loads.byID[1] = 80
	store.state.Rules[7].Config.CapacityNotifyOnly = false
	job := riskCapacityDueJob(t, s, now)
	require.Equal(t, int64(2), job.account.ID)
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(3*time.Second)))
	require.Equal(t, []int64{2}, store.stopped)
	require.Equal(t, int64(2), store.state.Rules[7].Events[0].AccountID)
	require.Contains(t, store.state.Rules[7].Events[0].Reason, "后位账号 #3")
}

func TestAccountRiskStableThirdReferenceSurvivesMiddleLoadChanges(t *testing.T) {
	s, _, _, loads, now := riskThreeAccounts(t)
	for sec := 0; sec <= 2; sec++ {
		loads.byID[2] = 40
		if sec == 1 {
			loads.byID[2] = 80
		}
		job, err := s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
		require.NoError(t, err)
		if sec < 2 {
			require.Nil(t, job)
		} else {
			require.NotNil(t, job)
			require.Equal(t, int64(1), job.account.ID)
			require.Equal(t, int64(3), job.referenceID)
		}
	}
}

func TestAccountRiskLegacyCooldownSurvivesMultiAccountUpgrade(t *testing.T) {
	s, store, accounts, _, now := riskThreeAccounts(t)
	r := store.state.Rules[7]
	r.Observation.First = &AccountRiskLoad{AccountID: 1}
	r.Observation.ProbeSignature = riskAccountSignature(&accounts.pool[0]) + riskAccountSignature(&accounts.pool[1])
	r.Observation.NextProbeAt = riskAfter(now, 300)
	job := riskCapacityDueJob(t, s, now)
	require.Equal(t, int64(2), job.account.ID, "old first-account cooldown must not delay the new second target")
	require.Equal(t, now.Add(300*time.Second), *r.Observation.CapacityChecks[1].NextProbeAt)
}

func TestAccountRiskInterruptedMultiAccountProbeDoesNotBlockNextTarget(t *testing.T) {
	s, store, _, _, now := riskThreeAccounts(t)
	first := riskCapacityDueJob(t, s, now)
	var next *accountRiskJob
	for sec := 3; sec <= 122; sec++ {
		var err error
		next, err = s.tick(context.Background(), now.Add(time.Duration(sec)*time.Second))
		require.NoError(t, err)
		if sec < 122 {
			require.Nil(t, next)
		}
	}
	require.NotNil(t, next)
	require.Equal(t, int64(2), next.account.ID)
	require.NotEqual(t, first.pending.ID, next.pending.ID)
	require.Empty(t, store.stopped)
	// An expired first result cannot finish or stop the second reservation.
	require.NoError(t, s.finishProbe(context.Background(), first, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(123*time.Second)))
	require.Equal(t, next.pending.ID, store.state.Pending.ID)
	require.Empty(t, store.stopped)
}
