package service

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// A timestamped request key can be retried for 24 hours. Recent manual receipts
// survive history pruning; an older key can never create a second paid job.
const manualRefillRequestLifetime = 24 * time.Hour

type ManualAccountRefillRequest struct {
	Provider  string `json:"provider,omitempty"`
	Force     bool   `json:"force,omitempty"`
	GroupID   int64  `json:"group_id,omitempty"`
	RequestID string `json:"request_id"`
	Plan      string `json:"plan"`
	Revision  int64  `json:"revision"`
}

type ManualAccountRefillValidationError struct{ Message string }

func (e *ManualAccountRefillValidationError) Error() string { return e.Message }
func invalidManualRefill(message string) error {
	return &ManualAccountRefillValidationError{Message: message}
}

func validateManualRefillRequest(r ManualAccountRefillRequest, now time.Time) error {
	parts := strings.Split(r.RequestID, ":")
	validPlan := r.Plan == "pro5x" || r.Plan == "pro20x"
	if r.Provider == refillProviderMuze {
		validPlan = validMuzeIdentifier(r.Plan, false) && muzeTier(r.Plan) != ""
	}
	if (r.Provider != "" && r.Provider != refillProviderMuze && r.Provider != refillProvider558669) || len(parts) != 2 || len(parts[0]) != 13 || len(parts[1]) != 36 || r.Revision < 1 || !validPlan {
		return invalidManualRefill("手动补号参数无效")
	}
	stamp, err := strconv.ParseInt(parts[0], 10, 64)
	key, keyErr := uuid.Parse(parts[1])
	if err != nil || keyErr != nil || key.Version() != 4 || key.String() != parts[1] {
		return invalidManualRefill("手动补号请求标识无效")
	}
	issued := time.UnixMilli(stamp)
	if issued.After(now.Add(5*time.Minute)) || issued.Before(now.Add(-manualRefillRequestLifetime)) {
		return invalidManualRefill("请求已过期或本机时间不正确，请核对任务进度后重新提交")
	}
	return nil
}

func trimAccountRefillJobs(jobs []AccountRefillJob, now time.Time) []AccountRefillJob {
	kept := jobs[:0]
	for i, j := range jobs {
		if i < 200 || j.pending() || j.needs558669Sync(now) || j.ReplacementPending || (j.ManualRequestID != "" && j.CreatedAt.After(now.Add(-manualRefillRequestLifetime-time.Hour))) {
			kept = append(kept, j)
		}
	}
	return kept
}

