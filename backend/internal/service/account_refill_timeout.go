package service

import (
	"context"
	"fmt"
	"slices"
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

// Use creation times, never UpdatedAt: polling or changing supplier status is
// not delivery. A fresh order for this pool restarts the wait across all rules.
// Timed-out orders remain pending, keep their budget, and continue reconciling.
func refillTimeoutCandidate(state *accountRefillState, jobs []AccountRefillJob, now time.Time) *AccountRefillJob {
	c := state.Config
	if !c.Enabled || !c.MonitorEnabled || !c.PushEnabled || !refillTimeoutRetryEnabled(c) {
		return nil
	}
	deadline := state.LastSubmitAt.Add(refillTimeoutRetryDuration(c))
	var candidate *AccountRefillJob
	for i := range jobs {
		j := &jobs[i]
		if !j.pending() || j.AccountID != 0 || j.CreatedAt.IsZero() {
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
		copy := *job
		state.Evaluation.timeoutRetryJob = &copy
	}
}

// A long monitoring interval must not postpone a due rescue. After evaluating
// an overdue order, defer to normal checks (or one timeout interval) when there
// is no shortage or the purchase is blocked by budget/configuration.
func refillTimeoutCheckDue(state *accountRefillState, now time.Time) bool {
	job := refillTimeoutCandidate(state, state.Jobs, now)
	if job == nil {
		return false
	}
	deadline := job.CreatedAt.Add(refillTimeoutRetryDuration(state.Config))
	if due := state.LastSubmitAt.Add(refillTimeoutRetryDuration(state.Config)); due.After(deadline) {
		deadline = due
	}
	return state.TimeoutCheckedAt.Before(deadline) || now.Sub(state.TimeoutCheckedAt) >= refillTimeoutRetryDuration(state.Config)
}

func refillTimeoutReasonDetail(state *accountRefillState) string {
	return fmt.Sprintf("号池健康储备为 0，补号超过 %d 分钟仍未导入；忽略待核对阻塞、在途上限、普通冷却和时段，按原套餐重新购买 1 个账号。原任务 %s 继续核对，原单可能仍交付并计费。",
		int(refillTimeoutRetryDuration(state.Config)/time.Minute), state.Evaluation.TimeoutRetryJobID)
}

func refillTimeoutNotifyEnabled(c AccountRefillConfig) bool {
	return c.TimeoutRetryNotify == nil || *c.TimeoutRetryNotify
}

type refillTimeoutNotice struct {
	job          AccountRefillJob
	lastSubmitAt time.Time
}

func captureRefillTimeoutNotice(state *accountRefillState) *refillTimeoutNotice {
	if state.Evaluation.timeoutRetryJob == nil {
		return nil
	}
	return &refillTimeoutNotice{job: *state.Evaluation.timeoutRetryJob, lastSubmitAt: state.LastSubmitAt}
}

// Persist the cycle and retry deadline before sending. A failed alert must not
// block purchasing or flood the robot on every worker tick.
func (s *AccountRefillService) notifyRefillTimeoutRetry(ctx context.Context, state *accountRefillState, notice *refillTimeoutNotice, started time.Time, persist func() error) error {
	if !refillTimeoutNotifyEnabled(state.Config) || state.EncryptedWebhook == "" {
		return nil
	}
	now := time.Now()
	if notice == nil {
		// Retry the saved alert even after a rescue has reset the pool deadline.
		if state.TimeoutNoticeContent == "" || state.TimeoutNoticeRetryAt.IsZero() || now.Before(state.TimeoutNoticeRetryAt) {
			return nil
		}
		state.TimeoutNoticeRetryAt = now.Add(5 * time.Minute)
		if err := persist(); err != nil {
			return err
		}
		if s.sendRefillNotification(ctx, state, state.TimeoutNoticeContent) {
			state.TimeoutNoticeRetryAt = time.Time{}
		}
		return persist()
	}
	key := notice.job.ID + ":" + notice.lastSubmitAt.UTC().Format(time.RFC3339Nano)
	result := "尚未创建新订单：" + state.Message
	for _, job := range state.Jobs {
		if job.DailyBatchID != "" && !job.CreatedAt.Before(started) {
			key += ":" + job.DailyBatchID
			result = fmt.Sprintf("本轮每日定时补号已创建 %d 个新任务，继续等待导入。原订单仍保留核对。", state.DailyScheduleStatus.LastQuantity)
			break
		}
		if job.TimeoutRetryOf == notice.job.ID && !job.CreatedAt.Before(started) {
			key += ":" + job.ID
			result = fmt.Sprintf("已创建超时补单，套餐 %s，预占 %g 积分；新号商任务：%s；当前状态：%s。原订单保留核对，仍可能交付并计费。", job.Plan, job.Price, refillSupplierTaskID(job), job.Status)
			break
		}
	}
	if state.TimeoutNoticeKey == key && (state.TimeoutNoticeRetryAt.IsZero() || now.Before(state.TimeoutNoticeRetryAt)) {
		return nil
	}
	state.TimeoutNoticeKey, state.TimeoutNoticeRetryAt = key, now.Add(5*time.Minute)
	content := fmt.Sprintf("Sub2API 补号超时重试提醒\n监测分组：#%d\n健康储备：0 个\n超时阈值：%d 分钟\n原号商任务：%s\n原任务状态：%s\n已等待：%s\n处理结果：%s\n时间：%s",
		state.Config.GroupID, int(refillTimeoutRetryDuration(state.Config)/time.Minute), refillSupplierTaskID(notice.job), notice.job.Status,
		now.Sub(notice.job.CreatedAt).Round(time.Second), result, now.In(refillTimezone).Format("2006-01-02 15:04:05"))
	state.TimeoutNoticeContent = content
	if err := persist(); err != nil {
		return err
	}
	if s.sendRefillNotification(ctx, state, content) {
		state.TimeoutNoticeRetryAt = time.Time{}
	}
	return persist()
}
