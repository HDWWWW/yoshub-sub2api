//go:build unit

package service

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillTimingMinimumValidationAndPersistence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		check, cooldown int
		valid           bool
	}{
		{"minimum", 5, 5, true},
		{"previous minimum", 15, 60, true},
		{"maximum", 3600, 86400, true},
		{"check below minimum", 4, 5, false},
		{"cooldown below minimum", 5, 4, false},
		{"check above maximum", 3601, 5, false},
		{"cooldown above maximum", 5, 86401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, _, _ := refillHarness(t)
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			cfg := state.Config
			cfg.CheckSeconds, cfg.CooldownSeconds = tc.check, tc.cooldown
			before := store.raw
			err = s.Configure(context.Background(), cfg, "", state.Revision)
			if !tc.valid {
				require.Error(t, err)
				require.Equal(t, before, store.raw)
				return
			}
			require.NoError(t, err)
			view, err := s.View(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.check, view.Config.CheckSeconds)
			require.Equal(t, tc.cooldown, view.Config.CooldownSeconds)
		})
	}
}

type refillTimingStore struct {
	mu  sync.Mutex
	raw string
}

func (s *refillTimingStore) Read(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raw, nil
}

func (s *refillTimingStore) WithLock(_ context.Context, fn func(string, func(string) error) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(s.raw, func(raw string) error { s.raw = raw; return nil })
}

type refillTimingProvider struct {
	*failureRefillProvider
	mu sync.Mutex
}

func (p *refillTimingProvider) Submit(ctx context.Context, token string, job AccountRefillJob) (*RefillProviderJob, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failureRefillProvider.Submit(ctx, token, job)
}

func (p *refillTimingProvider) submittedOrders() []AccountRefillJob {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.orders)
}

func TestRefillTimingWorkerHonorsFiveSecondSettings(t *testing.T) {
	for _, tc := range []struct {
		name            string
		check, cooldown int
		secondOrderAt   time.Duration
	}{
		{"both minimum", 5, 5, 10 * time.Second},
		{"longer check", 10, 5, 15 * time.Second},
		{"longer cooldown", 5, 10, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, baseStore, baseProvider, _ := failureRefillHarness(t)
				provider := &refillTimingProvider{failureRefillProvider: baseProvider}
				s.provider = provider
				baseStore.mutate(t, func(state *accountRefillState) {
					state.Config.Mode = "fixed"
					state.Config.CheckSeconds, state.Config.CooldownSeconds = tc.check, tc.cooldown
					state.Config.MinReady, state.Config.MaxReady, state.Config.MaxInFlight = 3, 3, 3
				})
				s.store = &refillTimingStore{raw: baseStore.raw}
				s.stop = make(chan struct{})
				s.Start()
				defer s.Stop()
				synctest.Wait()
				time.Sleep(5*time.Second - time.Nanosecond)
				synctest.Wait()
				require.Empty(t, provider.submittedOrders())
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				require.Len(t, provider.submittedOrders(), 1, "the first worker tick must run at five seconds")
				time.Sleep(tc.secondOrderAt - 5*time.Second - time.Nanosecond)
				synctest.Wait()
				require.Len(t, provider.submittedOrders(), 1, "both check interval and cooldown must elapse before another purchase")
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				orders := provider.submittedOrders()
				require.Len(t, orders, 2)
				require.NotEqual(t, orders[0].ID, orders[1].ID)
			})
		})
	}
}
