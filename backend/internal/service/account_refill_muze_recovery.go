package service

import (
	"context"
	"errors"
	"time"
)

// A lost POST receipt is reconciled using only the exact persisted requestId.
// Concurrent purchases of the same plan (even in the same second) stay independent.
func (s *AccountRefillService) recoverMuzeOrder(ctx context.Context, token string, state *accountRefillState, j AccountRefillJob) (*muzeCycle, error) {
	reader, ok := s.muzeClient().(muzeOrderFinder)
	if !ok || j.SubmissionStartedAt == nil || j.MuzeStudio {
		return nil, errors.New("请在 muze 核对此请求的原订单")
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cycle, err := reader.FindOrder(ctx, token, j)
	if err != nil {
		return nil, err
	}
	if cycle == nil || cycle.RequestID != j.ID {
		return nil, errors.New("muze 订单缺少匹配的请求编号")
	}
	if err = validateMuzeCycle(cycle, j); err != nil {
		return nil, err
	}
	if err = s.muzeOrderUnclaimed(ctx, state.Jobs, j.ID, cycle.ID); err != nil {
		return nil, err
	}
	return cycle, nil
}
