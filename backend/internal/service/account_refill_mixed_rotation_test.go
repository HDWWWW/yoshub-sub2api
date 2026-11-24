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

// The provider may list both standard and V2 with supply in the same tier.
func TestRefillRotationExactMuzeSKU(t *testing.T) {
	const sku = "pro20xv2"
	for _, tc := range []struct {
		name, product string
		stock, backup bool
		want          string
	}{
		{"pinned V2", sku, true, true, sku},
		{"automatic tier prefers standard", "", true, true, "pro20x"},
		{"missing V2 waits", sku, false, false, ""},
		{"missing V2 triggers configured 1208 backup", sku, false, true, "1208"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, store, muze, wallet, _ := reverseRefillHarness(t)
			muze.products = muzeStudioCatalog()
			muze.products[0].Closed = false
			muze.products[0].Stock, muze.products[0].AvailableNow = 2, 2
			if !tc.stock {
				muze.products = muze.products[:2]
			}
			v, err := s.View(ctx)
			require.NoError(t, err)
			c := v.Config
			c.RotationEnabled = true
			c.Rotation = []AccountRefillRotationStep{{Provider: "muze", Plan: "pro20x", MuzeProductID: tc.product, Count: 1}, {Provider: "1208", Plan: "pro20x", Count: 1}}
			c.MuzeFallback.Enabled = tc.backup
			require.NoError(t, s.Configure(ctx, c, "", v.Revision))
			v, err = s.View(ctx)
			require.NoError(t, err)
			require.Equal(t, tc.product, v.NextMuzeProductID)
			require.Equal(t, c.Rotation, v.Config.Rotation)
			require.NoError(t, s.run(ctx, false))
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			switch tc.want {
			case "":
				require.Empty(t, state.Jobs)
				require.Zero(t, state.RotationCursor)
				require.Contains(t, state.Message, sku)
				require.Empty(t, wallet.orders)
				require.Zero(t, muze.orders)
			case "1208":
				require.Len(t, wallet.orders, 1)
				require.Zero(t, muze.orders)
				require.Len(t, state.Jobs, 1)
				j := state.Jobs[0]
				require.True(t, j.SupplyFallback)
				require.Empty(t, j.Provider)
				require.False(t, j.MuzeProductPinned)
				require.Empty(t, j.MuzeProductID)
				require.Equal(t, "pro20x", j.Plan)
				require.Equal(t, sku, state.MuzeFallback.Shortage.MuzeProductID)
			default:
				require.Len(t, state.Jobs, 1)
				require.Equal(t, 1, muze.orders)
				require.Empty(t, wallet.orders)
				j := state.Jobs[0]
				require.Equal(t, tc.want, muze.cycle.Product)
				require.Equal(t, tc.want, j.MuzeProductID)
				require.Equal(t, tc.product != "", j.MuzeProductPinned)
				require.Equal(t, tc.product, j.purchaseRoute().muzeProductID)
				require.Equal(t, "pro20x", j.Plan)
				require.Equal(t, 1, state.RotationCursor)
			}
		})
	}
}

func TestRefillRotationMuzeBatchHonorsPinnedSlotsAndSharedQuota(t *testing.T) {
	ctx := context.Background()
	s, store, muze, _, _ := reverseRefillHarness(t)
	muze.products = muzeStudioCatalog()
	muze.products[0].Closed = false
	muze.products[0].Stock, muze.products[0].AvailableNow = 1, 1
	seedDailyRefill(t, store, time.Now().Truncate(time.Minute), 2)
	store.mutate(t, func(v *accountRefillState) {
		v.Config.RotationEnabled = true
		v.Config.Rotation = []AccountRefillRotationStep{{Provider: "muze", Plan: "pro20x", Count: 1}, {Provider: "muze", Plan: "pro20x", MuzeProductID: "pro20x", Count: 1}}
	})
	muze.products[0].Remaining, muze.products[2].Remaining = 1, 1
	require.NoError(t, s.run(ctx, false))
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, v.Jobs)
	require.Zero(t, v.RotationCursor)
	muze.products[0].Remaining, muze.products[2].Remaining = 2, 2
	store.mutate(t, func(v *accountRefillState) { v.DailyScheduleStatus.RetryAt = time.Time{} })
	require.NoError(t, s.run(ctx, false))
	v, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 2, v.DailyScheduleStatus.Message)
	require.Equal(t, "pro20xv2", v.Jobs[0].MuzeProductID)
	require.False(t, v.Jobs[0].MuzeProductPinned)
	require.Equal(t, "pro20x", v.Jobs[1].MuzeProductID)
	require.True(t, v.Jobs[1].MuzeProductPinned)
}

func TestRefillRotationValidatesMuzeProductBinding(t *testing.T) {
	for _, tc := range []struct {
		primary, provider, id string
		ok                    bool
	}{
		{"1208", "muze", "pro20xv2", true}, {"muze", "", "pro20xv2", true}, {"1208", "", "pro20xv2", false},
		{"muze", "1208", "pro20x", false}, {"1208", "muze", "pro5xv2", false}, {"muze", "muze", "arbitrary", false}, {"1208", "", "", true},
	} {
		c := AccountRefillConfig{PrimaryProvider: tc.primary, RotationEnabled: true, Rotation: []AccountRefillRotationStep{{Provider: tc.provider, Plan: "pro20x", MuzeProductID: tc.id, Count: 1}}}
		require.Equal(t, tc.ok, validateRefillRotation(c) == nil)
	}
}
