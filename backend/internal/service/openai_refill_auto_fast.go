package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

type refillAutoFastContextKey struct{}
type refillAutoFastDecision struct {
	accountID int64
	enabled   bool
}

// Freeze each HTTP attempt / WS turn. Usage must retain its original price
// even if quota reaches the stop threshold while the response is streaming.
func (s *OpenAIGatewayService) withRefillAutoFast(ctx context.Context, account *Account) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil {
		return ctx
	}
	if decision, ok := ctx.Value(refillAutoFastContextKey{}).(refillAutoFastDecision); ok && decision.accountID == account.ID {
		return ctx
	}
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	enabled := s != nil && s.accountRefill != nil && account.IsOpenAIOAuth() && !account.IsShadow() &&
		IsGroupContextValid(group) && groupSupportsOpenAIFast(group.Platform) &&
		!group.FilterOpenAIServiceTier && !group.ForceOpenAIUltrafast &&
		s.accountRefill.accountAutoFast(group.ID, account.ID, time.Now())
	return context.WithValue(ctx, refillAutoFastContextKey{}, refillAutoFastDecision{accountID: account.ID, enabled: enabled})
}

func refillAutoFastForRequest(ctx context.Context, account *Account) bool {
	if ctx == nil || account == nil {
		return false
	}
	decision, ok := ctx.Value(refillAutoFastContextKey{}).(refillAutoFastDecision)
	return ok && decision.accountID == account.ID && decision.enabled
}

func markRefillAutoFastResult(ctx context.Context, account *Account, result *OpenAIForwardResult) {
	if result != nil {
		result.RefillAutoFast = refillAutoFastForRequest(ctx, account)
	}
}

func (s *OpenAIGatewayService) forcedOpenAIServiceTier(ctx context.Context, account *Account) string {
	if refillAutoFastForRequest(ctx, account) {
		return OpenAIFastTierPriority
	}
	return openAIGroupForcedServiceTier(ctx, account)
}

func refillBillsOpenAIFastAtStandard(result *OpenAIForwardResult, account *Account, serviceTier string) bool {
	return result != nil && result.RefillAutoFast && account != nil && account.IsOpenAIOAuth() &&
		normalizedOpenAIServiceTierValue(serviceTier) == OpenAIFastTierPriority
}
