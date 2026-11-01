package service

import (
	"context"
	"errors"
	"math"
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
	if len(parts) != 2 || len(parts[0]) != 13 || len(parts[1]) != 36 || r.Revision < 1 || !slices.Contains([]string{"pro5x", "pro20x"}, r.Plan) {
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
		if i < 200 || j.pending() || j.ReplacementPending || (j.ManualRequestID != "" && j.CreatedAt.After(now.Add(-manualRefillRequestLifetime-time.Hour))) {
			kept = append(kept, j)
		}
	}
	return kept
}

// Manual reserves one order using the saved delivery settings. The existing
// worker submits and imports it, even with monitoring and automation disabled.
// This endpoint never makes a paid request before its durable reservation.
func (s *AccountRefillService) Manual(ctx context.Context, request ManualAccountRefillRequest) error {
	if err := validateManualRefillRequest(request, time.Now()); err != nil {
		return err
	}
	jobID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("sub2api:manual-refill:"+request.RequestID)).String()
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
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
				if j.Plan != request.Plan || (j.ConfigRevision != 0 && j.ConfigRevision != request.Revision) {
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
		if now.Before(state.ProviderRetryAt) {
			return invalidManualRefill("号商暂不可用或正在限流，请稍后再试")
		}
		pending := 0
		for _, j := range state.Jobs {
			if j.pending() {
				pending++
			}
			if slices.Contains([]string{"blocked", "review", "import_failed"}, j.Status) {
				return invalidManualRefill("请先在消费与进度中处理需要核对的原任务")
			}
		}
		if pending >= c.MaxInFlight {
			return invalidManualRefill("已达到在途任务上限，请等待交付或调整规则")
		}
		if err := s.validateGroups(ctx, c); err != nil {
			return invalidManualRefill(err.Error())
		}
		if _, err := s.selectProxy(ctx, c.Delivery); err != nil {
			return invalidManualRefill(err.Error())
		}
		if state.EncryptedToken == "" {
			return invalidManualRefill("请先配置号商 API Key")
		}
		token, err := s.encryptor.Decrypt(state.EncryptedToken)
		if err != nil {
			return invalidManualRefill("密钥解密失败，请重新保存 API Key")
		}
		persist := func() error { return saveRefillState(&state, save) }
		providerFailure := func(err error) error {
			state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
			if err := persist(); err != nil {
				return err
			}
			return invalidManualRefill("无法读取号商报价或余额，请稍后重试")
		}
		catalog, err := s.provider.Catalog(ctx, token)
		if err != nil || catalog == nil {
			return providerFailure(err)
		}
		wallet, key, err := s.provider.Identity(ctx, token)
		if err != nil || wallet == nil || key == nil {
			return providerFailure(err)
		}
		state.Wallet, state.Key, state.IdentityAt = wallet, key, &now
		if err := s.notifyRefillBalance(ctx, &state, wallet.Available, 0, persist); err != nil {
			return err
		}
		if key.Revoked || (!key.ExpiresAt.IsZero() && !key.ExpiresAt.After(time.Now())) {
			return invalidManualRefill("号商 API Key 已失效，请替换")
		}
		var quote *RefillQuote
		for i := range catalog.Items {
			q := &catalog.Items[i]
			if q.Service == "refill" && q.Plan == request.Plan && q.Enabled {
				quote = q
				break
			}
		}
		if quote == nil {
			return invalidManualRefill("所选补号套餐暂不可用")
		}
		if math.IsNaN(quote.Price) || math.IsInf(quote.Price, 0) || quote.Price < 0 || quote.Price > c.MaxUnitPrice {
			return invalidManualRefill("当前单价超过允许上限或报价无效，请检查补号规则")
		}
		day := now.In(refillTimezone).Format("2006-01-02")
		usage := state.Usage[day]
		if usage.Count >= c.DailyMaxCount || usage.Points+quote.Price > c.DailyMaxPoints+1e-8 {
			return invalidManualRefill("已达到本日数量或积分预算")
		}
		// Locally queued jobs have not reached the supplier wallet yet.
		unsubmitted, reserveErr := s.unsubmittedPoints(ctx, &state)
		if reserveErr != nil {
			return invalidManualRefill(reserveErr.Error())
		}
		if math.IsNaN(catalog.Available) || math.IsNaN(wallet.Available) || math.Min(catalog.Available, wallet.Available)+1e-8 < quote.Price+unsubmitted {
			if err := s.notifyRefillBalance(ctx, &state, math.Min(catalog.Available, wallet.Available), quote.Price+unsubmitted, persist); err != nil {
				return err
			}
			return invalidManualRefill("号商可用积分不足（已扣除本地在途预占）")
		}
		state.Catalog, state.Wallet, state.Key = catalog, wallet, key
		state.CatalogAt, state.IdentityAt = &now, &now
		grace := c.ImportExpiryGraceMinutes
		job := AccountRefillJob{ID: jobID, ManualRequestID: request.RequestID, ConfigRevision: request.Revision,
			Status: "submitting", Reason: "手动补号", Plan: request.Plan, Price: quote.Price, PriceVersion: quote.PriceVersion,
			ReasonDetail:       "管理员手动提交 1 个账号，沿用提交时保存的推送设置。",
			ExpiryGraceMinutes: &grace, GroupID: c.GroupID, Concurrency: c.ImportConcurrency,
			DeliveryGroupIDs: slices.Clone(c.DeliveryGroupIDs), DeliveryConfig: c.Delivery,
			CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
		state.Jobs = trimAccountRefillJobs(append([]AccountRefillJob{job}, state.Jobs...), now)
		usage.Count++
		usage.Points += quote.Price
		state.Usage[day] = usage
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
