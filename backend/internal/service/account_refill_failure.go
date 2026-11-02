package service

import (
	"fmt"
	"time"
)

// Missing values retain the immediate retry behavior of existing rules.
func refillImmediateFailureRetryEnabled(c AccountRefillConfig) bool {
	return c.ImmediateFailureRetry == nil || *c.ImmediateFailureRetry
}

// A confirmed failure supplies the plan for an existing demand slot. It never
// adds demand or submits from a second worker. Old terminal jobs are not replayed.
func markRefillFailureReplacement(state *accountRefillState, job *AccountRefillJob) {
	if job.ManualRequestID != "" || job.Reason == "手动补号" || job.AccountID != 0 {
		return
	}
	if job.Mode == "" { // Orders accepted before this field existed.
		job.Mode = state.Config.Mode
		if job.Reason == "定时补号" {
			job.Mode = "interval"
		}
	}
	job.ReplacementPending = true
	job.Error += "；仍有补号需求时自动重补原套餐"
	// Only the enabled shortcut bypasses the ordinary monitoring deadline.
	// A disabled shortcut retains the replacement plan and interval allowance.
	if refillImmediateFailureRetryEnabled(state.Config) {
		state.NextCheckAt = time.Time{}
	}
	// The failed reservation already consumed this interval's quantity. Return
	// exactly that slot, instead of creating a separate replacement allowance.
	if job.Mode == "interval" && state.Config.Mode == "interval" {
		state.IntervalRemaining++
	}
}

func nextRefillFailureReplacement(state *accountRefillState) int {
	selected := -1
	for i, j := range state.Jobs {
		if !j.ReplacementPending || j.ReplacementJobID != "" || j.Status != "failed" ||
			j.SupplierStatus != "failed" || j.SupplierID == "" || !j.BudgetReleased ||
			j.ManualRequestID != "" || j.Reason == "手动补号" || j.AccountID != 0 ||
			j.GroupID != state.Config.GroupID || j.Mode != state.Config.Mode {
			continue
		}
		if selected < 0 || j.CreatedAt.Before(state.Jobs[selected].CreatedAt) {
			selected = i
		}
	}
	return selected
}

func nextRefillPurchasePlan(state *accountRefillState) string {
	if index := nextRefillFailureReplacement(state); index >= 0 {
		return state.Jobs[index].Plan
	}
	return refillNextPlan(state.Config, state.RotationCursor)
}

// Once the pool has recovered, stale failures must not commandeer the plan of
// an unrelated future refill. Pending deliveries and other blocking conditions
// are not proof that demand has resolved.
func reconcileRefillFailureReplacements(state *accountRefillState) {
	e := state.Evaluation
	resolved := state.Config.Mode != "interval" && e.Error == "" && e.Needed == 0 && e.Pending == 0 &&
		(e.Reason == "储备充足" || e.Reason == "已达健康账号上限")
	for i := range state.Jobs {
		j := &state.Jobs[i]
		if j.ReplacementPending && (resolved || j.GroupID != state.Config.GroupID || j.Mode != state.Config.Mode) {
			j.ReplacementPending = false
			j.Error = "号商确认补号失败，预算预占已释放；当前需求已消失或规则已切换，取消原套餐重补"
		}
	}
}

func linkRefillFailureReplacement(state *accountRefillState, index int, job *AccountRefillJob) {
	failed := &state.Jobs[index]
	job.ReplacesJobID = failed.ID
	job.ReasonDetail = fmt.Sprintf("号商已确认原任务 %s 失败，沿用 %s 套餐自动重补，不推进套餐轮转。本次需求：%s", failed.ID, failed.Plan, job.ReasonDetail)
	failed.ReplacementPending = false
	failed.ReplacementJobID = job.ID
	failed.Error = "号商确认补号失败，预算预占已释放；已创建同套餐替补任务 " + job.ID
}
