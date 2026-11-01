//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRefillForecastSwitchStopsOnlyProjectedDemand(t *testing.T) {
	now := time.Now()
	c := refillTestConfig()
	accounts := refillForecastAccounts(now)
	loads := refillForecastLoads()
	before := evaluateAccountRefill(c, accounts, loads, nil, now)
	require.Equal(t, refillForecastReason, before.Reason)
	flag := false
	c.ForecastEnabled = &flag
	after := evaluateAccountRefill(c, accounts, loads, nil, now)
	require.Zero(t, after.Needed)
	require.Zero(t, after.ForecastAccounts)
	require.Zero(t, after.ForecastCapacity)
	require.False(t, after.Accounts[0].ForecastLoss)
	require.Equal(t, before.Capacity, after.Capacity)
	require.Equal(t, before.InUse, after.InUse)
	require.Equal(t, before.Ready, after.Ready)
	loads[1].CurrentConcurrency, loads[2].CurrentConcurrency = 90, 90
	realDemand := evaluateAccountRefill(c, accounts, loads, nil, now)
	require.Positive(t, realDemand.Needed)
	require.Equal(t, "并发利用率超过目标", realDemand.Reason)
	c.MinReady = 3
	minimum := evaluateAccountRefill(c, accounts, refillForecastLoads(), nil, now)
	require.Positive(t, minimum.Needed)
	require.Equal(t, "健康储备不足", minimum.Reason)
	c.MinReady, c.MaxReady = 1, 2
	limited := evaluateAccountRefill(c, accounts, loads, nil, now)
	require.Zero(t, limited.Needed, "disabled forecasting must not relax the current stock cap")
}

func TestRefillForecastSwitchRequiresNewWindowWhenReenabled(t *testing.T) {
	now := time.Now()
	c := refillTestConfig()
	s := &AccountRefillService{}
	e := evaluateAccountRefill(c, refillForecastAccounts(now), refillForecastLoads(), nil, now)
	feedRefillForecast(s, c, 1, e, now, 30)
	require.True(t, s.forecastObservation(c, 1, e, now).Ready)
	disabled := false
	c.ForecastEnabled = &disabled
	observation := s.forecastObservation(c, 1, e, now)
	require.Equal(t, "disabled", observation.Status)
	require.False(t, observation.Ready)
	require.False(t, s.refillObservationReady(c, 1, e, now))
	s.recordCapacitySample(c, 1, e, now.Add(time.Second))
	c.ForecastEnabled = nil
	require.False(t, s.forecastObservation(c, 1, e, now.Add(time.Second)).Ready)
	feedRefillForecast(s, c, 1, e, now.Add(32*time.Second), 30)
	require.True(t, s.forecastObservation(c, 1, e, now.Add(32*time.Second)).Ready)
}

func TestRefillForecastSwitchPersistsAndLegacyClientsPreserveChoice(t *testing.T) {
	s, store, _, _ := refillHarness(t)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, refillForecastEnabled(state.Config), "old rules default to enabled")
	disabled := false
	c := state.Config
	c.Enabled, c.ForecastEnabled = false, &disabled
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.False(t, refillForecastEnabled(state.Config))
	c = state.Config
	c.ForecastEnabled = nil
	c.CooldownSeconds++
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.False(t, refillForecastEnabled(state.Config), "an unrelated legacy save keeps false")
	enabled := true
	c.ForecastEnabled = &enabled
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, refillForecastEnabled(state.Config))
}

func TestRefillForecastSwitchBlocksPurchaseDespiteEarlierReadyForecast(t *testing.T) {
	s, store, provider, accounts := refillHarness(t)
	now := time.Now()
	accounts.accounts = refillForecastAccounts(now)
	s.loads = refillTestLoads{refillForecastLoads()}
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	e := evaluateAccountRefill(state.Config, accounts.accounts, refillForecastLoads(), nil, now)
	feedRefillForecast(s, state.Config, state.Revision, e, now, 30)
	store.mutate(t, func(v *accountRefillState) {
		disabled := false
		v.Config.ForecastEnabled = &disabled
		v.Revision++
	})
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, provider.submitted)
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, state.Jobs)
}
