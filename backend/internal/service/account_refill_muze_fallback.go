package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

type AccountRefillMuzeFallbackConfig struct {
	Enabled          bool `json:"enabled"`
	FailureThreshold int  `json:"failure_threshold"`
	TimeoutMinutes   int  `json:"timeout_minutes"`
}

func (c *AccountRefillMuzeFallbackConfig) enabled() bool { return c != nil && c.Enabled }

func (c *AccountRefillMuzeFallbackConfig) validate() error {
	if c != nil && (c.FailureThreshold < 1 || c.FailureThreshold > 100 || c.TimeoutMinutes < 1 || c.TimeoutMinutes > 1440) {
		return errors.New("备用补号：连续失败次数须为 1–100，等待时间须为 1–1440 分钟")
	}
	return nil
}

type accountRefillMuzeFallbackState struct {
	Shortage      *refillSupplyShortage `json:"supply_shortage,omitempty"`
	Since         time.Time             `json:"since"`
	LastAt        *time.Time            `json:"last_at,omitempty"`
	NextAttemptAt time.Time             `json:"next_attempt_at"`
	Message       string                `json:"message"`
}

type AccountRefillMuzeFallbackStatus struct {
	ConsecutiveFailures int        `json:"consecutive_failures"`
	LastAt              *time.Time `json:"last_at"`
	Message             string     `json:"message"`
}

func refillMuzeFallbackStatus(state *accountRefillState) AccountRefillMuzeFallbackStatus {
	failures := refillMuzeFailures(state)
	count := len(failures)
	if shortage := state.MuzeFallback.Shortage; shortage != nil && shortage.BackupJobID == "" {
		count = max(count, shortage.Checks)
	}
	return AccountRefillMuzeFallbackStatus{count, state.MuzeFallback.LastAt, state.MuzeFallback.Message}
}

// Manual purchases and independent daily batches never drive this automatic
// rule. Unknown HTTP outcomes are not confirmed failures; their durable order
// creation time is handled by the independent timeout condition instead.
func refillPrimaryAutomaticJob(state *accountRefillState, j AccountRefillJob) bool {
	return j.Provider == state.Config.automaticProvider() && !j.isBackup() && j.ManualRequestID == "" && j.DailyBatchID == "" && j.Reason != "手动补号" &&
		j.GroupID == state.Config.GroupID && j.Mode == state.Config.Mode && j.RotationGeneration == state.RotationGeneration
}

func confirmedRefillFailure(j AccountRefillJob) bool {
	if j.Status != "failed" {
		return false
	}
	if j.Provider == refillProviderMuze {
		return j.SupplierStatus == "rejected" || j.SupplierID != "" && (j.SupplierStatus == "failed" || j.SupplierStatus == "returned" || j.SupplierRefundPhase == "returned")
	}
	return j.Provider == "" && j.SupplierStatus == "failed" && j.BudgetReleased && j.SupplierID != ""
}

func refillMuzeFailures(state *accountRefillState) []AccountRefillJob {
	cutoff := state.MuzeFallback.Since
	if state.MuzeFallback.LastAt != nil && state.MuzeFallback.LastAt.After(cutoff) {
		cutoff = *state.MuzeFallback.LastAt
	}
	for _, j := range state.Jobs {
		// Any completed delivery for this rule breaks the failure streak,
		// including manual and daily orders from either supplier.
		if j.GroupID == state.Config.GroupID && j.Status == "imported" && j.AccountID != 0 && j.FinishedAt != nil && j.FinishedAt.After(cutoff) {
			cutoff = *j.FinishedAt
		}
	}
	var failures []AccountRefillJob
	for _, j := range state.Jobs {
		if refillPrimaryAutomaticJob(state, j) && j.MuzeFallbackJobID == "" && j.AccountID == 0 &&
			confirmedRefillFailure(j) &&
			j.FinishedAt != nil && j.FinishedAt.After(cutoff) {
			failures = append(failures, j)
		}
	}
	slices.SortFunc(failures, func(a, b AccountRefillJob) int { return a.FinishedAt.Compare(*b.FinishedAt) })
	return failures
}

func refillMuzeStalled(state *accountRefillState, j AccountRefillJob, now time.Time) bool {
	return state.Config.MuzeFallback != nil && refillPrimaryAutomaticJob(state, j) && j.MuzeFallbackJobID == "" &&
		j.pending() && !j.awaitingStudioReceipt() && j.AccountID == 0 && !j.CreatedAt.IsZero() &&
		!now.Before(j.CreatedAt.Add(time.Duration(state.Config.MuzeFallback.TimeoutMinutes)*time.Minute))
}

