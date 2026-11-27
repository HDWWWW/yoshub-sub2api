package service

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
)

const AccountRiskAPIFallbackExtraKey = "yoshub_risk_api_fallback"

type AccountRiskAPIFallbackConfig struct {
	Enabled   bool  `json:"enabled"`
	AccountID int64 `json:"account_id"`
}

type AccountRiskAPIFallbackStatus struct {
	AccountID int64     `json:"account_id"`
	State     string    `json:"state"`
	Healthy   int       `json:"healthy"`
	CheckedAt time.Time `json:"checked_at"`
}

type AccountRiskAPIFallbackLease struct {
	Token  string `json:"token"`
	Paused bool   `json:"paused,omitempty"`
}

func riskAPIAccount(a *Account) bool {
	return a != nil && a.Platform == PlatformOpenAI && a.Type == AccountTypeAPIKey && !a.IsShadow()
}

func (s *AccountRiskService) validateAPIFallback(ctx context.Context, groupID int64, c AccountRiskConfig) error {
	if !c.Enabled || c.APIFallback == nil || !c.APIFallback.Enabled {
		return nil
	}
	a, err := s.accounts.GetByID(ctx, c.APIFallback.AccountID)
	if err != nil {
		return err
	}
	if !riskAPIAccount(a) || !a.IsActive() || !slices.Contains(a.GroupIDs, groupID) {
		return errors.New("兜底账号须为当前分组内状态正常的 OpenAI API Key 账号")
	}
	return nil
}

func (s *AccountRiskService) apiFallbackLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		checkCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		err := s.reconcileAPIFallbacks(checkCtx, time.Now())
		cancel()
		if err != nil && !errors.Is(err, ErrAccountRiskBusy) && ctx.Err() == nil {
			slog.Warn("account_risk_api_fallback_failed", "error", "API 兜底检查未完成，下轮重试")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Use the risk scheduler's eligibility, independently of refill thresholds,
// orders and notification-only events. API accounts and shadows cannot satisfy
// recovery: a usable original OAuth account must actually be available.
func healthyRiskOAuth(a *Account, groupID int64, now time.Time) bool {
	if !a.IsOpenAIOAuth() || a.IsShadow() || !slices.Contains(a.GroupIDs, groupID) ||
		!a.isSchedulableAt(now) || EvaluateAccountSchedulingThreshold(a, nil, now).ShouldPause ||
		supplierReclaimed(riskProbeDeadline(a, nil), now) {
		return false
	}
	// A disabled early-pause threshold does not make an exhausted quota usable.
	for _, quota := range openAIThresholdCandidates(a, now) {
		if quota != nil && quota.usedPercent >= 100 {
			return false
		}
	}
	return true
}

func (s *AccountRiskService) reconcileAPIFallbacks(ctx context.Context, now time.Time) error {
	return s.store.Update(ctx, func(state *AccountRiskState, scheduling AccountRiskScheduling) error {
		statuses := map[int64]*AccountRiskAPIFallbackStatus{}
		ids := make([]int64, 0, len(state.APIFallbackLeases))
		for id := range state.APIFallbackLeases {
			ids = append(ids, id)
		}
		// Read every relevant group before mutating anything. An unavailable
		// snapshot is neither an empty pool nor evidence of recovery.
		for groupID, rule := range state.Rules {
			f := rule.Config.APIFallback
			if !rule.Config.Enabled || f == nil || !f.Enabled {
				continue
			}
			group, err := s.groups.GetByID(ctx, groupID)
			if err != nil {
				return err
			}
			status := &AccountRiskAPIFallbackStatus{AccountID: f.AccountID, State: "disabled", CheckedAt: now}
			statuses[groupID] = status
			ids = append(ids, f.AccountID)
			if group == nil || group.Platform != PlatformOpenAI || group.Status != StatusActive {
				continue
			}
			accounts, err := s.accounts.ListByGroup(ctx, groupID)
			if err != nil {
				return err
			}
			for i := range accounts {
				if healthyRiskOAuth(&accounts[i], groupID, now) {
					status.Healthy++
				}
			}
			status.State = "standby"
			if status.Healthy == 0 {
				status.State = "unavailable"
			}
		}
		if len(ids) > 0 {
			slices.Sort(ids)
			ids = slices.Compact(ids)
			accounts, err := s.accounts.GetByIDs(ctx, ids)
			if err != nil {
				return err
			}
			byID := map[int64]*Account{}
			for _, a := range accounts {
				if a != nil {
					byID[a.ID] = a
				}
			}
			if state.APIFallbackLeases == nil {
				state.APIFallbackLeases = map[int64]AccountRiskAPIFallbackLease{}
			}
			for _, id := range ids {
				a := byID[id]
				lease, owned := state.APIFallbackLeases[id]
				neededBy := int64(0)
				for groupID, status := range statuses {
					if a != nil && status.AccountID == id && status.State != "disabled" && status.Healthy == 0 && slices.Contains(a.GroupIDs, groupID) {
						neededBy = groupID
						break
					}
				}
				if !riskAPIAccount(a) || neededBy == 0 {
					if !owned && riskAPIAccount(a) && a.Schedulable {
						for groupID, status := range statuses {
							if status.AccountID == id && status.State != "disabled" && slices.Contains(a.GroupIDs, groupID) {
								status.State = "manual"
							}
						}
					}
					if owned {
						if riskAPIAccount(a) && a.Extra[AccountRiskAPIFallbackExtraKey] == lease.Token {
							changed, err := scheduling.SetAPIFallback(0, a, lease.Token, false)
							if err != nil {
								return err
							}
							if !changed {
								return ErrAccountRiskConflict
							}
						}
						delete(state.APIFallbackLeases, id)
					}
					continue
				}
				result := "unavailable"
				if owned && (!a.Schedulable || a.Extra[AccountRiskAPIFallbackExtraKey] != lease.Token) {
					lease.Paused = true
					state.APIFallbackLeases[id] = lease
				}
				candidate := *a
				candidate.Schedulable = true
				switch {
				case owned && lease.Paused:
					result = "paused"
				case !candidate.isSchedulableAt(now) || EvaluateAccountSchedulingThreshold(&candidate, nil, now).ShouldPause || AccountHasRiskCapacityPause(a):
				case !owned && a.Schedulable:
					result = "manual"
				case owned:
					result = "active"
				default:
					token := uuid.NewString()
					changed, err := scheduling.SetAPIFallback(neededBy, a, token, true)
					if err != nil {
						return err
					}
					if !changed {
						return ErrAccountRiskConflict
					}
					state.APIFallbackLeases[id] = AccountRiskAPIFallbackLease{Token: token}
					result = "active"
				}
				for groupID, status := range statuses {
					if status.AccountID == id && status.State != "disabled" && slices.Contains(a.GroupIDs, groupID) {
						status.State = result
					}
				}
			}
		}
		for groupID, rule := range state.Rules {
			rule.APIFallback = statuses[groupID]
		}
		return nil
	})
}
