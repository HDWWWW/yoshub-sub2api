//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type refillConfigBusyStore struct {
	AccountRefillStore
	busy           int
	calls, entered int
	beforeAcquire  func()
}

func (s *refillConfigBusyStore) WithLock(ctx context.Context, fn func(string, func(string) error) error) error {
	s.calls++
	if s.busy > 0 {
		s.busy--
		return ErrAccountRefillBusy
	}
	if s.beforeAcquire != nil {
		s.beforeAcquire()
		s.beforeAcquire = nil
	}
	return s.AccountRefillStore.WithLock(ctx, func(raw string, save func(string) error) error { s.entered++; return fn(raw, save) })
}

func TestRefillConfigureWaitsForWorkerAndSavesOnce(t *testing.T) {
	s, base, provider, _ := refillHarness(t)
	state, err := decodeAccountRefillState(base.raw)
	require.NoError(t, err)
	store := &refillConfigBusyStore{AccountRefillStore: base, busy: 2}
	s.store = store
	c := state.Config
	c.LeadMinutes = 12
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	after, err := decodeAccountRefillState(base.raw)
	require.NoError(t, err)
	require.Equal(t, 12, after.Config.LeadMinutes)
	require.Equal(t, state.Revision+1, after.Revision)
	require.Equal(t, 3, store.calls)
	require.Equal(t, 1, store.entered)
	require.Empty(t, provider.submitted)
	require.Equal(t, state.Jobs, after.Jobs)
	require.Equal(t, state.Usage, after.Usage)
}

func TestRefillConfigureRechecksRevisionAfterWaiting(t *testing.T) {
	s, base, _, _ := refillHarness(t)
	state, err := decodeAccountRefillState(base.raw)
	require.NoError(t, err)
	store := &refillConfigBusyStore{AccountRefillStore: base, busy: 1, beforeAcquire: func() { base.mutate(t, func(v *accountRefillState) { v.Revision++; v.Config.LeadMinutes = 18 }) }}
	s.store = store
	c := state.Config
	c.LeadMinutes = 12
	require.ErrorContains(t, s.Configure(context.Background(), c, "", state.Revision), "配置已被其他页面修改")
	after, err := decodeAccountRefillState(base.raw)
	require.NoError(t, err)
	require.Equal(t, 18, after.Config.LeadMinutes)
	require.Equal(t, state.Revision+1, after.Revision)
	require.Equal(t, 1, store.entered)
}

func TestRefillConfigureCancelledWaitDoesNotWrite(t *testing.T) {
	s, base, provider, _ := refillHarness(t)
	state, err := decodeAccountRefillState(base.raw)
	require.NoError(t, err)
	before := base.raw
	store := &refillConfigBusyStore{AccountRefillStore: base, busy: 100}
	s.store = store
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Configure(ctx, state.Config, "", state.Revision), context.DeadlineExceeded)
	require.Equal(t, before, base.raw)
	require.Zero(t, store.entered)
	require.Empty(t, provider.submitted)
}

func TestRefillConfigLockWaitIsBounded(t *testing.T) {
	store := &refillConfigBusyStore{busy: 100}
	err := withAccountRefillConfigLock(context.Background(), store, 20*time.Millisecond, func(string, func(string) error) error { t.Fatal("must not enter while busy"); return nil })
	require.ErrorIs(t, err, ErrAccountRefillBusy)
	require.Zero(t, store.entered)
	require.Equal(t, 1, store.calls)
}

func TestRefillConfigLockDoesNotReplayEnteredCallbackOrStorageErrors(t *testing.T) {
	for _, failure := range []error{ErrAccountRefillBusy, errors.New("uncertain durable save")} {
		store := &refillConfigBusyStore{AccountRefillStore: &refillTestStore{}}
		err := withAccountRefillConfigLock(context.Background(), store, time.Second, func(string, func(string) error) error { return failure })
		require.ErrorIs(t, err, failure)
		require.Equal(t, 1, store.calls)
		require.Equal(t, 1, store.entered)
	}
}

func TestRefillWorkerStillSkipsBusyLockWithoutWaiting(t *testing.T) {
	s, base, provider, _ := refillHarness(t)
	store := &refillConfigBusyStore{AccountRefillStore: base, busy: 100}
	s.store = store
	require.ErrorIs(t, s.run(context.Background(), false), ErrAccountRefillBusy)
	require.Equal(t, 1, store.calls)
	require.Empty(t, provider.submitted)
}