// Keep the full capacity/lease calculation, including other groups' in-flight
// orders. Ignore only the primary orders this backup can actually replace.
func (s *AccountRefillService) muzeFallbackDemand(ctx context.Context, state *accountRefillState, now time.Time) (*accountRefillState, error) {
	probe := *state
	probe.Jobs = slices.Clone(state.Jobs)
	probe.ExpiryDemands = slices.Clone(state.ExpiryDemands)
	for i := range probe.Jobs {
		if refillMuzeStalled(state, probe.Jobs[i], now) {
			probe.Jobs[i].MuzeFallbackJobID = "fallback-evaluation"
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

func (s *AccountRefillService) runMuzeFallback(ctx context.Context, state *accountRefillState, now time.Time, persist func() error) (bool, error) {
	c := state.Config
	if !c.MuzeFallback.enabled() || !c.Enabled || !c.MonitorEnabled || !c.PushEnabled {
		return false, nil
	}
	if err := c.Validate(); err != nil {
		return false, nil // Normal automation reports invalid configuration.
	}
	// A backup with an unknown result must be reconciled, never multiplied.
	for _, j := range state.Jobs {
		if j.isBackup() && j.pending() {
			return false, nil
		}
	}
	failures := refillMuzeFailures(state)
	var source *AccountRefillJob
	reason := ""
	if len(failures) >= c.MuzeFallback.FailureThreshold {
		source = &failures[len(failures)-1]
		reason = fmt.Sprintf("主号商自动补号连续失败 %d 次", len(failures))
	}
	for i := range state.Jobs {
		j := &state.Jobs[i]
		if refillMuzeStalled(state, *j, now) && (source == nil || source.Status == "failed" || j.CreatedAt.Before(source.CreatedAt)) {
			source = j
			reason = fmt.Sprintf("主号商自动补号超过 %d 分钟仍未导入", c.MuzeFallback.TimeoutMinutes)
		}
	}
	if source == nil || now.Before(state.MuzeFallback.NextAttemptAt) {
		return false, nil
	}
	sourceJob := *source
	blocked := func(message string) (bool, error) {
		state.MuzeFallback.Message = message
		state.MuzeFallback.NextAttemptAt = time.Now().Add(time.Minute)
		return false, persist()
	}
	provider := c.backupProvider()
	if now.Before(state.providerBackoff(provider)) {
		return blocked(refillProviderName(provider) + " 正在退避，稍后重新检查备用补号")
	}
	probe, err := s.muzeFallbackDemand(ctx, state, now)
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
	quote, connectionID, err := s.routedQuote(ctx, state, provider, "pro20x", persist)
	if err != nil {
		return blocked(err.Error())
	}
	// Supplier quota requests can be slow. Recheck the actual gap immediately
	// before reserving the prepaid purchase, under the shared database lock.
	now = time.Now()
	probe, err = s.muzeFallbackDemand(ctx, state, now)
	if err != nil {
		return blocked(err.Error())
	}
	if probe.Evaluation.Needed <= 0 {
		return false, nil
	}
	day := now.In(refillTimezone).Format("2006-01-02")
	usage := state.Usage[day]
	if usage.Count >= c.DailyMaxCount || usage.Points+quote.Price > c.DailyMaxPoints+1e-8 {
		return blocked("备用补号已达到本日数量或积分上限")
	}
	grace := c.ImportExpiryGraceMinutes
	job := AccountRefillJob{ID: uuid.NewString(), Provider: provider, Price: quote.Price, PriceVersion: quote.PriceVersion, FallbackForJobID: sourceJob.ID,
		MuzeConnectionID: connectionID, MuzeProductID: quote.muzeProductID, MuzeStudio: quote.muzeStudio, ConfigRevision: state.Revision,
		Status: "submitting", Plan: "pro20x", Mode: c.Mode, RotationGeneration: state.RotationGeneration,
		Reason: refillProviderName(provider) + "自动备用补号", ReasonDetail: fmt.Sprintf("%s。%s 原任务 %s 继续核对，%s 补入 1 个 Pro20x。", reason, refillProviderName(sourceJob.Provider), refillSupplierTaskID(sourceJob), refillProviderName(provider)),
		GroupID: c.GroupID, Concurrency: c.importConcurrencyForPlan("pro20x"), DeliveryGroupIDs: slices.Clone(c.DeliveryGroupIDs),
		DeliveryConfig: c.Delivery, ExpiryGraceMinutes: &grace, CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
	for i := range state.Jobs {
		j := &state.Jobs[i]
		if refillMuzeStalled(state, *j, now) || slices.ContainsFunc(failures, func(f AccountRefillJob) bool { return f.ID == j.ID }) {
			j.MuzeFallbackJobID = job.ID
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
	usage.Count++
	usage.Points += quote.Price
	state.Usage[day] = usage
	state.Jobs = trimAccountRefillJobs(append([]AccountRefillJob{job}, state.Jobs...), now)
	state.LastSubmitAt = now
	state.MuzeFallback.Shortage = nil
	state.MuzeFallback.LastAt = &now
	state.MuzeFallback.NextAttemptAt = time.Time{}
	state.MuzeFallback.Message = reason + "，已创建 " + refillProviderName(provider) + " Pro20x 备用订单"
	state.Message = state.MuzeFallback.Message
	if err := persist(); err != nil {
		return true, err
	}
	var token string
	if provider == "" {
		token, err = s.encryptor.Decrypt(state.EncryptedToken)
		if err != nil {
			return true, err
		}
	}
	return true, s.submitRefillReservation(ctx, state, &state.Jobs[0], token, persist)
}
