//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type refillDelayedPollingProvider struct {
	*refillContractProvider
	delay time.Duration
}

func (p *refillDelayedPollingProvider) Status(ctx context.Context, token, id string) (*RefillProviderJob, error) {
	time.Sleep(p.delay)
	return p.refillContractProvider.Status(ctx, token, id)
}

func TestRefillPollingFixedIntervalAfterSuccessfulQueriesAndDisconnects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, store, provider, accounts, _ := refillContractHarness(t)
		store.mutate(t, func(state *accountRefillState) {
			state.Config.Enabled, state.Config.MonitorEnabled = false, false
		})
		ctx := context.Background()
		// Reproduce the production trigger: many successful status queries
		// before the first broken connection. Attempts remains an audit count.
		for i := 0; i < 9; i++ {
			time.Sleep(20 * time.Second)
			require.NoError(t, s.run(ctx, false))
		}
		require.Equal(t, 9, provider.statusCalls)
		provider.statusErr = &refillProviderTemporaryError{"connection interrupted"}
		time.Sleep(20 * time.Second)
		for i := 0; i < 3; i++ {
			require.NoError(t, s.run(ctx, false))
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Equal(t, 11+i, state.Jobs[0].Attempts)
			require.WithinDuration(t, time.Now().Add(20*time.Second), state.Jobs[0].NextAttemptAt, 0)
			require.Equal(t, "supplier-1", state.Jobs[0].SupplierID)
			require.Len(t, provider.submitted, 1)
			require.Zero(t, accounts.creates)
			// A service replacement must honor the persisted deadline.
			restarted := *s
			s = &restarted
			time.Sleep(19 * time.Second)
			require.NoError(t, s.run(ctx, false))
			require.Equal(t, 10+i, provider.statusCalls, "do not query before the saved deadline")
			time.Sleep(time.Second)
		}
		provider.statusErr = nil
		provider.response.Status, provider.response.ResultReady = "succeeded", true
		require.NoError(t, s.run(ctx, false))
		time.Sleep(20 * time.Second)
		require.NoError(t, s.run(ctx, false))
		view, err := s.View(ctx)
		require.NoError(t, err)
		require.Equal(t, "imported", view.Jobs[0].Status)
		require.Len(t, provider.submitted, 1, "polling must never create another order")
		require.Equal(t, 1, accounts.creates)
	})
}

func TestRefillPollingWaitsTwentySecondsAfterSlowFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _, provider, _, state := refillContractHarness(t)
		provider.statusErr = context.DeadlineExceeded
		s.provider = &refillDelayedPollingProvider{refillContractProvider: provider, delay: 25 * time.Second}
		state.Jobs[0].Attempts = 100
		started := time.Now()
		advanceRefillContract(t, s, &state)
		require.Equal(t, started.Add(25*time.Second), time.Now())
		require.Equal(t, started.Add(45*time.Second), state.Jobs[0].NextAttemptAt)
		require.Equal(t, "supplier-1", state.Jobs[0].SupplierID)
	})
}

func TestRefillPollingStatusesAndRetryAfter(t *testing.T) {
	for _, scenario := range []string{"processing", "review", "unknown", "empty reply", "server error", "429 no delay", "429 retry after", "503 retry after", "unauthorized", "forbidden"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, _, provider, accounts, state := refillContractHarness(t)
				state.Jobs[0].Attempts = 100
				want := 20 * time.Second
				switch scenario {
				case "review":
					provider.response.Status = "review"
				case "unknown":
					provider.response.Status = "new-status"
				case "empty reply":
					provider.response = nil
				case "server error":
					provider.statusErr = &refillProviderError{code: 500}
				case "429 no delay":
					provider.statusErr = &refillProviderError{code: 429}
				case "429 retry after":
					provider.statusErr = &refillProviderError{code: 429, retryAfter: 90 * time.Second}
					want = 90 * time.Second
				case "503 retry after":
					provider.statusErr = &refillProviderError{code: 503, retryAfter: 2 * time.Minute}
					want = 2 * time.Minute
				case "unauthorized", "forbidden":
					code := 401
					if scenario == "forbidden" {
						code = 403
					}
					provider.statusErr = &refillProviderError{code: code}
					want = 5 * time.Minute
				}
				advanceRefillContract(t, s, &state)
				require.Equal(t, time.Now().Add(want), state.Jobs[0].NextAttemptAt)
				if scenario == "429 no delay" || scenario == "429 retry after" {
					require.Equal(t, state.Jobs[0].NextAttemptAt, state.ProviderRetryAt)
				}
				if scenario == "unauthorized" || scenario == "forbidden" {
					require.Equal(t, "blocked", state.Jobs[0].Status)
				}
				require.Len(t, provider.submitted, 1)
				require.Zero(t, accounts.creates)
			})
		})
	}
}

