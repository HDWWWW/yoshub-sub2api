package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
)

type AccountRefillDailySchedule struct {
	ID       string `json:"id,omitempty"`
	Plan     string `json:"plan,omitempty"`
	Enabled  bool   `json:"enabled"`
	Time     string `json:"time"`
	Quantity int    `json:"quantity"`
}

type AccountRefillDailyScheduleStatus struct {
	NextAt       time.Time `json:"next_at"`
	LastAt       time.Time `json:"last_at"`
	LastQuantity int       `json:"last_quantity"`
	LastBatchID  string    `json:"last_batch_id,omitempty"`
	RetryAt      time.Time `json:"retry_at"`
	Message      string    `json:"message"`
}

func refillDailyScheduleEnabled(c AccountRefillConfig) bool {
	if c.DailySchedules != nil {
		return slices.ContainsFunc(c.DailySchedules, func(row AccountRefillDailySchedule) bool { return row.Enabled })
	}
	return c.DailySchedule != nil && c.DailySchedule.Enabled
}

func validateRefillDailySchedules(c AccountRefillConfig) error {
	if c.DailySchedules == nil {
		return validateRefillDailySchedule(c.DailySchedule)
	}
	if len(c.DailySchedules) > 20 {
		return errors.New("最多配置 20 个每日定时补号任务")
	}
	seen := map[string]bool{}
	for _, row := range c.DailySchedules {
		id, err := uuid.Parse(row.ID)
		if row.ID != "legacy" && (err != nil || id.String() != row.ID || id.Version() != 4) || seen[row.ID] {
			return errors.New("定时任务标识无效或重复，请刷新后重试")
		}
		seen[row.ID] = true
		if err := validateRefillDailySchedule(&row); err != nil {
			return err
		}
	}
	return nil
}

func validateRefillDailySchedule(c *AccountRefillDailySchedule) error {
	if c == nil {
		return nil
	}
	clock, err := time.Parse("15:04", c.Time)
	if err != nil || clock.Format("15:04") != c.Time || c.Quantity < 1 || c.Quantity > 100 || !slices.Contains([]string{"", "pro5x", "pro20x"}, c.Plan) {
		return errors.New("每日定时补号须设置 HH:mm 时间和 1–100 个账号")
	}
	return nil
}

func refillDailyTime(c *AccountRefillDailySchedule, now time.Time) time.Time {
	clock, _ := time.Parse("15:04", c.Time) // Validated before use.
	local := now.In(refillTimezone)
	return time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, refillTimezone)
}

