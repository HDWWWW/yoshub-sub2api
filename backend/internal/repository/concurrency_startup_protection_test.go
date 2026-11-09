package repository

import (
	"context"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestStartupPreservesLiveSlotsAndReclaimsCrashResidue(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	cache := NewConcurrencyCache(client, 15, 900)
	ctx := context.Background()
	ok, err := cache.AcquireAccountSlot(ctx, 7, 1, "older-process-request")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.AcquireUserSlot(ctx, 9, 1, "older-process-user")
	require.NoError(t, err)
	require.True(t, ok)
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.PreserveProcessSlots = true
	_ = service.ProvideConcurrencyService(cache, nil, cfg)
	n, err := cache.GetAccountConcurrency(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	n, err = cache.GetUserConcurrency(ctx, 9)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	ok, err = cache.AcquireAccountSlot(ctx, 7, 1, "new-process-request")
	require.NoError(t, err)
	require.False(t, ok)
	// Simulate an unrefreshed slot left by a crash. Cleanup uses age, not PID.
	now, err := client.Time(ctx).Result()
	require.NoError(t, err)
	require.NoError(t, client.ZAdd(ctx, accountSlotKey(7), redis.Z{Score: float64(now.Add(-16 * time.Minute).Unix()), Member: "older-process-request"}).Err())
	require.NoError(t, cache.CleanupExpiredAccountSlots(ctx, 7))
	ok, err = cache.AcquireAccountSlot(ctx, 7, 1, "new-process-request")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = cache.IncrementAccountWaitCount(ctx, 8, 10)
	require.NoError(t, err)
	require.True(t, ok)
	server.FastForward(16 * time.Minute)
	n, err = cache.GetAccountWaitingCount(ctx, 8)
	require.NoError(t, err)
	require.Zero(t, n)
}
