//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillConfirmedAutomaticFailureImmediatelyReplacesExactlyOne(t *testing.T) {
	s, store, p, accounts := failureRefillHarness(t)
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	first := p.orders[0]
	p.states[first.ID] = "failed"
	store.mutate(t, func(v *accountRefillState) {
		// Keep the ordinary cooldown and monitoring deadline in the future.
		v.Config.CooldownSeconds = 3600
		v.NextCheckAt = time.Now().Add(time.Hour)
		v.Jobs[0].NextAttemptAt = time.Time{}
	})
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.orders, 2, "failure detection must create one replacement in the same run")
	require.Equal(t, first.ID, p.orders[1].ReplacesJobID)
	require.Equal(t, first.Plan, p.orders[1].Plan)
	require.NotEqual(t, first.ID, p.orders[1].ID)
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, 1, v.RotationCursor)
	require.Equal(t, 2, v.Usage[first.CreatedAt.In(refillTimezone).Format("2006-01-02")].Count)
	// Repeated ticks and a restart must continue the existing replacement.
	s = &AccountRefillService{store: store, provider: p, encryptor: s.encryptor, accounts: accounts, admin: accounts, groups: s.groups, loads: s.loads}
	for i := 0; i < 3; i++ {
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, p.orders, 2)
}

func TestRefillImmediateReplacementDoesNotBypassOrdinaryCooldown(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	store.mutate(t, func(v *accountRefillState) {
		v.LastSubmitAt = time.Now()
		v.NextCheckAt = time.Time{}
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, p.orders)
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Contains(t, v.Message, "冷却")
}

func TestRefillImmediateReplacementStillHonorsDailyCount(t *testing.T) {
	s, store, p, _ := failureRefillHarness(t)
	ctx := context.Background()
	store.mutate(t, func(v *accountRefillState) { v.Config.DailyMaxCount = 2 })
	require.NoError(t, s.run(ctx, false))
	for i := 0; i < 2; i++ {
		p.states[p.orders[len(p.orders)-1].ID] = "failed"
		store.mutate(t, func(v *accountRefillState) { v.Jobs[0].NextAttemptAt = time.Time{} })
		require.NoError(t, s.run(ctx, false))
	}
	require.Len(t, p.orders, 2, "repeated confirmed failures must stop at the daily submission limit")
	v, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Contains(t, v.Message, "本日数量")
}
