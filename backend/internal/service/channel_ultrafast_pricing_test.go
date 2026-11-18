//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGPT62ChannelUltrafastKeepsGroupFreeCustomerCharge(t *testing.T) {
	const groupID = int64(77)
	for _, model := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		for _, override := range []*float64{nil, pricingMultiplier(1.25)} {
			for _, free := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/override=%t/free=%t", model, override != nil, free), func(t *testing.T) {
					repo := &openAIRecordUsageLogRepoStub{inserted: true}
					userRepo := &openAIRecordUsageUserRepoStub{}
					svc := newOpenAIRecordUsageServiceForTest(repo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
					svc.channelService = newChannelServiceWithPricings(groupID, []ChannelModelPricing{{
						Platform: PlatformOpenAI, Models: []string{model}, BillingMode: BillingModeToken,
						InputPrice: pricingMultiplier(0.001), OutputPrice: pricingMultiplier(0.002), UltrafastMultiplier: override,
					}})
					svc.resolver = NewModelPricingResolver(svc.channelService, svc.billingService)
					gid, tier := groupID, OpenAIFastTierUltrafast
					err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
						Result: &OpenAIForwardResult{RequestID: "resp_gpt62", Model: model, ServiceTier: &tier, Usage: OpenAIUsage{InputTokens: 100, OutputTokens: 50}},
						APIKey: &APIKey{ID: 1, GroupID: &gid, Group: &Group{ID: gid, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 0.5, FreeOpenAIFast: free}},
						User:   &User{ID: 2}, Account: &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
						PricingAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC),
					})
					require.NoError(t, err)
					require.NotNil(t, repo.lastLog)
					factor := 3.0
					if override != nil {
						factor = *override
					}
					const standard = 100*0.001 + 50*0.002
					require.InDelta(t, standard*factor, repo.lastLog.TotalCost, 1e-10)
					charged := standard * factor * 0.5
					if free {
						charged = standard * 0.5
					}
					require.Equal(t, tier, *repo.lastLog.ServiceTier)
					require.InDelta(t, charged, repo.lastLog.ActualCost, 1e-10)
					require.Equal(t, 1, userRepo.deductCalls)
					require.InDelta(t, charged, userRepo.lastAmount, 1e-10)
				})
			}
		}
	}
}

func TestChannelUltrafastPricingOverridesBothBillingPaths(t *testing.T) {
	svc := &BillingService{fallbackPrices: make(map[string]*ModelPricing)}
	svc.initFallbackPricing()
	tokens := UsageTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheCreationTokens: 1_000_000, CacheReadTokens: 1_000_000}
	for _, tc := range []struct {
		name string
		card string
		want float64
	}{
		{"unset", `{}`, 6},
		{"null", `{"ultrafast_multiplier":null}`, 6},
		{"custom", `{"ultrafast_multiplier":3}`, 3},
		{"standard price", `{"ultrafast_multiplier":1}`, 1},
		{"fractional", `{"ultrafast_multiplier":1.25}`, 1.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var card ChannelModelPricing
			require.NoError(t, json.Unmarshal([]byte(tc.card), &card))
			card.Platform = PlatformOpenAI
			card.Models = []string{"gpt-6-astra"}
			card.InputPrice = pricingMultiplier(1e-6)
			card.OutputPrice = pricingMultiplier(2e-6)
			card.CacheWritePrice = pricingMultiplier(3e-6)
			card.CacheReadPrice = pricingMultiplier(0.1e-6)
			card.FastMultiplier = pricingMultiplier(2.5)
			card.FlexMultiplier = pricingMultiplier(0.4)
			groupID := int64(8907)
			resolver := NewModelPricingResolver(newChannelServiceWithPricings(groupID, []ChannelModelPricing{card}), svc)
			for name, calculate := range map[string]func(string) (*CostBreakdown, error){
				"legacy": func(tier string) (*CostBreakdown, error) {
					return svc.calculateCostInternalWithPolicy("gpt-6-astra", tokens, 1, tier, &card, false)
				},
				"unified": func(tier string) (*CostBreakdown, error) {
					return svc.CalculateCostUnified(CostInput{
						Ctx: context.Background(), Model: "gpt-6-astra", GroupID: &groupID,
						Tokens: tokens, RateMultiplier: 1, ServiceTier: tier, Resolver: resolver,
						DisableModelLongContextPricing: true,
					})
				},
			} {
				t.Run(name, func(t *testing.T) {
					cost, err := calculate(" UltraFast ")
					require.NoError(t, err)
					require.InDelta(t, tc.want, cost.InputCost, 1e-12)
					require.InDelta(t, 2*tc.want, cost.OutputCost, 1e-12)
					require.InDelta(t, 3*tc.want, cost.CacheCreationCost, 1e-12)
					require.InDelta(t, 0.1*tc.want, cost.CacheReadCost, 1e-12)
					for tier, multiplier := range map[string]float64{"priority": 2.5, "flex": 0.4, "default": 1} {
						control, err := calculate(tier)
						require.NoError(t, err)
						require.InDelta(t, multiplier, control.InputCost, 1e-12)
					}
				})
			}
			card.Intervals = []PricingInterval{{MinTokens: 100, InputMultiplier: pricingMultiplier(2)}}
			resolver = NewModelPricingResolver(newChannelServiceWithPricings(groupID, []ChannelModelPricing{card}), svc)
			intervalCost, err := svc.CalculateCostUnified(CostInput{
				Ctx: context.Background(), Model: "gpt-6-astra", GroupID: &groupID,
				Tokens: tokens, RateMultiplier: 1, ServiceTier: "ultrafast", Resolver: resolver,
				DisableModelLongContextPricing: true,
			})
			require.NoError(t, err)
			require.InDelta(t, 2*tc.want, intervalCost.InputCost, 1e-12)
			// A channel override must not mutate the model's shared default.
			catalog, err := svc.GetModelPricing("gpt-6-astra")
			require.NoError(t, err)
			require.Equal(t, 6.0, configuredServiceTierMultiplier("ultrafast", catalog))
		})
	}
}
