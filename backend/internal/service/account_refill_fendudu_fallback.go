package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

type AccountRefillFenduduFallbackConfig struct {
	Enabled          bool `json:"enabled"`
	FailureThreshold int  `json:"failure_threshold"`
	TimeoutMinutes   int  `json:"timeout_minutes"`
}

func (c *AccountRefillFenduduFallbackConfig) enabled() bool { return c != nil && c.Enabled }

func (c *AccountRefillFenduduFallbackConfig) validate() error {
	if c != nil && (c.FailureThreshold < 1 || c.FailureThreshold > 100 || c.TimeoutMinutes < 1 || c.TimeoutMinutes > 1440) {
		return errors.New("粉嘟嘟备用补号：连续失败次数须为 1–100，等待时间须为 1–1440 分钟")
	}
	return nil
}

type accountRefillFenduduFallbackState struct {
	Since         time.Time  `json:"since"`
	LastAt        *time.Time `json:"last_at,omitempty"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	Message       string     `json:"message"`
}

type AccountRefillFenduduFallbackStatus struct {
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastAt              *time.Time `json:"last_at"`
	Message             string     `json:"message"`
}

func refillFenduduFallbackStatus(state *accountRefillState) AccountRefillFenduduFallbackStatus {
	failures := refillFenduduFailures(state)
	return AccountRefillFenduduFallbackStatus{len(failures), state.FenduduFallback.LastAt, state.FenduduFallback.Message}
}

// Manual purchases and independent daily batches never drive this automatic
// rule. Unknown HTTP outcomes are not confirmed failures; their durable order
// creation time is handled by the independent timeout condition instead.
func refillPrimaryAutomaticJob(state *accountRefillState, j AccountRefillJob) bool {
	return j.Provider == "" && j.ManualRequestID == "" && j.DailyBatchID == "" && j.Reason != "手动补号" &&
		j.GroupID == state.Config.GroupID && j.Mode == state.Config.Mode && j.RotationGeneration == state.RotationGeneration
}

func refillFenduduFailures(state *accountRefillState) []AccountRefillJob {
	cutoff := state.FenduduFallback.Since
	if state.FenduduFallback.LastAt != nil && state.FenduduFallback.LastAt.After(cutoff) {
		cutoff = *state.FenduduFallback.LastAt
	}
	for _, j := range state.Jobs {
		automatic := refillPrimaryAutomaticJob(state, j) || j.Provider == refillProviderFendudu && j.FallbackForJobID != ""
		if automatic && j.Status == "imported" && j.AccountID != 0 && j.FinishedAt != nil && j.FinishedAt.After(cutoff) {
			cutoff = *j.FinishedAt
		}
	}
	var failures []AccountRefillJob
	for _, j := range state.Jobs {
		if refillPrimaryAutomaticJob(state, j) && j.FenduduFallbackJobID == "" && j.AccountID == 0 &&
			j.Status == "failed" && j.SupplierStatus == "failed" && j.BudgetReleased && j.SupplierID != "" &&
			j.FinishedAt != nil && j.FinishedAt.After(cutoff) {
			failures = append(failures, j)
		}
	}
	slices.SortFunc(failures, func(a, b AccountRefillJob) int { return a.FinishedAt.Compare(*b.FinishedAt) })
	return failures
}

func refillFenduduStalled(state *accountRefillState, j AccountRefillJob, now time.Time) bool {
	return state.Config.FenduduFallback != nil && refillPrimaryAutomaticJob(state, j) && j.FenduduFallbackJobID == "" &&
		j.pending() && j.AccountID == 0 && !j.CreatedAt.IsZero() &&
		!now.Before(j.CreatedAt.Add(time.Duration(state.Config.FenduduFallback.TimeoutMinutes)*time.Minute))
}

// Keep the full capacity/lease calculation, including other groups' in-flight
// orders. Ignore only the primary orders this backup can actually replace.
func (s *AccountRefillService) fenduduFallbackDemand(ctx context.Context, state *accountRefillState, now time.Time) (*accountRefillState, error) {
	probe := *state
	probe.Jobs = slices.Clone(state.Jobs)
	probe.ExpiryDemands = slices.Clone(state.ExpiryDemands)
	for i := range probe.Jobs {
		if refillFenduduStalled(state, probe.Jobs[i], now) {
			probe.Jobs[i].FenduduFallbackJobID = "fallback-evaluation"
		}
	}
	if err := s.evaluate(ctx, &probe, now); err != nil {
		return nil, err
	}
	if state.Config.Mode == "interval" {
		probe.Evaluation.Needed = min(1, max(0, state.Config.MaxReady-probe.Evaluation.Ready-probe.Evaluation.Pending), max(0, state.Config.MaxInFlight-probe.Evaluation.Pending))
		if probe.Evaluation.waitingForReview {
			probe.Evaluation.Needed = 0
		}
	}
	return &probe, nil
}

func (s *AccountRefillService) runFenduduFallback(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) (bool, error) {
	c := state.Config
	if !c.FenduduFallback.enabled() || !c.Enabled || !c.MonitorEnabled || !c.PushEnabled {
		return false, nil
	}
	if err := c.Validate(); err != nil {
		return false, nil // Normal automation reports invalid configuration.
	}
	// A backup with an unknown result must be reconciled, never multiplied.
	for _, j := range state.Jobs {
		if j.Provider == refillProviderFendudu && j.FallbackForJobID != "" && j.pending() {
			return false, nil
		}
	}
	failures := refillFenduduFailures(state)
	var source *AccountRefillJob
	reason := ""
	if len(failures) >= c.FenduduFallback.FailureThreshold {
		source = &failures[len(failures)-1]
		reason = fmt.Sprintf("主号商自动补号连续失败 %d 次", len(failures))
	}
	for i := range state.Jobs {
		j := &state.Jobs[i]
		if refillFenduduStalled(state, *j, now) && (source == nil || source.Status == "failed" || j.CreatedAt.Before(source.CreatedAt)) {
			source = j
			reason = fmt.Sprintf("主号商自动补号超过 %d 分钟仍未导入", c.FenduduFallback.TimeoutMinutes)
		}
	}
	if source == nil || now.Before(state.FenduduFallback.NextAttemptAt) {
		return false, nil
	}
	sourceJob := *source
	blocked := func(message string) (bool, error) {
		state.FenduduFallback.Message = message
		state.FenduduFallback.NextAttemptAt = time.Now().Add(time.Minute)
		return false, persist()
	}
	if state.Fendudu.EncryptedToken == "" {
		return blocked("备用补号等待配置粉嘟嘟连接")
	}
	if now.Before(state.Fendudu.RetryAt) {
		return blocked("粉嘟嘟正在退避，稍后重新检查备用补号")
	}
	probe, err := s.fenduduFallbackDemand(ctx, state, now)
	if err != nil {
		return blocked(err.Error())
	}
	if probe.Evaluation.Needed <= 0 {
		return false, nil
	}
	if err := s.validateGroups(ctx, c); err != nil {
		return blocked(err.Error())
	}
	if _, err := s.selectProxy(ctx, c.Delivery); err != nil {
		return blocked(err.Error())
	}
	if state.Usage[now.In(refillTimezone).Format("2006-01-02")].Count >= c.DailyMaxCount {
		return blocked("备用补号已达到本日数量上限")
	}
	if _, err := s.fenduduManualQuote(ctx, state, "pro20x", persist); err != nil {
		return blocked(err.Error())
	}
	// Supplier quota requests can be slow. Recheck the actual gap immediately
	// before reserving the prepaid purchase, under the shared database lock.
	now = time.Now()
	probe, err = s.fenduduFallbackDemand(ctx, state, now)
	if err != nil {
		return blocked(err.Error())
	}
	if probe.Evaluation.Needed <= 0 {
		return false, nil
	}
	day := now.In(refillTimezone).Format("2006-01-02")
	usage := state.Usage[day]
	if usage.Count >= c.DailyMaxCount {
		return blocked("备用补号已达到本日数量上限")
	}
	grace := c.ImportExpiryGraceMinutes
	job := AccountRefillJob{ID: uuid.NewString(), Provider: refillProviderFendudu, FallbackForJobID: sourceJob.ID,
		FenduduConnectionID: state.Fendudu.ConnectionID, ConfigRevision: state.Revision,
		Status: "submitting", Plan: "pro20x", Mode: c.Mode, RotationGeneration: state.RotationGeneration,
		Reason: "粉嘟嘟自动备用补号", ReasonDetail: fmt.Sprintf("%s；原号商任务：%s。备用补入 1 个 Pro20x，扣 1 次粉嘟嘟预购次数。原订单继续核对，仍可能交付并计费。", reason, refillSupplierTaskID(sourceJob)),
		GroupID: c.GroupID, Concurrency: c.importConcurrencyForPlan("pro20x"), DeliveryGroupIDs: slices.Clone(c.DeliveryGroupIDs),
		DeliveryConfig: c.Delivery, ExpiryGraceMinutes: &grace, CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
	for i := range state.Jobs {
		j := &state.Jobs[i]
		if refillFenduduStalled(state, *j, now) || slices.ContainsFunc(failures, func(f AccountRefillJob) bool { return f.ID == j.ID }) {
			j.FenduduFallbackJobID = job.ID
			if j.pending() {
				j.TimeoutIgnoredAt = &now
			}
			if j.ReplacementPending {
				j.ReplacementPending, j.ReplacementJobID = false, job.ID
			}
		}
	}
	state.Evaluation, state.ExpiryDemands = probe.Evaluation, probe.ExpiryDemands
	claimRefillExpiryDemand(state, &job, -1)
	if c.Mode == "interval" && sourceJob.Status == "failed" {
		state.IntervalRemaining = max(0, state.IntervalRemaining-1)
	}
	usage.Count++ // Fendudu quotas do not consume the primary supplier's points.
	state.Usage[day] = usage
	state.Jobs = trimAccountRefillJobs(append([]AccountRefillJob{job}, state.Jobs...), now)
	state.LastSubmitAt = now
	state.FenduduFallback.LastAt = &now
	state.FenduduFallback.NextAttemptAt = time.Time{}
	state.FenduduFallback.Message = reason + "；已创建粉嘟嘟 Pro20x 备用订单"
	state.Message = state.FenduduFallback.Message
	if err := persist(); err != nil {
		return true, err
	}
	return true, s.advanceFenduduJobs(ctx, state, now, persist)
}
