//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/stretchr/testify/require"
)

func TestAccountHealthDurationOverridesLeaseAndSurvivesReload(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	minutes := 100
	for _, kind := range []string{"native", "legacy", "no policy", "equal deadline"} {
		t.Run(kind, func(t *testing.T) {
			a := nativeExpiryFixture()
			a.AutoPauseOnExpired = false
			source, _ := a.SourceExpiresAt()
			if kind == "legacy" {
				delete(a.Extra, AccountNativeExpiryExtraKey)
			}
			if kind == "no policy" {
				a.Extra = map[string]any{AccountSourceExpiryExtraKey: source.Unix(), AccountSourceExpiryOriginExtraKey: "supplier_api"}
			}
			if kind == "equal deadline" {
				at := now.Add(100 * time.Minute)
				a.ExpiresAt = &at
			}
			current := *a
			input := &UpdateAccountInput{HealthDurationMinutes: &minutes}
			ctx, prepared, err := prepareAccountHealthDuration(context.Background(), input, now)
			require.NoError(t, err)
			require.Nil(t, input.ExpiresAt, "do not mutate the caller's request")
			ctx = WithAccountExpiryWrite(ctx, prepared.ExpiresAt)
			ctx = withAccountAutoPauseWrite(ctx, prepared.AutoPauseOnExpired)
			a.AutoPauseOnExpired = *prepared.AutoPauseOnExpired
			ApplyAccountExpiryWrite(ctx, a, *prepared.ExpiresAt)
			// Persistence merges metadata from the locked row before reapplying this edit.
			MergeLockedAccountExpiry(ctx, a, &current)
			require.Equal(t, now.Add(100*time.Minute).Unix(), a.ExpiresAt.Unix())
			require.Equal(t, true, a.Extra[AccountExpiryManualExtraKey])
			require.True(t, a.isSchedulableAt(a.ExpiresAt.Add(-time.Second)))
			require.False(t, a.isSchedulableAt(*a.ExpiresAt), "the deadline blocks admission without waiting for maintenance")
			require.False(t, a.isSchedulableAt(a.ExpiresAt.Add(time.Minute)))
			// Reloading after a restart retains the absolute deadline, never another 100 minutes.
			raw, err := json.Marshal(a)
			require.NoError(t, err)
			var reloaded Account
			require.NoError(t, json.Unmarshal(raw, &reloaded))
			require.Equal(t, a.ExpiresAt.Unix(), reloaded.ExpiresAt.Unix())
			require.False(t, reloaded.isSchedulableAt(*a.ExpiresAt))
			ApplyAccountExpiryExtra(&reloaded, map[string]any{AccountSourceExpiryExtraKey: source.Add(time.Hour).Unix()})
			require.Equal(t, a.ExpiresAt.Unix(), reloaded.ExpiresAt.Unix(), "supplier metadata must not reset the manual deadline")
			if kind == "native" {
				ApplyAccountExpiryWrite(adminaccess.WithLegacyKey(context.Background()), &reloaded, source.Add(2*time.Hour).Unix())
				require.Equal(t, a.ExpiresAt.Unix(), reloaded.ExpiresAt.Unix(), "supplier delivery renewal cannot overwrite the manual deadline")
			}
			stale := current
			MergeLockedAccountExpiry(context.Background(), &stale, &reloaded)
			require.Equal(t, reloaded.ExpiresAt, stale.ExpiresAt, "an unrelated stale credential write cannot restore the old deadline")
			require.True(t, stale.AutoPauseOnExpired, "a stale write cannot undo pause on expiry")
			require.False(t, stale.isSchedulableAt(*reloaded.ExpiresAt))
		})
	}
}

func TestUpdateAccountHealthDuration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		a := nativeExpiryFixture()
		a.ID, a.Schedulable, a.AutoPauseOnExpired = 9, enabled, false
		repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{a.ID: a}}
		s := &adminServiceImpl{accountRepo: repo}
		minutes, start := 100, time.Now()
		updated, err := s.UpdateAccount(context.Background(), a.ID, &UpdateAccountInput{HealthDurationMinutes: &minutes})
		require.NoError(t, err)
		require.WithinDuration(t, start.Add(100*time.Minute), *updated.ExpiresAt, time.Second)
		require.True(t, updated.AutoPauseOnExpired)
		require.Equal(t, enabled, updated.Schedulable, "setting a deadline does not reactivate paused accounts")
		require.Equal(t, true, updated.Extra[AccountExpiryManualExtraKey])
		deadline := updated.ExpiresAt
		updated, err = s.UpdateAccount(context.Background(), a.ID, &UpdateAccountInput{Name: "renamed"})
		require.NoError(t, err)
		require.Equal(t, deadline, updated.ExpiresAt)
	}
}

func TestAccountHealthDurationValidation(t *testing.T) {
	now, minutes, zero, off := time.Now(), 100, int64(0), false
	for _, invalid := range []int{-1, 0, 525601} {
		_, _, err := prepareAccountHealthDuration(context.Background(), &UpdateAccountInput{HealthDurationMinutes: &invalid}, now)
		require.Error(t, err)
	}
	for _, in := range []*UpdateAccountInput{
		{HealthDurationMinutes: &minutes, ExpiresAt: &zero},
		{HealthDurationMinutes: &minutes, ExpiryGraceMinutes: &minutes},
		{HealthDurationMinutes: &minutes, AutoPauseOnExpired: &off},
	} {
		_, _, err := prepareAccountHealthDuration(context.Background(), in, now)
		require.Error(t, err)
	}
	ctx := adminaccess.WithPrincipal(context.Background(), &adminaccess.Principal{KeyID: "restricted"})
	_, _, err := prepareAccountHealthDuration(ctx, &UpdateAccountInput{HealthDurationMinutes: &minutes}, now)
	require.Error(t, err)
	for _, valid := range []int{1, 100, 525600} {
		_, in, err := prepareAccountHealthDuration(context.Background(), &UpdateAccountInput{HealthDurationMinutes: &valid}, now)
		require.NoError(t, err)
		require.Equal(t, now.Add(time.Duration(valid)*time.Minute).Unix(), *in.ExpiresAt)
	}
}

func TestAccountExpiryPauseRequiresExplicitWrite(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		current := nativeExpiryFixture()
		incoming := *current
		incoming.AutoPauseOnExpired = !current.AutoPauseOnExpired
		ctx := withAccountAutoPauseWrite(context.Background(), &enabled)
		MergeLockedAccountExpiry(ctx, &incoming, current)
		require.Equal(t, enabled, incoming.AutoPauseOnExpired, "an explicit administrator choice still takes effect")
	}
}
