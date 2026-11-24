//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNativeExpiryAtomicRetentionSupplierEchoAndAutoPause(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	original := time.Now().Add(-15 * time.Minute).Truncate(time.Second).Unix()
	deadline, extra := service.ImportedAccountExpiryWithGrace(&original, nil, 30)
	extra[service.AccountDeliveryKeyExtraKey] = "legacy"
	extra[service.AccountDeliveryExpiryExtraKey] = original
	at := time.Unix(*deadline, 0)
	a := mustCreateAccount(t, client, &service.Account{Name: "native-expiry-round-trip", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Priority: 7, Schedulable: true, ExpiresAt: &at, AutoPauseOnExpired: true, Extra: extra})
	_, err := client.Account.UpdateOneID(a.ID).SetExpiresAt(at).SetAutoPauseOnExpired(true).Save(ctx)
	require.NoError(t, err)
	stale, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.AccountImportExpiryGraceExtraKey: 3600}))
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{
		"refill_job_id": "verified-job", "refill_supplier_id": "rvc_fixture",
		"refill_plan": "pro20x", "import_source": "muze_api",
	}))
	stale.Name = "unrelated edit"
	stale.Extra = map[string]any{"public": true, service.AccountDeliveryKeyExtraKey: "forged"}
	require.NoError(t, repo.Update(ctx, stale))
	loaded, err := repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, original+3600, loaded.ExpiresAt.Unix())
	require.Equal(t, "legacy", loaded.Extra[service.AccountDeliveryKeyExtraKey])
	require.Equal(t, true, loaded.Extra["public"])
	require.Equal(t, 3600, loaded.ExpiryGraceSeconds())
	require.Equal(t, "verified-job", loaded.Extra["refill_job_id"])
	require.Equal(t, "rvc_fixture", loaded.Extra["refill_supplier_id"])
	require.Equal(t, "pro20x", loaded.Extra["refill_plan"])
	require.Equal(t, "muze_api", loaded.Extra["import_source"])
	var sqlDeadline int64
	query := "SELECT extract(epoch from " + aliasedAccountSchedulingExpirySQL("a") + ")::bigint FROM accounts a WHERE a.id=$1"
	require.NoError(t, scanSingleRow(ctx, tx, query, []any{a.ID}, &sqlDeadline))
	require.Equal(t, loaded.ExpiresAt.Unix(), sqlDeadline)
	_, err = repo.AutoPauseExpiredAccounts(ctx, time.Now())
	require.NoError(t, err)
	loaded, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.True(t, loaded.Schedulable)
	// The administrator's exact native date wins, even when the supplier echoes
	// its original timestamp after this edit or a source sync arrives later.
	manual := time.Now().Add(-time.Minute).Truncate(time.Second).Unix()
	require.NoError(t, repo.Update(service.WithAccountExpiryWrite(ctx, &manual), loaded))
	supplier := adminaccess.WithLegacyKey(ctx)
	require.NoError(t, repo.Update(service.WithAccountExpiryWrite(supplier, &original), loaded))
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.AccountSourceExpiryExtraKey: original + 60}))
	loaded, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, manual, loaded.ExpiresAt.Unix())
	require.Equal(t, original, service.AccountForAdminRead(supplier, loaded).ExpiresAt.Unix())
	require.NoError(t, scanSingleRow(ctx, tx, query, []any{a.ID}, &sqlDeadline))
	require.Equal(t, manual, sqlDeadline)
	_, err = repo.AutoPauseExpiredAccounts(ctx, time.Now())
	require.NoError(t, err)
	loaded, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.False(t, loaded.Schedulable)
	require.NoError(t, repo.UpdateExtra(ctx, a.ID, map[string]any{service.AccountImportExpiryGraceExtraKey: 7200}))
	loaded, err = repo.GetByID(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, original+7200, loaded.ExpiresAt.Unix())
	require.False(t, loaded.Schedulable, "retention must not reactivate a paused account")
}

func TestMuzePushCandidateQueryScopesGroupSubjectAndTime(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := newAccountRepositoryWithSQL(client, tx, nil)
	group := mustCreateGroup(t, client, &service.Group{Name: "muze-push-target", Platform: service.PlatformOpenAI})
	other := mustCreateGroup(t, client, &service.Group{Name: "muze-push-other", Platform: service.PlatformOpenAI})
	since := time.Now().Add(-time.Minute)
	create := func(name, subject, status string, groupID int64) int64 {
		a := mustCreateAccount(t, client, &service.Account{Name: name, Platform: service.PlatformOpenAI, Status: status, Credentials: map[string]any{"chatgpt_account_id": subject, "email": "delivery@example.test"}})
		mustBindAccountToGroup(t, client, a.ID, groupID, 1)
		return a.ID
	}
	id := create("paused-push", "shared-subject", "inactive", group.ID)
	create("wrong-group", "shared-subject", "active", other.ID)
	create("wrong-subject", "other", "active", group.ID)
	find := func(at time.Time) []*service.Account {
		rows, err := repo.FindRefillPushCandidates(ctx, []int64{group.ID}, "shared-subject", at)
		require.NoError(t, err)
		return rows
	}
	matches := find(since)
	require.Len(t, matches, 1)
	require.Equal(t, id, matches[0].ID)
	require.Equal(t, "inactive", matches[0].Status)
	require.Empty(t, find(time.Now().Add(time.Hour)))
	create("duplicate-identity", "shared-subject", "active", group.ID)
	require.Len(t, find(since), 2)
}
