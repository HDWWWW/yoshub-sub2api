package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type accountHealthDurationWriteKey struct{}

// Resolve relative time once on the server. The existing expiry field remains
// the durable deadline used by request admission and expiry maintenance.
func prepareAccountHealthDuration(ctx context.Context, input *UpdateAccountInput, now time.Time) (context.Context, *UpdateAccountInput, error) {
	if input.HealthDurationMinutes == nil {
		return ctx, input, nil
	}
	if adminaccess.RestrictedFromContext(ctx) != nil {
		return ctx, input, infraerrors.Forbidden("HEALTH_DURATION_FORBIDDEN", "健康时长仅限完整管理员设置")
	}
	minutes := *input.HealthDurationMinutes
	if minutes < 1 || minutes > 525600 {
		return ctx, input, infraerrors.BadRequest("INVALID_HEALTH_DURATION", "健康时长须为 1–525600 的整数分钟")
	}
	if input.ExpiresAt != nil || input.ExpiryGraceMinutes != nil || (input.AutoPauseOnExpired != nil && !*input.AutoPauseOnExpired) {
		return ctx, input, infraerrors.BadRequest("CONFLICTING_HEALTH_DURATION", "健康时长不能同时指定到期时间、保留时间或关闭到期停调")
	}
	copy := *input
	deadline, autoPause := now.Add(time.Duration(minutes)*time.Minute).Unix(), true
	copy.ExpiresAt, copy.AutoPauseOnExpired = &deadline, &autoPause
	copy.HealthDurationMinutes = nil
	return context.WithValue(ctx, accountHealthDurationWriteKey{}, true), &copy, nil
}

func isAccountHealthDurationWrite(ctx context.Context) bool {
	enabled, _ := ctx.Value(accountHealthDurationWriteKey{}).(bool)
	return enabled
}
