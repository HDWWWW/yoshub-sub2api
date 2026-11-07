//go:build unit

package service

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func seedDailyRefill(t *testing.T, store *refillTestStore, due time.Time, quantity int) {
	t.Helper()
	store.mutate(t, func(v *accountRefillState) {
		v.Config.DailySchedule = &AccountRefillDailySchedule{Enabled: true, Time: due.In(refillTimezone).Format("15:04"), Quantity: quantity}
		v.DailyScheduleStatus.NextAt = due
		v.Config.Enabled, v.Config.MonitorEnabled = false, false
		v.Config.CheckSeconds, v.Config.CooldownSeconds = 3600, 86400
		v.Config.WindowEnabled = true
		v.Config.WindowStart, v.Config.WindowEnd = "00:00", "00:01"
		v.NextCheckAt = due.Add(time.Hour)
	})
}

func TestRefillDailyBatchAtBeijingTimeBypassesNormalGatesAndSurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s, store, provider, accounts := failureRefillHarness(t)
		accounts.accounts = []Account{refillTestAccount(time.Now().Add(72 * time.Hour))}
		seedStalledRefill(t, store, "review", time.Now())
		due := time.Now().Add(time.Minute)
		require.Equal(t, "08:01", due.In(refillTimezone).Format("15:04"))
		seedDailyRefill(t, store, due, 3)
		time.Sleep(time.Minute - time.Nanosecond)
		require.NoError(t, s.run(ctx, false))
		require.Empty(t, provider.orders)
		time.Sleep(time.Nanosecond)
		require.NoError(t, s.Check(ctx), "preview never consumes the daily batch")
		require.NoError(t, s.run(ctx, false))
		state, err := decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.Len(t, state.Jobs, 4, "full healthy pool, review and in-flight cap do not block N jobs")
		require.Equal(t, []string{"pro5x", "pro20x", "pro5x"}, []string{state.Jobs[0].Plan, state.Jobs[1].Plan, state.Jobs[2].Plan})
		require.Equal(t, 3, state.DailyScheduleStatus.LastQuantity)
		require.True(t, state.DailyScheduleStatus.NextAt.Equal(due.Add(24*time.Hour)))
		batch := state.DailyScheduleStatus.LastBatchID
		provider.beforeSubmit = func(j AccountRefillJob) {
			v, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Equal(t, batch, v.DailyScheduleStatus.LastBatchID)
			require.Equal(t, 4, v.Usage[time.Now().In(refillTimezone).Format("2006-01-02")].Count)
			require.Equal(t, batch, j.DailyBatchID)
		}
		s = &AccountRefillService{store: store, provider: provider, encryptor: s.encryptor, accounts: accounts, groups: s.groups, loads: s.loads}
		for i := 0; i < 6; i++ {
			require.NoError(t, s.run(ctx, false))
		}
		require.Len(t, provider.orders, 3, "restart and repeated ticks submit each reserved job once")
		// Saving a different clock cannot create a second batch on this day.
		state, err = decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		config := state.Config
		config.DailySchedule = &AccountRefillDailySchedule{Enabled: true, Time: "09:00", Quantity: 2}
		require.NoError(t, s.Configure(ctx, config, "", state.Revision))
		state, err = decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.Equal(t, "2000-01-02 09:00", state.DailyScheduleStatus.NextAt.In(refillTimezone).Format("2006-01-02 15:04"))
		time.Sleep(time.Until(state.DailyScheduleStatus.NextAt))
		provider.beforeSubmit = nil
		require.NoError(t, s.run(ctx, false))
		state, err = decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.NotEqual(t, batch, state.DailyScheduleStatus.LastBatchID)
		require.Equal(t, 2, state.DailyScheduleStatus.LastQuantity)
		require.Len(t, state.Jobs, 6)
	})
}

