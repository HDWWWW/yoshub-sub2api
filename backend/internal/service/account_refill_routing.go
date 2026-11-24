package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

const retiredRefillProvider = "fendudu"

// The empty job provider is the on-disk identifier for 1208, including old orders.
func (c AccountRefillConfig) automaticProvider() string {
	if c.PrimaryProvider == refillProviderMuze {
		return refillProviderMuze
	}
	return ""
}
func otherRefillProvider(primary string) string {
	if primary == refillProviderMuze {
		return ""
	}
	return refillProviderMuze
}
func refillPublicProvider(provider string) string {
	if provider == refillProviderMuze {
		return refillProviderMuze
	}
	return "1208"
}
func refillProviderName(provider string) string {
	if provider == refillProviderMuze {
		return "Muze"
	}
	return "1208"
}
func (state *accountRefillState) providerBackoff(provider string) time.Time {
	if provider == refillProviderMuze {
		return state.Muze.RetryAt
	}
	return state.ProviderRetryAt
}

// The legacy token and connection are deliberately excluded. Only rule settings
// and links accounting for an already triggered backup are portable.
func migrateRetiredRefillProvider(raw string, state *accountRefillState) error {
	var legacy struct {
		Config struct {
			Fallback *AccountRefillMuzeFallbackConfig `json:"fendudu_fallback"`
		}
		Fallback *accountRefillMuzeFallbackState `json:"fendudu_fallback"`
		Current  *accountRefillMuzeFallbackState `json:"muze_fallback"`
		Jobs     []struct {
			FallbackID string `json:"fendudu_fallback_job_id"`
		}
	}
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return errors.New("无法读取旧号商补号规则")
	}
	if state.Config.MuzeFallback == nil {
		state.Config.MuzeFallback = legacy.Config.Fallback
	}
	if legacy.Current == nil && legacy.Fallback != nil {
		state.MuzeFallback = *legacy.Fallback
		state.MuzeFallback.NextAttemptAt = time.Time{}
	}
	for i := range state.Jobs {
		j := &state.Jobs[i]
		if j.MuzeFallbackJobID == "" {
			j.MuzeFallbackJobID = legacy.Jobs[i].FallbackID
		}
		if j.Provider == retiredRefillProvider && j.pending() {
			j.Status = "blocked"
			j.Error = "旧号商订单已停止自动核对，请到原号商确认结果"
		}
	}
	return nil
}

// Reserve in the supplier's native unit. 1208 uses wallet points; Muze uses
// prepaid units and a concrete destination captured with the order.
func (s *AccountRefillService) routedQuote(ctx context.Context, state *accountRefillState, provider, plan string, persist func() error) (*RefillQuote, string, error) {
	if time.Now().Before(state.providerBackoff(provider)) {
		return nil, "", invalidManualRefill(refillProviderName(provider) + " 正在退避")
	}
	if provider == refillProviderMuze {
		return s.singleMuzeQuote(ctx, state, plan, false, persist)
	}
	q, err := s.manualRefillQuote(ctx, state, plan, persist)
	return q, "", err
}

func (s *AccountRefillService) submitRefillReservation(ctx context.Context, state *accountRefillState, j *AccountRefillJob, token string, persist func() error) error {
	if j.Provider == refillProviderMuze {
		return s.advanceMuzeJob(ctx, state, j, time.Now(), persist)
	}
	return s.advance(ctx, token, state, j, persist)
}

// A documented refusal to supply a plan is distinct from a failed HTTP read or
// local reservations. Only the former can drive stock-based supplier failover.
type refillSupplyUnavailable struct {
	*ManualAccountRefillValidationError
}

func (e *refillSupplyUnavailable) Unwrap() error { return e.ManualAccountRefillValidationError }
func supplyUnavailable(message string) error {
	return &refillSupplyUnavailable{&ManualAccountRefillValidationError{Message: message}}
}

type refillSupplyShortage struct {
	Provider    string    `json:"provider,omitempty"`
	BackupJobID string    `json:"backup_job_id,omitempty"`
	Plan        string    `json:"plan"`
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	Checks      int       `json:"checks"`
}

