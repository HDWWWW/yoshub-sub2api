package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIUltrafastPricingKeepsGroupFreeAcceleration(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol"} {
		for _, free := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/free=%t", model, free), func(t *testing.T) {
				usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
				userRepo := &openAIRecordUsageUserRepoStub{}
				svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
				svc.resolver = NewModelPricingResolver(nil, svc.billingService)
				groupID := int64(77)
				inputPrice, outputPrice := 0.001, 0.002
				tier := OpenAIFastTierUltrafast
				err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
					Result: &OpenAIForwardResult{
						RequestID: "resp_ultrafast_group_policy", ServiceTier: &tier,
						UpstreamResponseServiceTier: "default",
						Usage:                       OpenAIUsage{InputTokens: 100, OutputTokens: 50},
						Model:                       model, Duration: time.Second,
					},
					APIKey: &APIKey{ID: 1020, GroupID: &groupID, Group: &Group{
						ID: groupID, Platform: PlatformOpenAI, Status: StatusActive,
						Hydrated: true, RateMultiplier: 0.5, FreeOpenAIFast: free,
						ForceOpenAIFast: true, ForceOpenAIUltrafast: true,
						ModelPricing: []ChannelModelPricing{{Models: []string{model},
							BillingMode: BillingModeToken, InputPrice: &inputPrice, OutputPrice: &outputPrice}},
					}},
					User: &User{ID: 2020}, Account: &Account{ID: 3020, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
				})
				require.NoError(t, err)
				require.NotNil(t, usageRepo.lastLog)
				require.Equal(t, tier, *usageRepo.lastLog.ServiceTier)
				standard := 100*inputPrice + 50*outputPrice
				require.InDelta(t, standard*6, usageRepo.lastLog.TotalCost, 1e-10, "official model cost keeps the 6x tier")
				charged := standard * 6 * 0.5
				if free {
					charged = standard * 0.5
				}
				require.InDelta(t, charged, usageRepo.lastLog.ActualCost, 1e-10)
				require.Equal(t, 1, userRepo.deductCalls)
				require.InDelta(t, charged, userRepo.lastAmount, 1e-10, "deducted balance matches the group policy")
			})
		}
	}
}
