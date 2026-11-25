package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

func ProvideAccountRiskService(store AccountRiskStore, accounts AccountRepository, groups GroupRepository, loads *ConcurrencyService, probe *AccountTestService, cfg *config.Config, refill *AccountRefillService, gateway *OpenAIGatewayService, counter429 AccountRisk429Counter, reporter *Account401Reporter) *AccountRiskService {
	s := NewAccountRiskService(store, accounts, groups, loads, probe, cfg)
	s.notificationWake = reporter.wake
	s.notifier = refill
	s.counter429 = counter429
	s.onProbe401Committed = func(ctx context.Context, a *Account, until time.Time, invalidate bool) {
		limiter := gateway.rateLimitService
		reason := "auth_error"
		if !until.IsZero() {
			reason = "oauth_401"
		}
		limiter.notifyAccountSchedulingBlocked(a, until, reason)
		if invalidate && limiter != nil && limiter.tokenCacheInvalidator != nil {
			if err := limiter.tokenCacheInvalidator.InvalidateToken(ctx, a); err != nil {
				slog.Warn("risk_probe_401_token_cache_invalidation_failed", "account_id", a.ID, "error", err)
			}
		}
	}
	gateway.accountRisk = s
	s.Start()
	return s
}
