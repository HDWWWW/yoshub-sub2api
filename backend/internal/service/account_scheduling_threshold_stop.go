package service

import (
	"context"
	"time"
)

// AccountSchedulingThresholdStopper rechecks current persisted usage and the
// account override under a row lock before turning scheduling off permanently.
type AccountSchedulingThresholdStopper interface {
	StopAccountAtSchedulingThreshold(ctx context.Context, id int64) (bool, error)
}

// ShouldStopAccountAtSchedulingThreshold applies only to an explicit account
// override. Platform defaults and model-only windows retain their recovery rules.
func ShouldStopAccountAtSchedulingThreshold(account *Account, now time.Time) bool {
	if account == nil || !account.IsActive() || !account.Schedulable {
		return false
	}
	if _, ok := accountSchedulingThresholdOverride(account); !ok {
		return false
	}
	return EvaluateAccountSchedulingThreshold(account, nil, now).ShouldPause
}