func TestRefillDailyBatchBlocksAtomicallyAndRetriesWithoutDuplication(t *testing.T) {
	for _, scenario := range []string{"budget", "balance", "price", "backoff", "save failure", "preview", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, provider, _ := failureRefillHarness(t)
			seedDailyRefill(t, store, time.Now().Truncate(time.Minute), 3)
			store.mutate(t, func(v *accountRefillState) {
				switch scenario {
				case "budget":
					v.Config.DailyMaxCount = 2
				case "balance":
					provider.available = 1
				case "price":
					v.Config.MaxUnitPrice = 1
				case "backoff":
					v.ProviderRetryAt = time.Now().Add(time.Hour)
				case "disabled":
					v.Config.DailySchedule.Enabled = false
				}
			})
			store.failSave = scenario == "save failure"
			err := s.run(context.Background(), scenario == "preview")
			if scenario == "save failure" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Empty(t, provider.orders)
			require.Empty(t, state.Jobs)
			require.Empty(t, state.DailyScheduleStatus.LastBatchID)
			if scenario != "disabled" && scenario != "preview" {
				store.failSave = false
				provider.available = 500
				store.mutate(t, func(v *accountRefillState) {
					v.Config.DailyMaxCount, v.Config.MaxUnitPrice = 50, 60
					v.ProviderRetryAt, v.DailyScheduleStatus.RetryAt = time.Time{}, time.Time{}
				})
				require.NoError(t, s.run(context.Background(), false))
				state, err = decodeAccountRefillState(store.raw)
				require.NoError(t, err)
				require.Len(t, state.Jobs, 3)
			}
		})
	}
}

func TestRefillDailyConcurrentWorkersAndPausedDelivery(t *testing.T) {
	s, base, provider, _ := failureRefillHarness(t)
	seedDailyRefill(t, base, time.Now().Truncate(time.Minute), 2)
	base.mutate(t, func(v *accountRefillState) { v.Config.PushEnabled = false })
	store := &refillTimingStore{raw: base.raw}
	s.store = store
	other := &AccountRefillService{store: store, provider: provider, encryptor: s.encryptor, accounts: s.accounts, groups: s.groups, loads: s.loads}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, worker := range []*AccountRefillService{s, other} {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- worker.run(context.Background(), false) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, state.Jobs, 2)
	require.Equal(t, 2, state.DailyScheduleStatus.LastQuantity)
}

func TestRefillDailyConfigurationLegacySaveAndMissedDays(t *testing.T) {
	s, store, _, _ := failureRefillHarness(t)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.False(t, refillDailyScheduleEnabled(state.Config))
	for _, daily := range []AccountRefillDailySchedule{{Time: "24:00", Quantity: 1}, {Time: "8:00", Quantity: 1}, {Time: "08:00", Quantity: 0}, {Time: "08:00", Quantity: 101}} {
		c := state.Config
		c.DailySchedule = &daily
		require.Error(t, c.Validate())
	}
	c := state.Config
	c.DailySchedule = &AccountRefillDailySchedule{Enabled: true, Time: "08:00", Quantity: 2}
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.DailyScheduleStatus.NextAt.After(time.Now()), "enabling never buys retrospectively")
	next := state.DailyScheduleStatus.NextAt
	c = state.Config
	c.DailySchedule = nil
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, refillDailyScheduleEnabled(state.Config))
	require.True(t, next.Equal(state.DailyScheduleStatus.NextAt))
	seedDailyRefill(t, store, time.Now().Truncate(time.Minute).Add(-72*time.Hour), 2)
	require.NoError(t, s.run(context.Background(), false))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, state.Jobs, 2, "missed days do not multiply purchases")
	require.WithinDuration(t, time.Now(), state.DailyScheduleStatus.LastAt, 24*time.Hour)
	for i := range state.Jobs {
		state.Jobs[i].Status, state.Jobs[i].SupplierStatus = "failed", "failed"
		markRefillFailureReplacement(&state, &state.Jobs[i])
		require.False(t, state.Jobs[i].ReplacementPending, "fixed daily quantity must not grow via normal replacement slots")
	}
}
