//go:build unit

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAccountRisk429SharedRollingWindow(t *testing.T) {
	m := miniredis.RunT(t)
	now := time.Now().UTC()
	m.SetTime(now)
	rdb := redis.NewClient(&redis.Options{Addr: m.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	a, b := NewAccountRisk429Counter(rdb), NewAccountRisk429Counter(rdb)
	ctx := context.Background()
	for i := 1; i <= 20; i++ {
		c := a
		if i%2 == 0 {
			c = b
		}
		count, err := c.Increment(ctx, "account", fmt.Sprint(i), 60, 21)
		require.NoError(t, err)
		require.Equal(t, int64(i), count)
	}
	count, err := a.Increment(ctx, "account", "20", 60, 21)
	require.NoError(t, err)
	require.Equal(t, int64(20), count, "same response persistence retry is idempotent")
	m.SetTime(now.Add(59 * time.Second))
	count, err = b.Increment(ctx, "account", "21", 60, 21)
	require.NoError(t, err)
	require.Equal(t, int64(21), count)
	m.SetTime(now.Add(60 * time.Second))
	count, err = a.Increment(ctx, "account", "22", 60, 21)
	require.NoError(t, err)
	require.Equal(t, int64(2), count, "responses on the old window boundary expire")
	for i := 23; i < 200; i++ {
		_, err = a.Increment(ctx, "account", fmt.Sprint(i), 60, 21)
		require.NoError(t, err)
	}
	require.Equal(t, int64(21), rdb.ZCard(ctx, "account_risk_429:account").Val())
	count, err = b.Increment(ctx, "another-account", "1", 60, 21)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	m.FastForward(61 * time.Second)
	require.False(t, m.Exists("account_risk_429:account"))
}
