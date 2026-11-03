//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerCacheManualPriorityOverridesExpiryAndRestores(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	// A second reader models another worker with its own warmed decoded snapshot.
	reader := newSchedulerCacheWithChunkSizes(cache.rdb, defaultSchedulerSnapshotMGetChunkSize, defaultSchedulerSnapshotWriteChunkSize)
	bucket := service.SchedulerBucket{GroupID: 2, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	now := time.Now().Truncate(time.Second)
	soon, later := now.Add(time.Hour), now.Add(2*time.Hour)
	accounts := []service.Account{
		{ID: 153, Platform: service.PlatformOpenAI, Priority: 7, ExpiresAt: &soon},
		{ID: 154, Platform: service.PlatformOpenAI, Priority: 7, ExpiresAt: &later},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, accounts))

	check := func(wantFirst int64, wantManual *int) {
		t.Helper()
		for _, c := range []service.SchedulerCache{cache, reader} {
			candidates, hit, err := c.GetSnapshot(ctx, bucket)
			require.NoError(t, err)
			require.True(t, hit)
			require.Len(t, candidates, 2)
			rows := make([]service.Account, len(candidates))
			for i, candidate := range candidates {
				rows[i] = *candidate
				full, err := c.GetAccount(ctx, candidate.ID)
				require.NoError(t, err)
				require.NotNil(t, full)
				require.Equal(t, 7, candidate.Priority, "automatic ranks never overwrite the native priority")
				require.Equal(t, service.SchedulingMetadata(full).ManualPriority, service.SchedulingMetadata(candidate).ManualPriority,
					"candidate selection and full hydration must see the same override")
				if candidate.ID == 154 {
					require.Equal(t, wantManual, service.SchedulingMetadata(candidate).ManualPriority)
				}
			}
			preview := service.BuildAccountSchedulingPreview(rows, cfg, now)
			require.Equal(t, wantFirst, preview.Items[0].AccountID)
		}
	}
	check(153, nil) // Warm both workers before changing only the override metadata.
	for _, priority := range []int{0, 9999, 1} {
		accounts[1].Extra = map[string]any{service.AccountManualPriorityExtraKey: priority}
		require.NoError(t, cache.SetAccount(ctx, &accounts[1]))
		first := int64(153)
		if priority == 0 {
			first = 154
		}
		check(first, &priority)
	}
	accounts[1].Extra[service.AccountManualPriorityExtraKey] = nil
	require.NoError(t, cache.SetAccount(ctx, &accounts[1]))
	check(153, nil) // Restore automatic ordering without rebuilding bucket membership.
}
