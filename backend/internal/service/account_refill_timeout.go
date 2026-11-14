package service

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

const defaultRefillTimeoutRetryMinutes = 10
const refillTimeoutRetryReason = "补号超时，保底补入 1 个账号"

func refillTimeoutRetryEnabled(c AccountRefillConfig) bool {
	return c.TimeoutRetryEnabled == nil || *c.TimeoutRetryEnabled
}

func refillTimeoutRetryDuration(c AccountRefillConfig) time.Duration {
	minutes := defaultRefillTimeoutRetryMinutes
	if c.TimeoutRetryMinutes != nil {
		minutes = *c.TimeoutRetryMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// Each order has its own deadline. Polling, status updates and newer orders
// cannot extend it. Ignore overdue orders only for demand and capacity, never
// for reconciliation or financial reservations.
func refillJobTimedOut(c AccountRefillConfig, job AccountRefillJob, now time.Time) bool {
	return job.pending() && job.AccountID == 0 && (job.FenduduFallbackJobID != "" ||
		refillTimeoutRetryEnabled(c) && !job.CreatedAt.IsZero() && !now.Before(job.CreatedAt.Add(refillTimeoutRetryDuration(c))))
}

func markRefillTimeouts(state *accountRefillState, now time.Time) bool {
	if !state.Config.Enabled || !state.Config.MonitorEnabled || !state.Config.PushEnabled {
		return false
	}
	changed := false
	for i := range state.Jobs {
		job := &state.Jobs[i]
		if job.TimeoutIgnoredAt == nil && refillJobTimedOut(state.Config, *job, now) {
			job.TimeoutIgnoredAt = &now
			changed = true
		}
	}
	return changed
}

// The zero-reserve fallback waits for fresh orders to reach their own deadline.
// Normal automation separately ignores each expired order, even with reserves
// or newer pending orders present.
func refillTimeoutCandidate(state *accountRefillState, jobs []AccountRefillJob, now time.Time) *AccountRefillJob {
	c := state.Config
	if !c.Enabled || !c.MonitorEnabled || !c.PushEnabled || !refillTimeoutRetryEnabled(c) {
		return nil
	}
	deadline := state.LastSubmitAt.Add(refillTimeoutRetryDuration(c))
	var candidate *AccountRefillJob
	for i := range jobs {
		j := &jobs[i]
		if !j.pending() || j.AccountID != 0 || j.CreatedAt.IsZero() || j.FenduduFallbackJobID != "" {
			continue
		}
		if due := j.CreatedAt.Add(refillTimeoutRetryDuration(c)); due.After(deadline) {
			deadline = due
		}
		if candidate == nil || j.CreatedAt.After(candidate.CreatedAt) {
			candidate = j
		}
	}
	if candidate == nil || now.Before(deadline) || !slices.Contains([]string{"pro5x", "pro20x"}, candidate.Plan) {
		return nil
	}
	return candidate
}

func applyRefillTimeoutRetry(state *accountRefillState, jobs []AccountRefillJob, now time.Time) {
	if state.Evaluation.Ready > 0 || state.Evaluation.Error != "" {
		return
	}
	if job := refillTimeoutCandidate(state, jobs, now); job != nil {
		state.Evaluation.Needed = 1
		state.Evaluation.Reason = refillTimeoutRetryReason
		state.Evaluation.TimeoutRetryJobID = job.ID
		state.Evaluation.TimeoutRetryPlan = job.Plan
	}
}

// A long monitoring interval must not postpone removing a newly expired order
// from the demand calculation. New submissions never postpone older deadlines.
func refillTimeoutCheckDue(state *accountRefillState, now time.Time) bool {
	c := state.Config
	if !c.Enabled || !c.MonitorEnabled || !c.PushEnabled {
		return false
	}
	for _, job := range state.Jobs {
		if refillJobTimedOut(c, job, now) && (state.TimeoutCheckedAt.Before(job.CreatedAt.Add(refillTimeoutRetryDuration(c))) ||
			now.Sub(state.TimeoutCheckedAt) >= refillTimeoutRetryDuration(c)) {
			return true
		}
	}
	return false
}

func refillTimeoutReasonDetail(state *accountRefillState) string {
	return fmt.Sprintf("号池健康储备为 0，补号超过 %d 分钟仍未导入；忽略待核对阻塞、在途上限、普通冷却和时段，按原套餐重新购买 1 个账号。原任务 %s 继续核对，原单可能仍交付并计费。",
		int(refillTimeoutRetryDuration(state.Config)/time.Minute), state.Evaluation.TimeoutRetryJobID)
}

func refillTimeoutNotifyEnabled(c AccountRefillConfig) bool {
	return c.TimeoutRetryNotify == nil || *c.TimeoutRetryNotify
}

type refillTimeoutNotice struct {
	jobs  []AccountRefillJob
	ready int
}

func captureRefillTimeoutNotice(state *accountRefillState, now time.Time) *refillTimeoutNotice {
	c := state.Config
	if !c.Enabled || !c.MonitorEnabled || !c.PushEnabled {
		return nil
	}
	notice := &refillTimeoutNotice{ready: state.Evaluation.Ready}
	for _, job := range state.Jobs {
		if job.TimeoutNotificationAt == nil && refillJobTimedOut(c, job, now) {
			notice.jobs = append(notice.jobs, job)
			// Keep each WeCom message bounded. The next tick picks up the rest.
			if len(notice.jobs) == 3 {
				break
			}
		}
	}
	if len(notice.jobs) == 0 {
		return nil
	}
	return notice
}

// Persist the cycle and retry deadline before sending. A failed alert must not
// block purchasing or flood the robot on every worker tick.
func (s *AccountRefillService) notifyRefillTimeoutRetry(ctx context.Context, state *accountRefillState, notice *refillTimeoutNotice, started time.Time, persist func() error) error {
	if !refillTimeoutNotifyEnabled(state.Config) || state.EncryptedWebhook == "" {
		return nil
	}
	now := time.Now()
	if state.TimeoutNoticeContent != "" && !state.TimeoutNoticeRetryAt.IsZero() {
		ids := strings.Split(state.TimeoutNoticeKey, ":")
		unresolved := map[string]bool{}
		for _, job := range state.Jobs {
			if slices.Contains(ids, job.ID) && refillJobTimedOut(state.Config, job, now) {
				unresolved[job.ID] = true
			}
		}
		if len(unresolved) != len(ids) {
			// A queued message must not report completed jobs as still blocked.
			// Rebuild only its remaining unresolved entries from current state.
			for i := range state.Jobs {
				if unresolved[state.Jobs[i].ID] {
					state.Jobs[i].TimeoutNotificationAt = nil
				}
			}
			state.TimeoutNoticeKey, state.TimeoutNoticeContent = "", ""
			state.TimeoutNoticeRetryAt = time.Time{}
			notice = captureRefillTimeoutNotice(state, now)
			if err := persist(); err != nil {
				return err
			}
		}
	}
	if state.TimeoutNoticeContent != "" && !state.TimeoutNoticeRetryAt.IsZero() {
		// Keep an unsent alert even when other orders reach their deadlines.
		if now.Before(state.TimeoutNoticeRetryAt) {
			return nil
		}
		state.TimeoutNoticeRetryAt = now.Add(5 * time.Minute)
		if err := persist(); err != nil {
			return err
		}
		sent := s.sendRefillNotification(ctx, state, state.TimeoutNoticeContent)
		if sent {
			state.TimeoutNoticeRetryAt = time.Time{}
		}
		if err := persist(); err != nil || !sent {
			return err
		}
	}
	if notice == nil {
		return nil
	}
	var current []AccountRefillJob
	for _, captured := range notice.jobs {
		for _, job := range state.Jobs {
			if job.ID == captured.ID && refillJobTimedOut(state.Config, job, now) {
				current = append(current, job)
				break
			}
		}
	}
	if len(current) == 0 {
		return nil
	}
	notice.jobs = current
	result := "尚未创建新订单：" + state.Message
	for _, job := range state.Jobs {
		if job.DailyBatchID != "" && !job.CreatedAt.Before(started) {
			result = fmt.Sprintf("本轮每日定时补号已创建 %d 个新任务，继续等待导入。原订单仍保留核对。", state.DailyScheduleStatus.LastQuantity)
			break
		}
		if job.ManualRequestID == "" && !job.CreatedAt.Before(started) {
			label := "自动补号已创建新订单"
			if job.TimeoutRetryOf != "" {
				label = "已创建超时补单"
			}
			result = fmt.Sprintf("%s，套餐 %s，预占 %g 积分；新号商任务：%s；当前状态：%s。", label, job.Plan, job.Price, refillSupplierTaskID(job), job.Status)
			break
		}
	}
	ids, details := make([]string, 0, len(notice.jobs)), make([]string, 0, len(notice.jobs))
	for _, job := range notice.jobs {
		ids = append(ids, job.ID)
		details = append(details, fmt.Sprintf("方式：%s；套餐：%s；原号商任务：%s；状态：%s；已等待：%s", refillNotificationMode(&job), job.Plan, refillSupplierTaskID(job), job.Status, now.Sub(job.CreatedAt).Round(time.Second)))
		for i := range state.Jobs {
			if state.Jobs[i].ID == job.ID {
				state.Jobs[i].TimeoutNotificationAt = &now
			}
		}
	}
	slices.Sort(ids)
	state.TimeoutNoticeKey, state.TimeoutNoticeRetryAt = strings.Join(ids, ":"), now.Add(5*time.Minute)
	content := fmt.Sprintf("Sub2API 补号超时重试提醒\n监测分组：#%d\n健康储备：%d 个\n超时阈值：%d 分钟\n%s\n处理：超时订单不再阻塞自动补号，也不占补号缺口和在途名额，无需等待原单处理。\n处理结果：%s\n旧单仍会核对，可能后续交付并计费。\n时间：%s",
		state.Config.GroupID, notice.ready, int(refillTimeoutRetryDuration(state.Config)/time.Minute), strings.Join(details, "\n"), result, now.In(refillTimezone).Format("2006-01-02 15:04:05"))
	state.TimeoutNoticeContent = content
	if err := persist(); err != nil {
		return err
	}
	if s.sendRefillNotification(ctx, state, content) {
		state.TimeoutNoticeRetryAt = time.Time{}
	}
	return persist()
}