func nextRefillDailyTime(c *AccountRefillDailySchedule, now, last time.Time) time.Time {
	next := refillDailyTime(c, now)
	if !next.After(now) || (!last.IsZero() && next.In(refillTimezone).Format("2006-01-02") <= last.In(refillTimezone).Format("2006-01-02")) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// Only enabling/changing the clock resets the future deadline. Ordinary saves
// and quantity edits cannot consume or postpone a due batch. A successful day
// stays consumed even if the administrator toggles or changes the clock.
func configureRefillDailySchedule(state *accountRefillState, c AccountRefillConfig, now time.Time) {
	if c.DailySchedules != nil {
		configureRefillDailySchedules(state, c, now)
		return
	}
	status := &state.DailyScheduleStatus
	if !refillDailyScheduleEnabled(c) {
		status.NextAt, status.RetryAt = time.Time{}, time.Time{}
		status.Message = "每日定时补号已关闭"
		return
	}
	if !refillDailyScheduleEnabled(state.Config) || state.Config.DailySchedule.Time != c.DailySchedule.Time || status.NextAt.IsZero() {
		status.NextAt = nextRefillDailyTime(c.DailySchedule, now, status.LastAt)
	}
	status.RetryAt = time.Time{}
	status.Message = "等待每日定时补号"
}

func configureRefillDailySchedules(state *accountRefillState, c AccountRefillConfig, now time.Time) {
	if state.DailyScheduleStatuses == nil {
		state.DailyScheduleStatuses = map[string]AccountRefillDailyScheduleStatus{}
	}
	previous := map[string]AccountRefillDailySchedule{}
	for _, row := range state.Config.DailySchedules {
		previous[row.ID] = row
	}
	if state.Config.DailySchedules == nil && state.Config.DailySchedule != nil {
		previous["legacy"] = *state.Config.DailySchedule
		state.DailyScheduleStatuses["legacy"] = state.DailyScheduleStatus
	}
	current := map[string]bool{}
	for _, row := range c.DailySchedules {
		current[row.ID] = true
		status := state.DailyScheduleStatuses[row.ID]
		old := previous[row.ID]
		if !row.Enabled {
			status.NextAt, status.RetryAt = time.Time{}, time.Time{}
			status.Message = "此定时补号任务已关闭"
		} else {
			if !old.Enabled || old.Time != row.Time || status.NextAt.IsZero() {
				status.NextAt = nextRefillDailyTime(&row, now, status.LastAt)
			}
			status.RetryAt = time.Time{}
			status.Message = "等待每日定时补号"
		}
		state.DailyScheduleStatuses[row.ID] = status
	}
	// Retain today's receipts for removed IDs so re-adding one cannot buy twice.
	day := now.In(refillTimezone).Format("2006-01-02")
	for id, status := range state.DailyScheduleStatuses {
		if !current[id] && (status.LastAt.IsZero() || status.LastAt.In(refillTimezone).Format("2006-01-02") < day) {
			delete(state.DailyScheduleStatuses, id)
		}
	}
}

// Called inside the shared store lock, before normal automatic gates. One
// durable write reserves a task's jobs, budget, rotation and daily receipt.
// Network submission is performed by the existing idempotent job worker.
func (s *AccountRefillService) runRefillDailySchedule(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) (bool, error) {
	if state.Config.DailySchedules == nil {
		return s.runRefillDailyScheduleEntry(ctx, state, state.Config.DailySchedule, &state.DailyScheduleStatus, now, persist)
	}
	for _, schedule := range state.Config.DailySchedules {
		status := state.DailyScheduleStatuses[schedule.ID]
		save := func() error {
			if state.DailyScheduleStatuses == nil {
				state.DailyScheduleStatuses = map[string]AccountRefillDailyScheduleStatus{}
			}
			state.DailyScheduleStatuses[schedule.ID] = status
			return persist()
		}
		reserved, err := s.runRefillDailyScheduleEntry(ctx, state, &schedule, &status, now, save)
		if err != nil || reserved {
			return reserved, err
		}
	}
	return false, nil
}

func (s *AccountRefillService) runRefillDailyScheduleEntry(ctx context.Context, state *accountRefillState, schedule *AccountRefillDailySchedule, status *AccountRefillDailyScheduleStatus, now time.Time, persist func() error) (bool, error) {
	c := state.Config
	if schedule == nil || !schedule.Enabled {
		return false, nil
	}
	blocked := func(message string) (bool, error) {
		status.Message = "每日定时补号等待重试：" + message
		status.RetryAt = now.Add(time.Minute)
		return false, persist()
	}
	if now.Before(status.RetryAt) {
		return false, nil
	}
	if err := c.Validate(); err != nil {
		return blocked(err.Error())
	}
	if status.NextAt.IsZero() {
		status.NextAt = nextRefillDailyTime(schedule, now, status.LastAt)
		return false, persist()
	}
	if now.Before(status.NextAt) {
		return false, nil
	}
	// Downtime never causes an unbounded catch-up purchase. Keep only the most
	// recent elapsed daily batch; do not accumulate all missed days.
	due := refillDailyTime(schedule, now)
	if due.After(now) {
		due = due.AddDate(0, 0, -1)
	}
	if !status.LastAt.IsZero() && !due.After(status.LastAt) {
		status.NextAt = nextRefillDailyTime(schedule, now, status.LastAt)
		return false, persist()
	}
	if now.Before(state.ProviderRetryAt) {
		return blocked("号商限流或暂不可用，等待退避结束")
	}
	if state.EncryptedToken == "" {
		return blocked("请配置号商 API Key")
	}
	token, err := s.encryptor.Decrypt(state.EncryptedToken)
	if err != nil {
		return blocked("密钥解密失败，请重新保存 API Key")
	}
	if err := s.validateGroups(ctx, c); err != nil {
		return blocked(err.Error())
	}
	if _, err := s.selectProxy(ctx, c.Delivery); err != nil {
		return blocked(err.Error())
	}
	catalog, err := s.provider.Catalog(ctx, token)
	if err != nil || catalog == nil {
		state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
		return blocked("无法读取号商报价")
	}
	wallet, key, err := s.provider.Identity(ctx, token)
	if err != nil || wallet == nil || key == nil {
		state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
		return blocked("无法验证号商余额和 API Key")
	}
	state.Catalog, state.Wallet, state.Key = catalog, wallet, key
	state.CatalogAt, state.IdentityAt = &now, &now
	if key.Revoked || (!key.ExpiresAt.IsZero() && !key.ExpiresAt.After(time.Now())) {
		return blocked("号商 API Key 已失效")
	}
	quotes := make([]RefillQuote, 0, schedule.Quantity)
	total := 0.0
	for i := 0; i < schedule.Quantity; i++ {
		plan := refillNextPlan(c, state.RotationCursor+i)
		if schedule.Plan != "" {
			plan = schedule.Plan
		}
		index := slices.IndexFunc(catalog.Items, func(q RefillQuote) bool { return q.Service == "refill" && q.Plan == plan && q.Enabled })
		if index < 0 {
			return blocked("所选补号套餐暂不可用")
		}
		quote := catalog.Items[index]
		if math.IsNaN(quote.Price) || math.IsInf(quote.Price, 0) || quote.Price <= 0 || quote.Price > c.MaxUnitPrice {
			return blocked("当前单价超过允许上限或报价无效")
		}
		quotes = append(quotes, quote)
		total += quote.Price
	}
	day := now.In(refillTimezone).Format("2006-01-02")
	usage := state.Usage[day]
	if usage.Count+len(quotes) > c.DailyMaxCount || usage.Points+total > c.DailyMaxPoints+1e-8 {
		return blocked("本日数量或积分预算不足以创建整批任务")
	}
	unsubmitted, err := s.unsubmittedPoints(ctx, state)
	if err != nil {
		return blocked(err.Error())
	}
	available := math.Min(catalog.Available, wallet.Available)
	if math.IsNaN(available) || math.IsInf(available, 0) || available+1e-8 < total+unsubmitted {
		return blocked("号商可用积分不足以创建整批任务（含本地预占）")
	}
	batchID := uuid.NewString()
	jobs := make([]AccountRefillJob, 0, len(quotes))
	grace := c.ImportExpiryGraceMinutes
	for _, quote := range quotes {
		jobs = append(jobs, AccountRefillJob{ID: uuid.NewString(), DailyBatchID: batchID, Force: true,
			Status: "submitting", Mode: "daily", Reason: "每日定时强制补号", Plan: quote.Plan, Price: quote.Price, PriceVersion: quote.PriceVersion,
			ConfigRevision: state.Revision, RotationGeneration: state.RotationGeneration,
			ReasonDetail:       fmt.Sprintf("北京时间 %s 定时补号，共 %d 个；忽略健康储备、待核对、在途上限和普通冷却。", due.Format("2006-01-02 15:04"), len(quotes)),
			ExpiryGraceMinutes: &grace, GroupID: c.GroupID, Concurrency: c.importConcurrencyForPlan(quote.Plan),
			DeliveryGroupIDs: slices.Clone(c.DeliveryGroupIDs), DeliveryConfig: c.Delivery,
			CreatedAt: now, UpdatedAt: now, NextAttemptAt: now})
		if schedule.Plan == "" {
			advanceRefillRotation(state)
		}
	}
	state.Jobs = trimAccountRefillJobs(append(jobs, state.Jobs...), now)
	usage.Count += len(jobs)
	usage.Points += total
	state.Usage[day] = usage
	state.LastSubmitAt = now
	status.LastAt, status.LastQuantity, status.LastBatchID = due, len(jobs), batchID
	status.NextAt, status.RetryAt = nextRefillDailyTime(schedule, now, due), time.Time{}
	status.Message = fmt.Sprintf("每日定时补号已创建 %d 个任务，按已保存的推送设置处理", len(jobs))
	state.Message = status.Message
	return true, persist()
}
