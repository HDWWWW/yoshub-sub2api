//go:build unit

package service

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRefillMultipleDailySchedulesReserveIndependentPlansOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s, store, provider, _ := failureRefillHarness(t)
		due := time.Now().Add(time.Minute)
		seedDailyRefill(t, store, due, 1)
		first, second := uuid.NewString(), uuid.NewString()
		store.mutate(t, func(v *accountRefillState) {
			v.Config.DailySchedule = nil
			v.Config.DailySchedules = []AccountRefillDailySchedule{
				{ID: first, Enabled: true, Time: "08:01", Quantity: 2, Plan: "pro5x"},
				{ID: second, Enabled: true, Time: "08:01", Quantity: 1, Plan: "pro20x"},
			}
			v.DailyScheduleStatuses = map[string]AccountRefillDailyScheduleStatus{first: {NextAt: due}, second: {NextAt: due}}
		})
		time.Sleep(time.Minute)
		for range 8 {
			require.NoError(t, s.run(ctx, false))
		}
		state, err := decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.Len(t, state.Jobs, 3)
		require.Len(t, provider.orders, 3)
		require.ElementsMatch(t, []string{"pro5x", "pro5x", "pro20x"}, []string{state.Jobs[0].Plan, state.Jobs[1].Plan, state.Jobs[2].Plan})
		require.NotEqual(t, state.DailyScheduleStatuses[first].LastBatchID, state.DailyScheduleStatuses[second].LastBatchID)
		require.Equal(t, 0, state.RotationCursor, "explicit plans do not consume the shared rotation")
		require.Equal(t, 3, state.Usage["2000-01-01"].Count)
		// Reordering, changing the time/plan and restarting cannot replay a receipt.
		c := state.Config
		c.DailySchedules[0], c.DailySchedules[1] = c.DailySchedules[1], c.DailySchedules[0]
		c.DailySchedules[0].Time, c.DailySchedules[0].Plan = "09:00", "pro5x"
		require.NoError(t, s.Configure(ctx, c, "", state.Revision))
		state, err = decodeAccountRefillState(store.raw)
		require.NoError(t, err)
		require.Equal(t, "2000-01-02 09:00", state.DailyScheduleStatuses[second].NextAt.In(refillTimezone).Format("2006-01-02 15:04"))
		s = &AccountRefillService{store: store, provider: provider, encryptor: s.encryptor, accounts: s.accounts, groups: s.groups, loads: s.loads}
		require.NoError(t, s.run(ctx, false))
		require.Len(t, provider.orders, 3)
	})
}

func TestRefillDailyScheduleMigrationAndValidation(t *testing.T) {
	s, store, _, _ := failureRefillHarness(t)
	due := time.Now().Add(-time.Minute).Truncate(time.Minute)
	seedDailyRefill(t, store, due, 2)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	c := state.Config
	c.DailySchedules = []AccountRefillDailySchedule{{ID: "legacy", Enabled: true, Time: c.DailySchedule.Time, Quantity: 2}}
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.DailyScheduleStatuses["legacy"].NextAt.Equal(due), "migration preserves an already due task")
	require.Nil(t, state.Config.DailySchedule)
	for _, change := range []func(*AccountRefillConfig){
		func(c *AccountRefillConfig) { c.DailySchedules[0].Plan = "unknown" },
		func(c *AccountRefillConfig) { c.DailySchedules[0].ID = "" },
		func(c *AccountRefillConfig) { c.DailySchedules = append(c.DailySchedules, c.DailySchedules[0]) },
		func(c *AccountRefillConfig) {
			for range 20 {
				c.DailySchedules = append(c.DailySchedules, AccountRefillDailySchedule{ID: uuid.NewString(), Time: "08:00", Quantity: 1})
			}
		},
	} {
		copy := state.Config
		copy.DailySchedules = append([]AccountRefillDailySchedule{}, state.Config.DailySchedules...)
		change(&copy)
		require.Error(t, copy.Validate())
	}
	c = state.Config
	c.DailySchedules = nil
	c.DailySchedule = &AccountRefillDailySchedule{Time: "09:00", Quantity: 1}
	require.Error(t, s.Configure(context.Background(), c, "", state.Revision), "stale clients cannot replace the task list")
	c.DailySchedule = nil
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Len(t, state.Config.DailySchedules, 1)
	c = state.Config
	c.DailySchedules = []AccountRefillDailySchedule{}
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.NotNil(t, state.Config.DailySchedules, "an empty list explicitly disables all tasks")
	require.False(t, refillDailyScheduleEnabled(state.Config))
}
