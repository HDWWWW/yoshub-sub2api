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
			accounts[0].Priority, accounts[1].Priority = 2, 1
			accounts[1].AutoPauseOnExpired = true
			svc := newLegacySchedulerDecisionTestService(accounts, loadBatch, schedulerTestConcurrencyCache{})
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
			require.Equal(t, want, selection.Account.ID, "loadBatch=%v excluded=%v", loadBatch, blocked)
		}
	}
}
