//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func mixedRefillSteps() []AccountRefillRotationStep {
	return []AccountRefillRotationStep{{Provider: "1208", Plan: "pro20x", Count: 1}, {Provider: refillProviderMuze, Plan: "pro20x", Count: 1}}
}

func TestRefillMixedRotationPlacesOrdersAcrossReload(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, _ := reverseRefillHarness(t)
	store.mutate(t, func(v *accountRefillState) {
		v.Config.PrimaryProvider = "1208"
		v.Config.RotationEnabled = true
		v.Config.Rotation = mixedRefillSteps()
		v.Config.MinReady = 3
		v.Config.MaxReady = 5
		v.Config.MaxInFlight = 3
		v.Config.MuzeFallback.Enabled = false
	})
	for i, want := range []string{"1208", "muze", "1208"} {
		store.mutate(t, func(v *accountRefillState) {
			v.LastSubmitAt = time.Time{}
			v.NextCheckAt = time.Time{}
			if i == 1 {
				v.ProviderRetryAt = time.Now().Add(time.Hour)
			} else {
				v.ProviderRetryAt = time.Time{}
			}
		})
		view, err := s.View(ctx)
		require.NoError(t, err)
		require.Equal(t, want, view.NextProvider)
		require.Equal(t, "pro20x", view.NextPlan)
		require.NoError(t, s.run(ctx, false))
		v, err := decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.Len(t, v.Jobs, i+1, v.Message)
		require.Equal(t, want, refillPublicProvider(v.Jobs[0].Provider))
		require.Equal(t, "pro20x", v.Jobs[0].Plan)
	}
	require.Len(t, wallet.orders, 2)
	require.Equal(t, 1, muze.orders)
}

func TestRefillMixedRotationFailureUsesOppositeOfFailedStep(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, _ := reverseRefillHarness(t)
	store.mutate(t, func(v *accountRefillState) {
		v.Config.PrimaryProvider = "1208"
		v.Config.RotationEnabled = true
		v.Config.Rotation = mixedRefillSteps()
		v.RotationCursor = 1
	})
	muze.orderError = &muzeHTTPError{remote: &refillProviderError{code: 400}, code: "insufficient_stock", message: "unavailable", rejection: true}
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, muze.orders)
	require.NoError(t, s.run(ctx, false))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, wallet.orders, 1)
	require.Equal(t, v.Jobs[1].ID, v.Jobs[0].FallbackForJobID)
	require.Equal(t, 0, v.RotationCursor)
}

func TestRefillMixedDailyBatchReservesAtomically(t *testing.T) {
	ctx := context.Background()
	s, store, muze, wallet, _ := reverseRefillHarness(t)
	seedDailyRefill(t, store, time.Now().Truncate(time.Minute), 2)
	store.mutate(t, func(v *accountRefillState) { v.Config.RotationEnabled = true; v.Config.Rotation = mixedRefillSteps() })
	wallet.available = 0
	require.NoError(t, s.run(ctx, false))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, v.Jobs)
	require.Zero(t, v.RotationCursor)
	require.Zero(t, muze.orders)
	wallet.available = 500
	store.mutate(t, func(v *accountRefillState) { v.DailyScheduleStatus.RetryAt = time.Time{} })
	require.NoError(t, s.run(ctx, false))
	v, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 2, v.DailyScheduleStatus.Message)
	require.Empty(t, v.Jobs[0].Provider)
	require.Empty(t, v.Jobs[0].MuzeConnectionID)
	require.Equal(t, refillProviderMuze, v.Jobs[1].Provider)
	require.NotEmpty(t, v.Jobs[1].MuzeConnectionID)
	require.Equal(t, v.Jobs[0].DailyBatchID, v.Jobs[1].DailyBatchID)
	require.Zero(t, v.RotationCursor)
}
