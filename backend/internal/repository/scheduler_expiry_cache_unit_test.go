//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerCacheExpiryAdmissionAndRetentionRefresh(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 8, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	original := time.Now().Add(-15 * time.Minute).Truncate(time.Second)
	a := service.Account{ID: 46, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, AutoPauseOnExpired: true, ExpiresAt: &original,
		Extra: map[string]any{service.AccountImportExpiryGraceExtraKey: 1800, service.AccountSourceExpiryExtraKey: original.Unix(),
			service.AccountSourceExpiryOriginExtraKey: "supplier_api", "unrelated_large_payload": "drop me"}}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{a}))
	check := func(allowed bool) {
		t.Helper()
		candidates, hit, err := cache.GetSnapshot(ctx, bucket)
		require.NoError(t, err)
		require.True(t, hit)
		require.Len(t, candidates, 1)
		full, err := cache.GetAccount(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, allowed, candidates[0].IsSchedulable())
		require.Equal(t, full.SchedulingExpiresAt().Unix(), candidates[0].SchedulingExpiresAt().Unix())
		require.Equal(t, allowed, full.IsSchedulable())
		require.NotContains(t, candidates[0].Extra, "unrelated_large_payload")
	}
	check(true) // Legacy records must keep grace even before data conversion.
	service.MaterializeAccountExpiry(&a)
	require.NoError(t, cache.SetAccount(ctx, &a))
	check(true)
	service.ApplyAccountExpiryExtra(&a, map[string]any{service.AccountImportExpiryGraceExtraKey: 0})
	require.NoError(t, cache.SetAccount(ctx, &a))
	check(false) // Invalidates memoized snapshots as well as both Redis payloads.
	service.ApplyAccountExpiryExtra(&a, map[string]any{service.AccountImportExpiryGraceExtraKey: 3600})
	require.NoError(t, cache.SetAccount(ctx, &a))
	check(true)
	service.ApplyAccountExpiryWrite(ctx, &a, original.Unix())
	require.NoError(t, cache.SetAccount(ctx, &a))
	check(false) // A native manual deadline does not get a second grace period.
}
