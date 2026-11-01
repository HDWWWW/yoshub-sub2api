//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func feedRefillCapacity(s *AccountRefillService, c AccountRefillConfig, revision int64, until time.Time, seconds int) {
	for i := seconds; i >= 0; i-- {
		s.recordCapacitySample(c, revision, AccountRefillEvaluation{Capacity: 100, InUse: 85}, until.Add(-time.Duration(i)*time.Second))
	}
}

func TestRefillCapacityContinuousObservation(t *testing.T) {
	s := &AccountRefillService{}
	c := refillTestConfig()
	c.ConcurrencySustainSeconds = 30
	now := time.Now()
	feedRefillCapacity(s, c, 1, now, 29)
	require.False(t, s.capacityObservation(c, 1, now).Ready)
	s.recordCapacitySample(c, 1, AccountRefillEvaluation{Capacity: 100, InUse: 85}, now.Add(time.Second))
	require.True(t, s.capacityObservation(c, 1, now.Add(time.Second)).Ready, "85 percent is inclusive")
	s.recordCapacitySample(c, 1, AccountRefillEvaluation{Capacity: 100, InUse: 84}, now.Add(2*time.Second))
	require.Zero(t, s.capacityObservation(c, 1, now.Add(2*time.Second)).SustainedSeconds)
	s.recordCapacitySample(c, 1, AccountRefillEvaluation{Capacity: 100, InUse: 90}, now.Add(3*time.Second))
	require.False(t, s.capacityObservation(c, 1, now.Add(3*time.Second)).Ready)
	feedRefillCapacity(s, c, 1, now.Add(40*time.Second), 30)
	require.True(t, s.capacityObservation(c, 1, now.Add(40*time.Second)).Ready)
	require.False(t, s.capacityObservation(c, 1, now.Add(43*time.Second)).Ready, "stale samples cannot authorize purchases")
	s.recordCapacitySample(c, 1, AccountRefillEvaluation{Capacity: 100, InUse: 90}, now.Add(43*time.Second))
	require.Zero(t, s.capacityObservation(c, 1, now.Add(43*time.Second)).SustainedSeconds, "missed samples reset the window")
	c.ConcurrencySustainSeconds = 60
	require.False(t, s.capacityObservation(c, 1, now.Add(43*time.Second)).Ready)
	feedRefillCapacity(s, c, 2, now.Add(110*time.Second), 60)
	require.True(t, s.capacityObservation(c, 2, now.Add(110*time.Second)).Ready)
	require.False(t, s.capacityObservation(c, 3, now.Add(110*time.Second)).Ready, "config changes require fresh samples")
	c.MonitorEnabled = false
	require.Equal(t, "disabled", s.capacityObservation(c, 2, now.Add(110*time.Second)).Status)
	s.resetCapacityObservation()
	c.MonitorEnabled = true
	require.False(t, s.capacityObservation(c, 2, now.Add(110*time.Second)).Ready)
}

type failingRefillLoads struct{}

func (failingRefillLoads) GetAccountsLoadBatch(context.Context, []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	return nil, errors.New("unavailable")
}

func TestRefillSamplerIndependentOfCheckAndPurchaseSwitch(t *testing.T) {
	s, store, p, a := refillHarness(t)
	a.accounts = []Account{refillTestAccount(time.Now().Add(time.Hour))}
	s.loads = refillTestLoads{map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 60}}}
	store.mutate(t, func(v *accountRefillState) { v.Config.Enabled = false; v.NextCheckAt = time.Now().Add(time.Hour) })
	s.sampleCapacity(context.Background())
	v, err := s.View(context.Background())
	require.NoError(t, err)
	require.Equal(t, "observing", v.CapacityObservation.Status)
	require.Empty(t, p.submitted)
	s.loads = failingRefillLoads{}
	s.sampleCapacity(context.Background())
	v, err = s.View(context.Background())
	require.NoError(t, err)
	require.Equal(t, "waiting", v.CapacityObservation.Status)
}

func TestRefillSamplingRejectsEndedBurstAndOldPersistedDemand(t *testing.T) {
	s, store, p, a := refillHarness(t)
	a.accounts = []Account{refillTestAccount(time.Now().Add(time.Hour))}
	loads := map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 60}}
	s.loads = refillTestLoads{loads}
	store.mutate(t, func(v *accountRefillState) { old := time.Now().Add(-time.Hour); v.DemandSince = &old })
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, p.submitted, "old coarse timestamps do not count as samples")
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	feedRefillCapacity(s, state.Config, state.Revision, time.Now(), 30)
	loads[1].CurrentConcurrency = 0
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, p.submitted)
}

