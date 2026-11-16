//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type riskConcurrentLoads struct {
	*riskTestLoads
	active atomic.Int64
}

func (l *riskConcurrentLoads) AcquireAccountSlot(context.Context, int64, int) (*AcquireResult, error) {
	l.active.Add(1)
	return &AcquireResult{Acquired: true, ReleaseFunc: func() { l.active.Add(-1) }}, nil
}

type riskConcurrentProbe struct {
	started chan int64
	release map[int64]chan struct{}
	text    string
}

func (p *riskConcurrentProbe) RunRiskProbe(ctx context.Context, a *Account, _ AccountRiskConfig) (AccountRiskProbeResult, error) {
	p.started <- a.ID
	select {
	case <-p.release[a.ID]:
		text := p.text
		if text == "" {
			text = "21"
		}
		return AccountRiskProbeResult{Completed: true, Text: text}, nil
	case <-ctx.Done():
		return AccountRiskProbeResult{}, ctx.Err()
	}
}

func TestAccountRiskConcurrentFailuresAllPersistAndRespectNotifyOnly(t *testing.T) {
	for _, notifyOnly := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			s, store, accounts, baseLoads, now := riskAllAccountsHarness()
			for id := int64(4); id <= 64; id++ {
				a := accounts.pool[0]
				a.ID = id
				accounts.pool = append(accounts.pool, a)
			}
			store.state.Rules[7].Config.ProbeNotifyOnly = notifyOnly
			store.state.Rules[7].Config.NotifyEnabled = !notifyOnly
			loads := &riskConcurrentLoads{riskTestLoads: baseLoads}
			release := make(chan struct{})
			probe := &riskConcurrentProbe{started: make(chan int64, 64), release: map[int64]chan struct{}{}, text: "wrong"}
			for _, a := range accounts.pool {
				probe.release[a.ID] = release
			}
			s.loads, s.probe = loads, probe
			_, err := s.tick(context.Background(), now)
			require.NoError(t, err)
			time.Sleep(10 * time.Second)
			jobs, err := s.tick(context.Background(), time.Now())
			require.NoError(t, err)
			require.Len(t, jobs, 64)
			s.dispatchProbes(context.Background(), jobs)
			synctest.Wait()
			require.EqualValues(t, 64, loads.active.Load())
			close(release)
			s.wg.Wait()
			state, err := store.Read(context.Background())
			require.NoError(t, err)
			require.Empty(t, state.ActiveProbes)
			require.Len(t, state.Rules[7].Events, 64, "concurrent completions must not overwrite another result")
			for _, event := range state.Rules[7].Events {
				require.Equal(t, "pending", event.Notification)
				if notifyOnly {
					require.Equal(t, "notify_only", event.Action)
				} else {
					require.Equal(t, "stop", event.Action)
				}
			}
			for _, a := range accounts.pool {
				require.Equal(t, notifyOnly, a.Schedulable)
			}
			if notifyOnly {
				require.Empty(t, store.stopped)
			} else {
				require.Len(t, store.stopped, 64)
			}
			notifier := &riskNoticeStub{}
			s.notifier = notifier
			for i := 0; i < 64; i++ {
				require.NoError(t, s.deliverNotification(context.Background(), time.Now().Add(time.Duration(i)*3*time.Second)))
			}
			require.Len(t, notifier.messages, 64, "every failed account must retain its notification")
			state, err = store.Read(context.Background())
			require.NoError(t, err)
			require.Len(t, state.Rules[7].Events, 50)
		})
	}
}