func TestRefillPollingDeliveryDisconnectUsesSameTwentySecondInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _, provider, accounts, state := refillContractHarness(t)
		state.Jobs[0].SupplierStatus, state.Jobs[0].ResultReady = "succeeded", true
		provider.deliveryErr = &refillProviderTemporaryError{"delivery interrupted"}
		advanceRefillContract(t, s, &state)
		require.Equal(t, time.Now().Add(20*time.Second), state.Jobs[0].NextAttemptAt)
		require.Equal(t, "awaiting_delivery", state.Jobs[0].Status)
		require.False(t, state.Jobs[0].ResultReady)
		require.Len(t, provider.submitted, 1)
		require.Zero(t, accounts.creates)
	})
}

type fenduduPollingProvider struct {
	*fenduduTestProvider
	err        error
	delay      time.Duration
	queryCalls int
}

func (p *fenduduPollingProvider) Cycle(ctx context.Context, token, id string) (*fenduduCycle, error) {
	p.queryCalls++
	time.Sleep(p.delay)
	if p.err != nil {
		return nil, p.err
	}
	return p.fenduduTestProvider.Cycle(ctx, token, id)
}

func TestFenduduPollingFixedIntervalAndRateLimit(t *testing.T) {
	for _, scenario := range []string{"pending", "disconnect", "slow timeout", "rate limit"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// Manual request IDs require a 13-digit Unix millisecond timestamp.
				time.Sleep(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Sub(time.Now()))
				s, store, base, accounts := fenduduHarness(t)
				ctx := context.Background()
				require.NoError(t, s.Manual(ctx, fenduduRequest()))
				require.NoError(t, s.run(ctx, false))
				provider := &fenduduPollingProvider{fenduduTestProvider: base}
				s.fendudu = provider
				store.mutate(t, func(state *accountRefillState) { state.Jobs[0].Attempts = 100 })
				want := 20 * time.Second
				switch scenario {
				case "disconnect":
					provider.err = &refillProviderTemporaryError{"connection interrupted"}
				case "slow timeout":
					provider.err, provider.delay = context.DeadlineExceeded, 25*time.Second
				case "rate limit":
					provider.err = &fenduduHTTPError{remote: &refillProviderError{code: 429, retryAfter: 90 * time.Second}, message: "rate limited"}
					want = 90 * time.Second
				}
				time.Sleep(20 * time.Second)
				require.NoError(t, s.run(ctx, false))
				state, err := decodeAccountRefillState(store.raw)
				require.NoError(t, err)
				require.Equal(t, 1, provider.queryCalls)
				require.WithinDuration(t, time.Now().Add(want), state.Jobs[0].NextAttemptAt, 0)
				if scenario == "rate limit" {
					require.Equal(t, state.Jobs[0].NextAttemptAt, state.Fendudu.RetryAt)
				}
				time.Sleep(want - time.Nanosecond)
				require.NoError(t, s.run(ctx, false))
				require.Equal(t, 1, provider.queryCalls)
				time.Sleep(time.Nanosecond)
				require.NoError(t, s.run(ctx, false))
				require.Equal(t, 2, provider.queryCalls)
				require.Equal(t, 1, base.orders)
				require.Zero(t, accounts.adopt)
			})
		})
	}
}

func TestFenduduPollingLostReceiptRecoveryDoesNotBackOffOrResubmit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Manual request IDs require a 13-digit Unix millisecond timestamp.
		time.Sleep(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Sub(time.Now()))
		s, store, base, accounts := fenduduHarness(t)
		ctx := context.Background()
		base.orderError = errors.New("lost response")
		require.NoError(t, s.Manual(ctx, fenduduRequest()))
		require.NoError(t, s.run(ctx, false))
		s.fendudu = &fenduduRecoveryProvider{fenduduTestProvider: base, readError: &refillProviderTemporaryError{"query interrupted"}}
		store.mutate(t, func(state *accountRefillState) { state.Jobs[0].Attempts = 100 })
		for i := 0; i < 3; i++ {
			time.Sleep(20 * time.Second)
			require.NoError(t, s.run(ctx, false))
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.WithinDuration(t, time.Now().Add(20*time.Second), state.Jobs[0].NextAttemptAt, 0)
			require.Equal(t, 101+i, state.Jobs[0].Attempts)
			require.Empty(t, state.Jobs[0].SupplierID)
			require.Equal(t, 1, base.orders)
			require.Zero(t, accounts.adopt)
		}
	})
}
