//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestImportExpiryGraceConsistentAcrossSQLSchedulingAndAutoPause(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	group := mustCreateGroup(t, client, &service.Group{Name: "native-grace", Platform: service.PlatformOpenAI})
	now := time.Now().Truncate(time.Second)
	expiry := now.Add(-30 * time.Minute)
	ids := []int64{}
	for _, fixture := range []struct {
		name    string
		grace   any
		kind    string
		allowed bool
	}{
		{"grace", 3600, service.AccountTypeOAuth, true},
		{"no-grace", nil, service.AccountTypeOAuth, false},
		{"malformed", "not-seconds", service.AccountTypeOAuth, false},
		{"oversized", 99999, service.AccountTypeOAuth, false},
		{"huge", "99999999999999999999", service.AccountTypeOAuth, false},
		{"wrong-kind", 3600, service.AccountTypeAPIKey, false},
	} {
		a := mustCreateAccount(t, client, &service.Account{Name: fixture.name, Platform: service.PlatformOpenAI,
			Type: fixture.kind, Schedulable: true, Priority: 7, ExpiresAt: &expiry,
			AutoPauseOnExpired: true, Extra: map[string]any{service.AccountImportExpiryGraceExtraKey: fixture.grace}})
		_, err := client.Account.UpdateOneID(a.ID).SetExpiresAt(expiry).SetAutoPauseOnExpired(true).Save(ctx)
		require.NoError(t, err)
		require.NoError(t, repo.BindGroups(ctx, a.ID, []int64{group.ID}))
		ids = append(ids, a.ID)
		loaded, err := repo.GetByID(ctx, a.ID)
		require.NoError(t, err)
		require.Equal(t, fixture.allowed, loaded.IsSchedulable())
	}
	accounts, err := repo.ListSchedulableByGroupIDAndPlatform(ctx, group.ID, service.PlatformOpenAI)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, ids[0], accounts[0].ID)
	require.Equal(t, 7, accounts[0].Priority)
	require.True(t, accounts[0].ExpiresAt.Equal(expiry))
	loads, err := repo.ListSchedulableAccountLoads(ctx)
	require.NoError(t, err)
	for _, id := range ids {
		found := false
		for _, load := range loads {
			if load.ID == id {
				found = true
			}
		}
		require.Equal(t, id == ids[0], found)
	}
	capacity, err := repo.ListSchedulableCapacityByGroupIDs(ctx, []int64{group.ID})
	require.NoError(t, err)
	require.Len(t, capacity, 1)
	require.Equal(t, ids[0], capacity[0].AccountID)
	// The group's capacity/availability uses the same deadline as the actual scheduler.
	var available int
	require.NoError(t, scanSingleRow(ctx, tx, "SELECT COUNT(*) FROM accounts a JOIN account_groups ag ON ag.account_id=a.id WHERE ag.group_id=$1 AND "+groupAccountAvailableSQL, []any{group.ID}, &available))
	require.Equal(t, 1, available)
	_, err = repo.AutoPauseExpiredAccounts(ctx, now)
	require.NoError(t, err)
	kept, err := repo.GetByID(ctx, ids[0])
	require.NoError(t, err)
	require.True(t, kept.Schedulable)
	_, err = repo.AutoPauseExpiredAccounts(ctx, expiry.Add(time.Hour))
	require.NoError(t, err)
	paused, err := repo.GetByID(ctx, ids[0])
	require.NoError(t, err)
	require.False(t, paused.Schedulable)
	require.Equal(t, 7, paused.Priority)
	require.True(t, paused.ExpiresAt.Equal(expiry))
}

func TestLeaseExpirySurvivesNativeManualRenewalAndGraceUpdates(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	now := time.Now().Truncate(time.Second)
	native := now.AddDate(1, 0, 0)
	lease := now.Add(-30 * time.Minute)
	a := mustCreateAccount(t, client, &service.Account{Name: "lease-grace", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Schedulable: true, Priority: 7, ExpiresAt: &native, AutoPauseOnExpired: true, Extra: map[string]any{"import_original_expires_at": lease.Unix(), "import_expiry_offset_seconds": 3600}})
	_, err := client.Account.UpdateOneID(a.ID).SetExpiresAt(native).SetAutoPauseOnExpired(true).Save(ctx)
	require.NoError(t, err)
	assertDeadline := func(expected time.Time) {
		t.Helper()
		loaded, e := repo.GetByID(ctx, a.ID)
		require.NoError(t, e)
		require.Equal(t, expected.Unix(), loaded.SchedulingExpiresAt().Unix())
		var actual int64
		query := "SELECT extract(epoch from " + aliasedAccountSchedulingExpirySQL("a") + ")::bigint FROM accounts a WHERE a.id=$1"
		require.NoError(t, scanSingleRow(ctx, tx, query, []any{a.ID}, &actual))
		require.Equal(t, expected.Unix(), actual)
		if loaded.HasNativeExpiry() {
			require.Equal(t, expected.Unix(), loaded.ExpiresAt.Unix())
		} else {
			require.True(t, loaded.ExpiresAt.Equal(native))
		}
		require.Equal(t, 7, loaded.Priority)
	}
	assertDeadline(lease.Add(time.Hour))
	_, err = repo.AutoPauseExpiredAccounts(ctx, now)
	require.NoError(t, err)
	loaded, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, loaded.Schedulable)
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.AccountImportExpiryGraceExtraKey: 0}))
	assertDeadline(lease)
	// A later native date must not bypass the source lease in candidate queries.
	eligible, err := client.Account.Query().Where(notExpiredPredicate(now)).All(ctx)
	require.NoError(t, err)
	for _, candidate := range eligible {
		require.NotEqual(t, a.ID, candidate.ID)
	}
	_, err = repo.AutoPauseExpiredAccounts(ctx, now)
	require.NoError(t, err)
	loaded, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.False(t, loaded.Schedulable)
	newLease := now.Add(time.Hour)
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.AccountSourceExpiryExtraKey: newLease.Unix(), service.AccountImportExpiryGraceExtraKey: 7200}))
	assertDeadline(newLease.Add(2 * time.Hour))
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.AccountSourceExpiryExtraKey: "garbage"}))
	assertDeadline(lease.Add(2 * time.Hour))
}
