//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/stretchr/testify/require"
)

func TestManualSchedulingOverridesExpiryAndRestoresAutomatic(t *testing.T) {
	now := time.Now()
	soon, later := now.Add(time.Hour), now.Add(2*time.Hour)
	accounts := []Account{{ID: 1, Priority: 9, ExpiresAt: &soon}, {ID: 2, Priority: 0, ExpiresAt: &later, Extra: map[string]any{AccountManualPriorityExtraKey: 0}}}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
	result := BuildAccountSchedulingPreview(accounts, cfg, now)
	require.EqualValues(t, 2, result.Items[0].AccountID)
	require.Equal(t, 2, result.Items[0].AutomaticPriority)
	require.Zero(t, result.Items[0].EffectivePriority)
	require.Zero(t, accounts[0].schedulingPriority, "shared candidates are never mutated")
	accounts[1].Extra = applyManualSchedulingPriority(accounts[1].Extra, "auto", nil)
	result = BuildAccountSchedulingPreview(accounts, cfg, now)
	require.EqualValues(t, 1, result.Items[0].AccountID)
	require.Nil(t, result.Items[1].ManualPriority)
	require.Equal(t, 2, result.Items[1].EffectivePriority)
}

func TestManualSchedulingUpdateAndNativeImportCompatibility(t *testing.T) {
	repo := &updateAccountOveragesRepoStub{account: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Priority: 7}}
	svc := &adminServiceImpl{accountRepo: repo}
	zero := 0
	a, err := svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Priority: &zero, SchedulingPriorityMode: "manual"})
	require.NoError(t, err)
	require.Zero(t, a.SchedulingPriority())
	other := 90
	a, err = svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Priority: &other, Extra: map[string]any{AccountManualPriorityExtraKey: 999, "supplier": true}})
	require.NoError(t, err)
	require.Equal(t, 90, a.Priority)
	require.Zero(t, a.SchedulingPriority())
	require.NotContains(t, NativeAccountExtra(a.Extra), AccountManualPriorityExtraKey)
	a, err = svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{SchedulingPriorityMode: "auto"})
	require.NoError(t, err)
	require.Equal(t, 90, a.SchedulingPriority())
	_, err = svc.UpdateAccount(adminaccess.WithPrincipal(context.Background(), &adminaccess.Principal{}), 1, &UpdateAccountInput{Priority: &zero, SchedulingPriorityMode: "manual"})
	require.Error(t, err)
	for _, bad := range []int{-1, 10000} {
		_, err = svc.UpdateAccount(context.Background(), 1, &UpdateAccountInput{Priority: &bad, SchedulingPriorityMode: "manual"})
		require.Error(t, err)
	}
}

func TestManualSchedulingBulkWritesPriorityAndMetadataTogether(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDsAccounts: []*Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}}}
	svc := &adminServiceImpl{accountRepo: repo}
	priority := 0
	_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Priority: &priority, SchedulingPriorityMode: "manual"})
	require.NoError(t, err)
	require.Equal(t, priority, *repo.lastBulkUpdate.Priority)
	require.Equal(t, priority, repo.lastBulkUpdate.Extra[AccountManualPriorityExtraKey])
	_, err = svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, SchedulingPriorityMode: "auto"})
	require.NoError(t, err)
	require.Contains(t, repo.lastBulkUpdate.Extra, AccountManualPriorityExtraKey)
	require.Nil(t, repo.lastBulkUpdate.Extra[AccountManualPriorityExtraKey])
}

func TestSchedulingMetadataSeparatesSupplierAndEffectiveDeadline(t *testing.T) {
	supplier := time.Date(2026, 10, 31, 19, 5, 32, 0, time.UTC)
	a := &Account{ID: 38, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ExpiresAt: &supplier, Extra: map[string]any{
		AccountSourceExpiryExtraKey: supplier.Unix(), AccountSourceExpiryOriginExtraKey: "supplier_api", AccountImportExpiryGraceExtraKey: 1800,
	}}
	m := SchedulingMetadata(a)
	require.Equal(t, supplier, *m.SourceExpiresAt)
	require.Equal(t, supplier.Add(30*time.Minute), *m.EffectiveExpiresAt)
	require.Equal(t, supplier, *a.ExpiresAt)
	require.Equal(t, 30, m.GraceMinutes)
	a.Extra = nil
	require.Nil(t, SchedulingMetadata(a).SourceExpiresAt, "local expiry is not supplier evidence")
}

func TestManualPriorityReachesAstraSchedulers(t *testing.T) {
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	for _, advanced := range []bool{false, true} {
		for _, loadBatch := range []bool{false, true} {
			accounts := astraPriorityTestAccounts()
			accounts[1].Extra = map[string]any{AccountManualPriorityExtraKey: 0}
			svc := newAstraPriorityTestService(accounts, advanced, true, loadBatch, schedulerTestConcurrencyCache{})
			svc.cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
			selection, _ := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
			require.Equal(t, accounts[1].ID, selection.Account.ID, "advanced=%v loadBatch=%v", advanced, loadBatch)
		}
	}
}
