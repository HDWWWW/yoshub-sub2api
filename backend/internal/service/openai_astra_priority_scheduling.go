package service

import (
	"context"
	"sort"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// The original request group owns this policy, including composite routes.
// A route's supporting group may provide accounts without overriding that policy.
func (s *OpenAIGatewayService) openAIGroupReversesAccountPriority(ctx context.Context, groupID *int64, platform string) bool {
	if s == nil || ctx == nil || groupID == nil || NormalizeOpenAICompatiblePlatform(platform) != PlatformOpenAI {
		return false
	}
	if group, _ := ctx.Value(ctxkey.Group).(*Group); IsGroupContextValid(group) &&
		groupSupportsOpenAIFast(group.Platform) && (group.ID == *groupID || group.Platform == PlatformComposite) {
		return group.ReverseAccountPriority
	}
	if s.schedulerSnapshot == nil {
		return false
	}
	group, err := s.schedulerSnapshot.GetGroupByIDLite(ctx, *groupID)
	return err == nil && group != nil && groupSupportsOpenAIFast(group.Platform) && group.ReverseAccountPriority
}

func (s *OpenAIGatewayService) preferOpenAIAstraPriority(ctx context.Context, platform, model string) bool {
	if s == nil || NormalizeOpenAICompatiblePlatform(platform) != PlatformOpenAI || !isOpenAIGPT6AstraModel(model) {
		return false
	}
	if s.settingService != nil {
		return s.settingService.astraPriorityEnabled(ctx)
	}
	return s.cfg != nil && s.cfg.Gateway.Scheduling.OpenAIAstraPriorityEnabled
}

// buildOpenAIPrioritySelectionOrder exhausts each priority tier before trying the
// next. Top-K, scoring, compact preference, subscription preference and soft
// affinity operate within a tier, so none can skip an earlier-expiring account.
// buildTier includes overflow candidates: a busy Top-K must not hide capacity in
// the same tier or prevent fallback to a later tier.
func buildOpenAIPrioritySelectionOrder(
	req OpenAIAccountScheduleRequest,
	candidates []openAIAccountCandidateScore,
	buildTier func([]openAIAccountCandidateScore) []openAIAccountCandidateScore,
) []openAIAccountCandidateScore {
	pool := append([]openAIAccountCandidateScore(nil), candidates...)
	compactTier := func(candidate openAIAccountCandidateScore) int {
		if req.RequireCompact {
			return openAICompactSupportTier(candidate.account)
		}
		return 0
	}
	subscription := func(candidate openAIAccountCandidateScore) bool {
		return req.SubscriptionPriority && candidate.account.IsOpenAIChatGPTSubscription()
	}
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].account.SchedulingPriority() != pool[j].account.SchedulingPriority() {
			if req.ReversePriority {
				return pool[i].account.SchedulingPriority() > pool[j].account.SchedulingPriority()
			}
			return pool[i].account.SchedulingPriority() < pool[j].account.SchedulingPriority()
		}
		if compactTier(pool[i]) != compactTier(pool[j]) {
			return compactTier(pool[i]) > compactTier(pool[j])
		}
		return subscription(pool[i]) && !subscription(pool[j])
	})
	order := make([]openAIAccountCandidateScore, 0, len(pool))
	for start := 0; start < len(pool); {
		end := start + 1
		for end < len(pool) && pool[end].account.SchedulingPriority() == pool[start].account.SchedulingPriority() &&
			compactTier(pool[end]) == compactTier(pool[start]) && subscription(pool[end]) == subscription(pool[start]) {
			end++
		}
		order = append(order, buildTier(pool[start:end])...)
		start = end
	}
	return order
}