func (s *AccountRefillService) automaticPrimaryQuote(ctx context.Context, state *accountRefillState, primary, plan string, persist func() error) (*RefillQuote, string, error) {
	if primary == refillProviderMuze {
		return s.routedQuote(ctx, state, refillProviderMuze, plan, persist)
	}
	if state.Catalog == nil {
		return nil, "", errors.New("缺少 1208 报价")
	}
	if state.Key != nil && (state.Key.Revoked || !state.Key.ExpiresAt.IsZero() && !state.Key.ExpiresAt.After(time.Now())) {
		return nil, "", errors.New("1208 API Key 已失效，请替换")
	}
	for i := range state.Catalog.Items {
		q := &state.Catalog.Items[i]
		if q.Service != "refill" || q.Plan != plan || !q.Enabled {
			continue
		}
		if math.IsNaN(q.Price) || math.IsInf(q.Price, 0) || q.Price < 0 || q.Price > state.Config.MaxUnitPrice {
			return nil, "", errors.New("当前单价超过允许上限或报价无效")
		}
		reserved, err := s.unsubmittedPoints(ctx, state)
		if err != nil {
			return nil, "", err
		}
		available := state.Catalog.Available
		if math.IsNaN(available) || math.IsInf(available, 0) {
			return nil, "", errors.New("1208 余额无效")
		}
		if available+1e-8 < q.Price+reserved {
			if err := s.notifyRefillBalance(ctx, state, available, q.Price+reserved, persist); err != nil {
				return nil, "", err
			}
			if available+1e-8 < q.Price {
				return nil, "", supplyUnavailable("1208 可用积分不足")
			}
			return nil, "", errors.New("1208 可用积分不足（含本地预占）")
		}
		return q, "", nil
	}
	return nil, "", supplyUnavailable("1208 所选补号套餐暂不可用")
}

func (s *AccountRefillService) automaticQuote(ctx context.Context, state *accountRefillState, route refillRoute, now time.Time, persist func() error) (*RefillQuote, string, bool, error) {
	primary, plan := route.provider, route.plan
	q, connection, err := s.automaticPrimaryQuote(ctx, state, primary, plan, persist)
	rule := state.Config.MuzeFallback
	var unavailable *refillSupplyUnavailable
	shortage := state.MuzeFallback.Shortage
	if shortage != nil && shortage.Provider == "" { // Receipts written before per-step suppliers.
		shortage.Provider = refillPublicProvider(state.Config.automaticProvider())
	}
	supplier := refillPublicProvider(primary)
	if !rule.enabled() || !errors.As(err, &unavailable) {
		// Only uninterrupted observations of supplier shortage count. A failed
		// balance read cannot release an already funded backup for another POST.
		if !rule.enabled() || err == nil || (shortage != nil && shortage.BackupJobID == "") {
			state.MuzeFallback.Shortage = nil
		}
		return q, connection, false, err
	}
	if shortage != nil && shortage.Provider == supplier && shortage.Plan == plan && shortage.BackupJobID != "" {
		delivered := slices.ContainsFunc(state.Jobs, func(j AccountRefillJob) bool {
			return j.ID == shortage.BackupJobID && j.Status == "imported" && j.AccountID > 0
		})
		if !delivered {
			return nil, "", false, errors.New("本轮缺货已创建备用订单，等待主号商恢复或需求变化")
		}
		shortage = nil // A delivered backup covers its slot. Re-evaluate any remaining gap.
	}
	if shortage == nil || shortage.Provider != supplier || shortage.Plan != plan {
		shortage = &refillSupplyShortage{Provider: supplier, Plan: plan, FirstAt: now}
		state.MuzeFallback.Shortage = shortage
	}
	if shortage.Checks == 0 || now.Sub(shortage.LastAt) >= time.Duration(state.Config.CheckSeconds)*time.Second {
		shortage.Checks++
		shortage.LastAt = now
	}
	if shortage.Checks < rule.FailureThreshold && now.Before(shortage.FirstAt.Add(time.Duration(rule.TimeoutMinutes)*time.Minute)) {
		return nil, "", false, fmt.Errorf("%w，已连续确认 %d 次", err, shortage.Checks)
	}
	for _, j := range state.Jobs {
		if j.isBackup() && j.pending() {
			return nil, "", false, errors.New("等待在途备用订单完成")
		}
	}
	q, connection, err = s.routedQuote(ctx, state, otherRefillProvider(primary), "pro20x", persist)
	return q, connection, true, err
}
func (j AccountRefillJob) isBackup() bool { return j.FallbackForJobID != "" || j.SupplyFallback }
