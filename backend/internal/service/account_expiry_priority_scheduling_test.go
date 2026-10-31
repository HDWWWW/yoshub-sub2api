//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestExpiryPrioritiesReachLegacySchedulerAndFallback(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	groupID := int64(58100)
	for _, loadBatch := range []bool{true, false} {
		for _, blocked := range []bool{false, true} {
			accounts := newLegacySchedulerDecisionTestAccounts(groupID, true)
			soon, later := time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
			accounts[0].ExpiresAt, accounts[1].ExpiresAt = &later, &soon
			accounts[0].Priority, accounts[1].Priority = 1, 50
			accounts[1].AutoPauseOnExpired = true
			svc := newLegacySchedulerDecisionTestService(accounts, loadBatch, schedulerTestConcurrencyCache{})
			svc.cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
			var excluded map[int64]struct{}
			want := accounts[1].ID
			if blocked {
				excluded = map[int64]struct{}{accounts[1].ID: {}}
				want = accounts[0].ID
			}
			selection, _, err := svc.SelectAccountWithSchedulerForCapability(
				context.Background(), &groupID, "", "", "gpt-5.1", excluded,
				OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true)
			require.NoError(t, err)
			require.NotNil(t, selection)
			releaseLegacySchedulerDecisionSelection(selection)
			require.Equal(t, 1, accounts[0].Priority)
			require.Equal(t, 50, accounts[1].Priority)
			require.Equal(t, want, selection.Account.ID, "loadBatch=%v excluded=%v", loadBatch, blocked)
		}
	}
}

func TestAstraExpiryRankingIsInternalAcrossSchedulerModes(t *testing.T) {
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	for _, advanced := range []bool{false, true} {
		for _, loadBatch := range []bool{false, true} {
			accounts := astraPriorityTestAccounts()
			accounts[0].Priority, accounts[1].Priority = 90, 1
			svc := newAstraPriorityTestService(accounts, advanced, true, loadBatch, schedulerTestConcurrencyCache{})
			svc.cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
			bindAstraPriorityTestSession(t, svc, accounts[1].ID)
			selection, _ := selectAstraPriorityTestAccount(t, svc, "gpt-6-astra", "", false, nil)
			require.Equal(t, accounts[0].ID, selection.Account.ID)
			require.Equal(t, 90, selection.Account.Priority)
			require.Equal(t, 90, accounts[0].Priority)
			require.Zero(t, accounts[0].schedulingPriority)
		}
	}
}

func TestExpiryPrioritySurvivesFreshSnapshotReadWithoutMutatingCache(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	groupID := int64(58100)
	for _, loadBatch := range []bool{false, true} {
		accounts := newLegacySchedulerDecisionTestAccounts(groupID, true)
		soon, later := time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
		accounts[0].ExpiresAt, accounts[1].ExpiresAt = &later, &soon
		accounts[0].Priority, accounts[1].Priority = 1, 50
		svc := newLegacySchedulerDecisionTestService(accounts, loadBatch, schedulerTestConcurrencyCache{})
		svc.cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
		cache := &snapshotHydrationCache{snapshot: []*Account{&accounts[0], &accounts[1]}, accounts: map[int64]*Account{accounts[0].ID: &accounts[0], accounts[1].ID: &accounts[1]}}
		svc.schedulerSnapshot = &SchedulerSnapshotService{cache: cache, accountRepo: svc.accountRepo, cfg: svc.cfg}
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(context.Background(), &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true)
		require.NoError(t, err)
		require.NotNil(t, selection)
		releaseLegacySchedulerDecisionSelection(selection)
		require.Equal(t, accounts[1].ID, selection.Account.ID)
		require.Equal(t, 50, selection.Account.Priority)
		require.Zero(t, accounts[1].schedulingPriority)
	}
}
