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

const riskActionReduceConcurrency = "reduce_concurrency"

func risk429Enabled(c AccountRiskConfig) bool {
	return c.Enabled && (c.RateLimit429Enabled || c.RateLimit429ReduceConcurrency)
}

func riskBurstEpoch(r *AccountRiskRule, accountID int64, reduce bool) string {
	for _, e := range r.Events {
		if e.AccountID == accountID && (e.Action == "stop" || (reduce && e.Action == riskActionReduceConcurrency)) {
			return e.ID
		}
	}
	return ""
}

func canReduceRiskConcurrency(a *Account, limit int) bool {
	return a.Schedulable && limit > 0 && (a.Concurrency <= 0 || a.Concurrency > limit)
}

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
		reduce := !custom && r.Config.RateLimit429ReduceConcurrency
		if reduce && !canReduceRiskConcurrency(account, r.Config.RateLimit429ConcurrencyLimit) {
			continue
		}
		// A manual resume or increase should require fresh evidence after an action.
		epoch := riskBurstEpoch(r, account.ID, reduce)
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
		var applied bool
		err = s.store.Update(ctx, func(currentState *AccountRiskState, scheduling AccountRiskScheduling) error {
			currentRule := currentState.Rules[id]
			if currentRule == nil || currentRule.Revision != r.Revision || riskBurstEpoch(currentRule, account.ID, reduce) != epoch {
				return nil
			}
			if !currentRule.Config.Enabled || (custom && !currentRule.Config.CustomErrorEnabled) || (!custom && !risk429Enabled(currentRule.Config)) {
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
			event := AccountRiskEvent{At: now, Kind: kind, Occurrences: int(count)}
			trigger := fmt.Sprintf("%d 秒内%s超过 %d 次", window, label, threshold)
			if reduce {
				limit := currentRule.Config.RateLimit429ConcurrencyLimit
				if !canReduceRiskConcurrency(current, limit) {
					return nil
				}
				previous := fmt.Sprint(current.Concurrency)
				if current.Concurrency <= 0 {
					previous = "不限"
				}
				applied, err = scheduling.ReduceConcurrency(id, current, limit)
				event.Action = riskActionReduceConcurrency
				event.Reason = fmt.Sprintf("%s，账号并发上限从 %s 降至 %d，继续调度", trigger, previous, limit)
			} else {
				applied, err = scheduling.Stop(id, current)
				event.Action = "stop"
				event.Reason = trigger + "，已关闭账号调度，可使用测试消息手动复测"
			}
			if err != nil {
				return err
			}
			if !applied {
				return errAccountRiskModelAccountChanged
			}
			if !reduce {
				riskReset(&currentRule.Observation, "stopped", event.Reason)
			}
			appendAccountRiskAction(currentRule, current, event)
			return nil
		})
		if err != nil {
			return false, err
		}
		if applied {
			s.wakeNotifications()
			if !reduce {
				return true, nil
			}
		}
	}
	return false, nil
}
