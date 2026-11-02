//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type refillGroupedTestStore struct {
	*refillTestStore
	children map[int64]*refillTestStore
}

func (s *refillGroupedTestStore) ForGroup(id int64) AccountRefillStore {
	if s.children[id] == nil {
		s.children[id] = &refillTestStore{}
	}
	return s.children[id]
}
func (s *refillGroupedTestStore) ListRules(context.Context) (map[int64]string, error) {
	rows := map[int64]string{0: s.raw}
	for id, child := range s.children {
		if child.raw != "" {
			rows[id] = child.raw
		}
	}
	return rows, nil
}

func TestAccountRefillGroupRulesRemainIndependent(t *testing.T) {
	s, base, p, _ := refillHarness(t)
	s.provider = &manualRefillProvider{p, 100}
	store := &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	s.store = store
	ctx := context.Background()
	legacy, err := s.ForGroup(ctx, 2)
	require.NoError(t, err)
	require.Same(t, s, legacy)
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	v, err := child.View(ctx)
	require.NoError(t, err)
	require.False(t, v.Config.Enabled)
	require.True(t, v.TokenConfigured)
	require.Equal(t, []int64{3}, v.Config.DeliveryGroupIDs)
	require.Empty(t, store.children[3].raw, "view must not save or buy")
	c := v.Config
	c.MinReady, c.CooldownSeconds = 7, 600
	c.MaxInFlight = 10
	c.RotationEnabled = true
	c.Rotation = []AccountRefillRotationStep{{Plan: "pro20x", Count: 1}, {Plan: "pro5x", Count: 2}}
	require.NoError(t, child.Configure(ctx, c, "", 0))
	rootView, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, rootView.Config.MinReady)
	require.Equal(t, 300, rootView.Config.CooldownSeconds)
	require.True(t, rootView.Config.Enabled)
	require.Len(t, rootView.Rules, 2)
	// One group's unresolved order must not become another group's pending count.
	base.mutate(t, func(state *accountRefillState) {
		state.Jobs = []AccountRefillJob{{ID: "uncertain", Status: "submitting", Price: 60}}
	})
	v, err = child.View(ctx)
	require.NoError(t, err)
	require.Zero(t, v.Evaluation.Pending)
	require.Equal(t, 7, v.Evaluation.Needed)
	base.mutate(t, func(state *accountRefillState) { state.Jobs[0].DeliveryGroupIDs = []int64{2, 3} })
	v, err = child.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, v.Evaluation.Pending, "cross-group deliveries cover the same pool's shortage")
	require.Equal(t, 6, v.Evaluation.Needed)
	require.Empty(t, v.Jobs, "other rules retain their own order history")
	// Yet both must reserve against the same supplier wallet (100 - 60 < 60).
	r := manualRequest()
	r.GroupID = 3
	require.ErrorContains(t, child.Manual(ctx, r), "积分不足")
	base.mutate(t, func(state *accountRefillState) { state.Jobs = nil })
	require.NoError(t, child.Manual(ctx, r))
	v, err = child.View(ctx)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 1)
	require.Equal(t, int64(3), v.Jobs[0].GroupID)
	require.Equal(t, []int64{3}, v.Jobs[0].DeliveryGroupIDs)
	require.Zero(t, v.RotationCursor, "manual orders do not advance rotation")
	r.GroupID = 2
	require.ErrorContains(t, s.Manual(ctx, r), "其他分组")
	require.Error(t, child.Configure(ctx, c, "", 0), "stale revision must be rejected")
	c.GroupID = 2
	require.Error(t, child.Configure(ctx, c, "", 1), "cannot overwrite a different group")
	require.Empty(t, p.submitted)
}

type rotationTestProvider struct {
	*manualRefillProvider
	submitted []AccountRefillJob
	uncertain bool
}

func (p *rotationTestProvider) Submit(_ context.Context, _ string, job AccountRefillJob) (*RefillProviderJob, error) {
	p.submitted = append(p.submitted, job)
	if p.uncertain {
		return nil, errors.New("uncertain network result")
	}
	return &RefillProviderJob{ID: job.ID, Status: "queued", Price: job.Price}, nil
}