func TestAccountRiskConcurrentDispatchAndIndependentNextRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, _, baseLoads, now := riskAllAccountsHarness()
		loads := &riskConcurrentLoads{riskTestLoads: baseLoads}
		p := &riskConcurrentProbe{started: make(chan int64, 10), release: map[int64]chan struct{}{
			1: make(chan struct{}), 2: make(chan struct{}), 3: make(chan struct{}),
		}}
		s.loads, s.probe = loads, p
		_, err := s.tick(context.Background(), now)
		require.NoError(t, err)
		time.Sleep(10 * time.Second)
		jobs, err := s.tick(context.Background(), time.Now())
		require.NoError(t, err)
		require.Len(t, jobs, 3)
		s.dispatchProbes(context.Background(), jobs)
		synctest.Wait()
		require.EqualValues(t, 3, loads.active.Load(), "all accounts must reach upstream before any response completes")
		started := []int64{<-p.started, <-p.started, <-p.started}
		require.ElementsMatch(t, []int64{1, 2, 3}, started)
		other := *s
		duplicate, err := other.tick(context.Background(), time.Now().Add(time.Second))
		require.NoError(t, err)
		require.Empty(t, duplicate)
		// B can complete and run its next interval while A and C are still slow.
		time.Sleep(2 * time.Second)
		close(p.release[2])
		synctest.Wait()
		state, err := store.Read(context.Background())
		require.NoError(t, err)
		require.Len(t, state.ActiveProbes, 2)
		require.True(t, now.Add(22*time.Second).Equal(state.Rules[7].Observation.ScheduledChecks[2].NextProbeAt))
		time.Sleep(10 * time.Second)
		next, err := s.tick(context.Background(), time.Now())
		require.NoError(t, err)
		require.Len(t, next, 1)
		require.EqualValues(t, 2, next[0].account.ID)
		s.dispatchProbes(context.Background(), next)
		synctest.Wait()
		require.EqualValues(t, 2, <-p.started)
		require.EqualValues(t, 2, loads.active.Load())
		close(p.release[1])
		close(p.release[3])
		s.wg.Wait()
		require.Zero(t, loads.active.Load())
		state, err = store.Read(context.Background())
		require.NoError(t, err)
		require.Empty(t, state.ActiveProbes)
		for _, check := range state.Rules[7].Observation.ScheduledChecks {
			require.Contains(t, check.LastProbeResult, "检测通过")
		}
	})
}

func TestAccountRiskConcurrentManualTestExcludesOnlyItsAccount(t *testing.T) {
	s, store, _, _, now := riskAllAccountsHarness()
	jobs := riskAllAccountsDueJobs(t, s, now)
	_, err := s.TestNow(context.Background(), 7, 1, 1, store.state.Rules[7].Config)
	require.ErrorIs(t, err, ErrAccountRiskBusy)
	require.NoError(t, s.finishProbe(context.Background(), jobs[0], AccountRiskProbeResult{Completed: true, Text: "21"}, nil, now.Add(11*time.Second)))
	s.probe = &riskTestProbe{result: AccountRiskProbeResult{Completed: true, Text: "21"}}
	result, err := s.TestNow(context.Background(), 7, 1, 1, store.state.Rules[7].Config)
	require.NoError(t, err)
	require.True(t, result.Completed)
	require.Len(t, store.state.ActiveProbes, 2)
	require.Equal(t, jobs[1].pending.ID, store.state.ActiveProbes[2].ID)
	// A genuine old-version reservation blocks a new batch until it finishes.
	store.state.ActiveProbes = nil
	store.state.Pending = &AccountRiskPending{ID: "old-worker", ExpiresAt: now.Add(time.Minute)}
	next, err := s.tick(context.Background(), now.Add(30*time.Second))
	require.NoError(t, err)
	require.Empty(t, next)
}

func TestAccountRiskConcurrentResultSaveContentionDoesNotRepeatProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, _, _, now := riskAllAccountsHarness()
		job := riskAllAccountsDueJobs(t, s, now)[0]
		store.busy = 10 // More contention than the old five-attempt save budget.
		probe := &riskTestProbe{result: AccountRiskProbeResult{Completed: true, Text: "wrong"}}
		s.probe = probe
		s.runProbe(context.Background(), job)
		require.Equal(t, 1, probe.calls)
		require.Equal(t, []int64{1}, store.stopped)
		require.NotContains(t, store.state.ActiveProbes, int64(1))
		require.Len(t, store.state.ActiveProbes, 2)
	})
}
