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
	return s.observeRiskBurst(ctx, a, status, nil, false, uuid.NewString(), time.Now().UTC())
}

func (s *OpenAIGatewayService) observeRiskBurst(ctx context.Context, a *Account, status int, message []byte, custom bool, token string, observedAt time.Time) bool {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		var stopped bool
		stopped, err = s.accountRisk.handleRiskBurst(persist, a, token, observedAt, status, message, custom)
		if stopped {
			reason := "account_risk_429"
			if custom {
				reason = "account_risk_custom_error"
			}
			s.BlockAccountScheduling(a, time.Time{}, reason)
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
		slog.Warn("account upstream error risk action failed", "status", status, "error", err)
	}
	return false
}

func risk429Enabled(c AccountRiskConfig) bool { return c.Enabled && c.RateLimit429Enabled }

func (s *AccountRiskService) Handle429(ctx context.Context, account *Account, token string, now time.Time) (bool, error) {
	return s.handleRiskBurst(ctx, account, token, now, http.StatusTooManyRequests, nil, false)
}

func (s *AccountRiskService) handleRiskBurst(ctx context.Context, account *Account, token string, now time.Time, status int, message []byte, custom bool) (bool, error) {
	if s == nil || s.counter429 == nil || account == nil || account.Platform != PlatformOpenAI {
		return false, nil
	}
	if custom && status == http.StatusUnauthorized {
		reclaimed, err := s.risk401Reclaimed(ctx, account, 0, now)
		if err != nil || reclaimed {
			return false, err
		}
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
		if r == nil || !slices.Contains(account.GroupIDs, id) {
			continue
		}
		window, threshold, kind, label := r.Config.RateLimit429WindowSeconds, r.Config.RateLimit429Threshold, "rate_limit_429", "上游 429"
		if custom {
			if !r.Config.Enabled || !r.Config.CustomErrorEnabled || !matchesCustomError(r.Config.CustomErrorRules, status, message) {
				continue
			}
			window, threshold, kind, label = r.Config.CustomErrorWindowSeconds, r.Config.CustomErrorThreshold, "custom_error", "自定义上游错误"
		} else if !risk429Enabled(r.Config) {
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
		if custom {
			key = "custom:" + key
		}
		count, err := s.counter429.Increment(ctx, key, token, window, threshold+1)
		if err != nil {
			return false, err
		}
		if count <= int64(threshold) {
			continue
		}
		stopped := false
		err = s.store.Update(ctx, func(currentState *AccountRiskState, stop AccountRiskScheduling) error {
			currentRule := currentState.Rules[id]
			if currentRule == nil || currentRule.Revision != r.Revision || !currentRule.Config.Enabled || (custom && !currentRule.Config.CustomErrorEnabled) || (!custom && !currentRule.Config.RateLimit429Enabled) {
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
			reason := fmt.Sprintf("%d 秒内%s超过 %d 次，已关闭账号调度，可使用测试消息手动复测", window, label, threshold)
			riskRecordStop(currentRule, current, now, reason)
			currentRule.Events[0].Kind, currentRule.Events[0].Action = kind, "stop"
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