func TestAccountRefillRotationPersistsRetriesAndRestarts(t *testing.T) {
	s, store, baseProvider, accounts := refillHarness(t)
	p := &rotationTestProvider{manualRefillProvider: &manualRefillProvider{baseProvider, 500}, uncertain: true}
	s.provider = p
	store.mutate(t, func(state *accountRefillState) {
		state.Config.RotationEnabled = true
		state.Config.Rotation = []AccountRefillRotationStep{{Plan: "pro20x", Count: 1}, {Plan: "pro5x", Count: 2}}
		state.Config.PlanImportConcurrency = map[string]int{"pro5x": 50, "pro20x": 60}
	})
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	first := p.submitted[0]
	require.Equal(t, "pro20x", first.Plan)
	require.Equal(t, 60, first.Concurrency)
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, v.RotationCursor)
	refillDue(t, store)
	p.uncertain = false
	restarted := &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
	require.NoError(t, restarted.run(ctx, false))
	require.Equal(t, first.ID, p.submitted[1].ID)
	require.Equal(t, first.Plan, p.submitted[1].Plan)
	require.Equal(t, first.Concurrency, p.submitted[1].Concurrency)
	for _, expected := range []string{"pro5x", "pro5x", "pro20x"} {
		store.mutate(t, func(state *accountRefillState) {
			for i := range state.Jobs {
				state.Jobs[i].Status = "imported"
			}
			state.LastSubmitAt, state.NextCheckAt = time.Time{}, time.Time{}
		})
		require.NoError(t, restarted.run(ctx, false))
		require.Equal(t, expected, p.submitted[len(p.submitted)-1].Plan)
		wantConcurrency := map[string]int{"pro5x": 50, "pro20x": 60}[expected]
		require.Equal(t, wantConcurrency, p.submitted[len(p.submitted)-1].Concurrency)
	}
	// Editing an unrelated field keeps progress; changing the sequence resets it.
	v, err = restarted.View(ctx)
	require.NoError(t, err)
	c := v.Config
	c.MinReady = 2
	require.NoError(t, restarted.Configure(ctx, c, "", v.Revision))
	v, err = restarted.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, v.RotationCursor)
	c.Rotation[1].Count = 3
	require.NoError(t, restarted.Configure(ctx, c, "", v.Revision))
	v, err = restarted.View(ctx)
	require.NoError(t, err)
	require.Zero(t, v.RotationCursor)
}

func TestAccountRefillRotationCannotAdvanceWithoutReservation(t *testing.T) {
	s, store, p, _ := refillHarness(t)
	s.provider = &manualRefillProvider{p, 500}
	store.mutate(t, func(state *accountRefillState) {
		state.Config.RotationEnabled = true
		state.Config.Rotation = []AccountRefillRotationStep{{Plan: "pro20x", Count: 1}, {Plan: "pro5x", Count: 2}}
	})
	store.failSave = true
	require.Error(t, s.run(context.Background(), false))
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Zero(t, state.RotationCursor)
	require.Empty(t, state.Jobs)
	require.Empty(t, p.submitted)
}

func TestAccountRefillRotationValidation(t *testing.T) {
	for _, rows := range [][]AccountRefillRotationStep{nil, {{Plan: "unknown", Count: 1}}, {{Plan: "pro20x", Count: 0}}, {{Plan: "pro20x", Count: 101}}, make([]AccountRefillRotationStep, 21)} {
		c := refillTestConfig()
		c.RotationEnabled = true
		c.Rotation = rows
		require.Error(t, c.Validate())
	}
}

func TestAccountRefillAllGroupsResumeAfterRestart(t *testing.T) {
	s, base, provider, accounts := refillHarness(t)
	p := &rotationTestProvider{manualRefillProvider: &manualRefillProvider{provider, 500}}
	s.provider = p
	s.store = &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	ctx := context.Background()
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	v, err := child.View(ctx)
	require.NoError(t, err)
	c := v.Config
	c.Enabled = true
	c.Mode = "fixed"
	c.Plan = "pro20x"
	require.NoError(t, child.Configure(ctx, c, "", 0))
	restarted := &AccountRefillService{store: s.store, provider: p, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
	require.NoError(t, restarted.runAllRules(ctx))
	require.Len(t, p.submitted, 2)
	require.Equal(t, int64(2), p.submitted[0].GroupID)
	require.Equal(t, "pro5x", p.submitted[0].Plan)
	require.Equal(t, int64(3), p.submitted[1].GroupID)
	require.Equal(t, "pro20x", p.submitted[1].Plan)
	require.NotEqual(t, p.submitted[0].ID, p.submitted[1].ID)
}
