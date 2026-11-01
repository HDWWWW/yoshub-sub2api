//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Match the repository's session lock semantics: readers remain available,
// competing writers receive Busy, and a callback holds the lock until return.
type refillConcurrentStore struct {
	operation sync.Mutex
	data      sync.RWMutex
	raw       string
	contended chan struct{}
}

func (s *refillConcurrentStore) Read(context.Context) (string, error) {
	s.data.RLock()
	defer s.data.RUnlock()
	return s.raw, nil
}

func (s *refillConcurrentStore) WithLock(ctx context.Context, fn func(string, func(string) error) error) error {
	if !s.operation.TryLock() {
		select {
		case s.contended <- struct{}{}:
		default:
		}
		return ErrAccountRefillBusy
	}
	defer s.operation.Unlock()
	raw, _ := s.Read(ctx)
	return fn(raw, func(next string) error {
		s.data.Lock()
		defer s.data.Unlock()
		s.raw = next
		return nil
	})
}

type refillSlowDeliveryProvider struct {
	*refillTestProvider
	entered chan struct{}
	release chan struct{}
}

func (p *refillSlowDeliveryProvider) Delivery(ctx context.Context, token, id string) (*RefillDelivery, error) {
	close(p.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.release:
		return p.refillTestProvider.Delivery(ctx, token, id)
	}
}

func TestRefillAutoFastSaveDuring20xDeliveryDoesNotLoseOrDuplicateOrder(t *testing.T) {
	s, original, provider, accounts := refillHarness(t)
	now := time.Now()
	c, account := autoFastFixture(now)
	accounts.accounts = []Account{account}
	original.mutate(t, func(state *accountRefillState) {
		state.Config = c
		state.Jobs = []AccountRefillJob{{
			ID: "pending-pro20x", SupplierID: "supplier-1", Plan: "pro20x", Price: 80,
			Status: "awaiting_delivery", SupplierStatus: "succeeded", ResultReady: true,
			GroupID: 2, DeliveryGroupIDs: []int64{2}, DeliveryConfig: c.Delivery,
			Concurrency: c.ImportConcurrency, CreatedAt: now,
		}}
		state.Usage[now.In(refillTimezone).Format("2006-01-02")] = AccountRefillDailyUsage{Count: 1, Points: 80}
	})
	store := &refillConcurrentStore{raw: original.raw, contended: make(chan struct{}, 1)}
	s.store = store
	s.sampleCapacity(context.Background())
	require.True(t, s.accountAutoFast(2, 1, time.Now()))
	before, err := s.View(context.Background())
	require.NoError(t, err)
	p := &refillSlowDeliveryProvider{refillTestProvider: provider, entered: make(chan struct{}), release: make(chan struct{})}
	s.provider = p
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	workerDone, saveDone := make(chan error, 1), make(chan error, 1)
	go func() { workerDone <- s.run(ctx, false) }()
	select {
	case <-p.entered:
	case <-ctx.Done():
		t.Fatal("worker did not reach supplier delivery")
	}
	// Fast sampling and status reads continue while the supplier holds the lock.
	s.sampleCapacity(ctx)
	require.True(t, s.accountAutoFast(2, 1, time.Now()))
	during, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "importing", during.Jobs[0].Status)
	c.AutoFast.StopMode = "custom"
	c.AutoFast.StopUsed7dPercent = 90
	go func() { saveDone <- s.Configure(ctx, c, "", before.Revision) }()
	select {
	case <-store.contended:
	case <-ctx.Done():
		t.Fatal("save did not contend with the worker")
	}
	select {
	case err := <-saveDone:
		t.Fatalf("save returned before the supplier released its lock: %v", err)
	default:
	}
	close(p.release)
	require.NoError(t, <-workerDone)
	require.NoError(t, <-saveDone)
	require.NoError(t, s.run(ctx, false))
	after, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, before.Revision+1, after.Revision)
	require.Equal(t, "custom", after.Config.AutoFast.StopMode)
	require.Equal(t, 90.0, after.Config.AutoFast.StopUsed7dPercent)
	require.Len(t, after.Jobs, 1)
	require.Equal(t, "pro20x", after.Jobs[0].Plan)
	require.Equal(t, "imported", after.Jobs[0].Status)
	require.Equal(t, 1, accounts.creates)
	require.Equal(t, 1, provider.deliveryCalls)
	require.Empty(t, provider.submitted, "an accepted order must not be bought again")
	require.Equal(t, 1, after.Daily.Count)
	require.Zero(t, after.Daily.ReservedPoints)
	require.Equal(t, 80.0, after.Daily.SpentPoints)
}
