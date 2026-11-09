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

func TestAccountRiskCapacityPausesAllTargetsBeforeFirstProbe(t *testing.T) {
	s, store, accounts, _, now := riskThreeAccounts(t)
	store.state.Rules[7].Config.CapacityNotifyOnly = false
	first := riskCapacityDueJob(t, s, now)
	require.Equal(t, []int64{1, 2}, store.stopped)
	require.False(t, accounts.pool[0].Schedulable)
	require.False(t, accounts.pool[1].Schedulable)
	require.True(t, accounts.pool[2].Schedulable)
	require.Zero(t, s.probe.(*riskTestProbe).calls)
	pool, err := s.candidates(context.Background(), 7, now.Add(3*time.Second))
	require.NoError(t, err)
	require.Len(t, pool, 1, "paused targets must disappear from ordinary admission")
	other := *s
	other.worker = "other-instance"
	job, err := other.tick(context.Background(), now.Add(3*time.Second))
	require.NoError(t, err)
	require.Nil(t, job)
	require.NoError(t, s.finishProbe(context.Background(), first, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(4*time.Second)))
	require.Equal(t, []int64{1}, store.resumed)
	require.False(t, accounts.pool[1].Schedulable)
	second, err := other.tick(context.Background(), now.Add(5*time.Second))
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, int64(2), second.account.ID)
	require.NoError(t, other.finishProbe(context.Background(), second, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(6*time.Second)))
	require.Equal(t, []int64{1, 2}, store.resumed)
	require.Empty(t, store.state.CapacityPauses)
}

func TestAccountRiskCapacityRetriesWhilePausedAfterRestart(t *testing.T) {
	for _, outcome := range []string{"wrong", "network", "incomplete", "expired"} {
		t.Run(outcome, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			store.state.Rules[7].Config.ProbeIntervalSeconds = 10
			job := riskCapacityDueJob(t, s, now)
			result := AccountRiskProbeResult{Completed: outcome != "incomplete", Text: "wrong"}
			var failure error
			if outcome == "network" {
				failure = errors.New("network")
			}
			at := now.Add(11 * time.Second)
			if outcome == "expired" {
				at = job.pending.ExpiresAt
				_, err := s.tick(context.Background(), at)
				require.NoError(t, err)
			} else {
				require.NoError(t, s.finishProbe(context.Background(), job, result, failure, at))
			}
			require.False(t, accounts.pool[0].Schedulable)
			require.Empty(t, store.resumed)
			// A restart has no in-memory job, but persists the stopped account and queue.
			raw, err := json.Marshal(store.state)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(raw, &store.state))
			other := *s
			other.worker = "restarted"
			for i := 1; i < 10; i++ {
				next, err := other.tick(context.Background(), at.Add(time.Duration(i)*time.Second))
				require.NoError(t, err)
				require.Nil(t, next)
				require.Equal(t, store.state.CapacityPauses[1].NextProbeAt, store.state.Rules[7].Observation.NextProbeAt)
			}
			next, err := other.tick(context.Background(), at.Add(10*time.Second))
			require.NoError(t, err)
			require.NotNil(t, next)
			require.Equal(t, int64(1), next.account.ID)
			require.NoError(t, other.finishProbe(context.Background(), next, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, at.Add(11*time.Second)))
			require.True(t, accounts.pool[0].Schedulable)
			require.Equal(t, []int64{1}, store.resumed)
		})
	}
}

func TestAccountRiskCapacityPassingProbeCannotUndoOtherChanges(t *testing.T) {
	for _, change := range []string{"manual stop", "manual enable", "credentials", "group removed", "disabled", "revision", "quota", "cooldown", "CAS", "model mismatch"} {
		t.Run(change, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			store.state.Rules[7].Config.ModelMismatchAction = "stop"
			job := riskCapacityDueJob(t, s, now)
			a := &accounts.pool[0]
			switch change {
			case "manual stop":
				delete(a.Extra, AccountRiskCapacityPauseExtraKey)
			case "manual enable":
				a.Schedulable = true
				delete(a.Extra, AccountRiskCapacityPauseExtraKey)
			case "credentials":
				a.Credentials = map[string]any{"access_token": "new"}
			case "group removed":
				a.GroupIDs = nil
			case "disabled":
				store.state.Rules[7].Config.Enabled = false
			case "revision":
				store.state.Rules[7].Revision++
			case "quota":
				a.Credentials = map[string]any{"account_scheduling_threshold": 99}
				a.Extra["codex_5h_used_percent"] = 100.
				a.Extra["codex_5h_reset_at"] = now.Add(time.Hour).Format(time.RFC3339)
			case "cooldown":
				until := now.Add(time.Hour)
				a.TempUnschedulableUntil = &until
			case "CAS":
				store.cas = false
			case "model mismatch":
				require.NoError(t, s.HandleModelMismatch(context.Background(), 7, a, "gpt-6-astra", "gpt-5.4", now.Add(11*time.Second)))
			}
			require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(12*time.Second)))
			require.Empty(t, store.resumed)
			require.Equal(t, change == "manual enable", accounts.pool[0].Schedulable)
		})
	}
}

func TestAccountRiskCapacityRecoversWithoutOriginalWitness(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	job := riskCapacityDueJob(t, s, now)
	accounts.pool = accounts.pool[:1]
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(12*time.Second)))
	require.True(t, accounts.pool[0].Schedulable)
	require.Equal(t, []int64{1}, store.resumed)
}