func TestRefillDefaultsAndCustomGrace(t *testing.T) {
	state, err := decodeAccountRefillState(`{"config":{"enabled":false,"lead_minutes":5}}`)
	require.NoError(t, err)
	require.Equal(t, 30, state.Config.ConcurrencySustainSeconds)
	require.Equal(t, 60, state.Config.ImportExpiryGraceMinutes)
	require.Equal(t, 5, state.Config.LeadMinutes)
	for _, minutes := range []int{0, 45, 120, 1440} {
		expiry := int64(2000000000)
		native, extra := ImportedAccountExpiryWithGrace(&expiry, map[string]any{"keep": true}, minutes)
		require.Equal(t, expiry+int64(minutes*60), *native)
		require.Equal(t, minutes*60, extra[AccountImportExpiryGraceExtraKey])
		require.Equal(t, true, extra["keep"])
	}
	for _, seconds := range []int{0, -1, 3601} {
		c := refillTestConfig()
		c.ConcurrencySustainSeconds = seconds
		require.Error(t, c.Validate())
	}
	for _, minutes := range []int{-1, 1441} {
		c := refillTestConfig()
		c.ImportExpiryGraceMinutes = minutes
		require.Error(t, c.Validate())
	}
	s, store, _, _ := refillHarness(t)
	store.mutate(t, func(v *accountRefillState) { v.Config.ImportExpiryGraceMinutes = 0 })
	require.Zero(t, s.ImportExpiryGraceMinutes(context.Background()))
	var wire map[string]any
	require.NoError(t, json.Unmarshal([]byte(store.raw), &wire))
}

func TestExpiryGraceUpdatePreservesNativeFields(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	a := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Priority: 7, Concurrency: 150,
		GroupIDs: []int64{2, 5}, ExpiresAt: &expiry, Status: StatusActive, Schedulable: false, Extra: map[string]any{"keep": true}}
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{1: a}}
	svc := &adminServiceImpl{accountRepo: repo}
	for _, minutes := range []int{120, 0} {
		updated, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{ExpiryGraceMinutes: &minutes})
		require.NoError(t, err)
		require.Equal(t, minutes*60, updated.ExpiryGraceSeconds())
		require.Equal(t, expiry, *updated.ExpiresAt)
		require.Equal(t, 7, updated.Priority)
		require.Equal(t, 150, updated.Concurrency)
		require.Equal(t, []int64{2, 5}, updated.GroupIDs)
		require.False(t, updated.Schedulable)
		require.Equal(t, true, updated.Extra["keep"])
	}
	bad := 1441
	_, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{ExpiryGraceMinutes: &bad})
	require.Error(t, err)
}

func TestRefillUsesRecordedLeaseAfterManualNativeExpiryEdit(t *testing.T) {
	now := time.Now()
	a := refillTestAccount(now.AddDate(1, 0, 0))
	original := now.Add(3 * time.Minute).Unix()
	a.Extra = map[string]any{"import_original_expires_at": original, "import_expiry_offset_seconds": 3600}
	c := refillTestConfig()
	c.LeadMinutes = 5
	e := evaluateAccountRefill(c, []Account{a}, nil, nil, now)
	require.Equal(t, "expiring", e.Accounts[0].State)
	require.Equal(t, original, e.Accounts[0].SourceExpiresAt.Unix())
	require.Equal(t, "import", e.Accounts[0].ExpirySource)
	require.Equal(t, 1, e.Needed)
	require.Equal(t, now.AddDate(1, 0, 0), *a.ExpiresAt)
	a.Extra[AccountSourceExpiryExtraKey] = now.Add(time.Hour).Unix()
	a.Extra[AccountSourceExpiryOriginExtraKey] = "supplier_api"
	e = evaluateAccountRefill(c, []Account{a}, nil, nil, now)
	require.Equal(t, "supplier_api", e.Accounts[0].ExpirySource)
	require.Zero(t, e.Needed)
	require.Empty(t, NativeAccountExtra(a.Extra))
}
