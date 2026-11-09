package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Increment keeps a bounded, shared rolling window. token identifies one actual
// upstream response, so retrying persistence cannot count that response twice.
type AccountRisk429Counter interface {
	Increment(context.Context, string, string, int, int) (int64, error)
}

func (s *OpenAIGatewayService) observeAccountRisk429(ctx context.Context, a *Account, status int) bool {
	if s == nil || s.accountRisk == nil || status != http.StatusTooManyRequests {
		return false
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	token := uuid.NewString()
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var stopped bool
		stopped, err = s.accountRisk.Handle429(persist, a, token, time.Now().UTC())
		if stopped {
			s.BlockAccountScheduling(a, time.Time{}, "account_risk_429")
			return true
		}
		if !errors.Is(err, ErrAccountRiskBusy) && !errors.Is(err, errAccountRiskModelAccountChanged) {
			break
		}
		select {
		case <-persist.Done():
			return false
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	if err != nil {
		slog.Warn("account 429 risk action failed", "error", err)
	}
	return false
}

func risk429Enabled(c AccountRiskConfig) bool { return c.Enabled && c.RateLimit429Enabled }

func (s *AccountRiskService) Handle429(ctx context.Context, account *Account, token string, now time.Time) (bool, error) {
	if s == nil || s.counter429 == nil || account == nil || account.Platform != PlatformOpenAI {
		return false, nil
	}
	state, err := s.store.Read(ctx)
	if err != nil {
		return false, err
	}
	ids := make([]int64, 0, len(state.Rules))
	for id := range state.Rules {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		r := state.Rules[id]
		if !risk429Enabled(r.Config) || !slices.Contains(account.GroupIDs, id) {
			continue
		}
		// A stop starts a new counting epoch for a later manual re-enable.
		epoch := ""
		for _, event := range r.Events {
			if event.AccountID == account.ID && event.Action == "stop" {
				epoch = event.ID
				break
			}
		}
		key := fmt.Sprintf("%d:%d:%d:%x:%s", id, r.Revision, account.ID, riskModelAccountIdentity(account), epoch)
		count, err := s.counter429.Increment(ctx, key, token, r.Config.RateLimit429WindowSeconds, r.Config.RateLimit429Threshold+1)
		if err != nil {
			return false, err
		}
		if count <= int64(r.Config.RateLimit429Threshold) {
			continue
		}
		stopped := false
		err = s.store.Update(ctx, func(currentState *AccountRiskState, stop AccountRiskScheduling) error {
			currentRule := currentState.Rules[id]
			if currentRule == nil || currentRule.Revision != r.Revision || !risk429Enabled(currentRule.Config) {
				return nil
			}
			g, err := s.groups.GetByID(ctx, id)
			if err != nil {
				return err
			}
			if g == nil || g.Platform != PlatformOpenAI || g.Status != StatusActive {
				return nil
			}
			current, err := s.accounts.GetByID(ctx, account.ID)
			if err != nil {
				return err
			}
			if current == nil || current.Status != StatusActive || (!current.Schedulable && !AccountHasRiskCapacityPause(current)) || !slices.Contains(current.GroupIDs, id) || riskModelAccountIdentity(current) != riskModelAccountIdentity(account) {
				return nil
			}
			stopped, err = stop.Stop(id, current)
			if err != nil {
				return err
			}
			if !stopped {
				return errAccountRiskModelAccountChanged
			}
			reason := fmt.Sprintf("%d 秒内上游 429 超过 %d 次，已关闭账号调度；可使用测试消息手动复测", r.Config.RateLimit429WindowSeconds, r.Config.RateLimit429Threshold)
			riskRecordStop(currentRule, current, now, reason)
			currentRule.Events[0].Kind, currentRule.Events[0].Action = "rate_limit_429", "stop"
			currentRule.Events[0].Occurrences = int(count)
			return nil
		})
		if err != nil {
			return false, err
		}
		if stopped {
			s.wakeNotifications()
			return true, nil
		}
	}
	return false, nil
}