// Manual reserves one order using the saved delivery settings. The existing
// worker submits and imports it, even with monitoring and automation disabled.
// This endpoint never makes a paid request before its durable reservation.
func (s *AccountRefillService) Manual(ctx context.Context, request ManualAccountRefillRequest) error {
	if request.Provider == "1208" {
		request.Provider = "" // Historical receipts encode 1208 as an empty provider.
	}
	if err := validateManualRefillRequest(request, time.Now()); err != nil {
		return err
	}
	jobID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("sub2api:manual-refill:"+request.RequestID)).String()
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err == nil && request.Provider == refillProviderMuze {
			state, err = s.decodeRefillWithMuze(ctx, raw)
		}
		if err != nil {
			return err
		}
		if request.GroupID != 0 && request.GroupID != state.Config.GroupID {
			return invalidManualRefill("手动补号分组与当前规则不匹配")
		}
		// Check receipts before revision, budget or switches: retries acknowledge
		// the original accepted order, even if configuration changed afterwards.
		for _, j := range state.Jobs {
			if j.ID == jobID || j.ManualRequestID == request.RequestID {
				expectedPlan := j.Plan
				if j.Provider == refillProviderMuze {
					expectedPlan = j.muzeSKU()
				}
				if j.Provider != request.Provider || expectedPlan != request.Plan || j.Force != request.Force || (j.ConfigRevision != 0 && j.ConfigRevision != request.Revision) {
					return invalidManualRefill("同一请求不能修改套餐或配置版本，请核对原任务")
				}
				return nil
			}
		}
		if stores, ok := s.rootService().store.(AccountRefillGroupStore); ok {
			rows, err := stores.ListRules(ctx)
			if err != nil {
				return err
			}
			for key, raw := range rows {
				if key == s.ruleGroupID {
					continue
				}
				other, err := decodeAccountRefillState(raw)
				if err != nil {
					return err
				}
				for _, job := range other.Jobs {
					if job.ID == jobID || job.ManualRequestID == request.RequestID {
						return invalidManualRefill("此请求已属于其他分组，请核对原任务")
					}
				}
			}
		}
		now := time.Now()
		c := state.Config
		if request.Revision != state.Revision {
			return invalidManualRefill("配置已被其他页面修改，请刷新后再手动补号")
		}
		if err := c.Validate(); err != nil {
			return invalidManualRefill(err.Error())
		}
		if !c.PushEnabled {
			return invalidManualRefill("请先在推送设置中开启允许推送并保存")
		}
		if c.Delivery.ExpiryMode == "fixed" && *c.Delivery.FixedExpiresAt <= now.Unix() {
			return invalidManualRefill("指定截止时间已过，请修改推送设置并保存")
		}
		if now.Before(state.providerBackoff(request.Provider)) {
			return invalidManualRefill("号商暂不可用或正在限流，请稍后再试")
		}
		pending := 0
		for _, j := range state.Jobs {
			if refillJobTimedOut(c, j, now) {
				continue
			}
			if j.pending() {
				pending++
			}
			if !request.Force && refillNeedsAttention(j.Status) {
				return invalidManualRefill("请先在消费与进度中处理需要核对的原任务")
			}
		}
		if !request.Force && pending >= c.MaxInFlight {
			return invalidManualRefill("已达到在途任务上限，请等待交付或调整规则")
		}
		if err := s.validateGroups(ctx, c); err != nil {
			return invalidManualRefill(err.Error())
		}
		if _, err := s.selectProxy(ctx, c.Delivery); err != nil {
			return invalidManualRefill(err.Error())
		}
		persist := func() error { return saveRefillState(&state, save) }
		var quote *RefillQuote
		var connectionID string
		if request.Provider == refillProviderMuze {
			quote, connectionID, err = s.singleMuzeQuote(ctx, &state, request.Plan, true, persist)
		} else {
			quote, connectionID, err = s.routedQuote(ctx, &state, request.Provider, request.Plan, persist)
		}
		if err != nil {
			return err
		}
		if !state.canReserveRefill(request.Provider, quote.Price, now) {
			return invalidManualRefill("已达到本日数量或积分预算")
		}
		grace := c.ImportExpiryGraceMinutes
		job := AccountRefillJob{ID: jobID, Provider: request.Provider, ManualRequestID: request.RequestID, ConfigRevision: request.Revision, Force: request.Force,
			Status: "submitting", Reason: "手动补号", Plan: quote.Plan, Price: quote.Price, PriceVersion: quote.PriceVersion,
			ReasonDetail:       "管理员手动提交 1 个账号，沿用提交时保存的推送设置。",
			ExpiryGraceMinutes: &grace, GroupID: c.GroupID, Concurrency: c.importConcurrencyForPlan(quote.Plan),
			DeliveryGroupIDs: slices.Clone(c.DeliveryGroupIDs), DeliveryConfig: c.Delivery,
			CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
		if request.Force {
			job.Reason = "强制手动补号"
			job.ReasonDetail = "管理员强制购买 1 个账号，忽略原任务待核对和在途上限；原订单继续核对，可能仍会交付并计费。"
		}
		if request.Provider == refillProviderMuze {
			job.MuzeConnectionID, job.MuzeProductID, job.MuzeStudio = connectionID, quote.muzeProductID, quote.muzeStudio
			job.Reason = "muze" + job.Reason
			job.ReasonDetail += " muze扣减 1 次预购次数，不折算原号商积分；推号后按本单保存的设置应用。"
		}
		state.Jobs = trimAccountRefillJobs(append([]AccountRefillJob{job}, state.Jobs...), now)
		state.addRefillBudget(job)
		state.LastSubmitAt = now
		state.Message = "手动补号已受理，按已保存的推送设置处理"
		return persist()
	})
}

// IsManualRefillValidationError distinguishes a definite rejection from an
// uncertain storage/transport failure, which must retain the same request key.
func IsManualRefillValidationError(err error) bool {
	var invalid *ManualAccountRefillValidationError
	return errors.As(err, &invalid)
}
