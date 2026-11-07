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
	if job.ManualRequestID != "" || job.DailyBatchID != "" || job.Reason == "手动补号" || job.AccountID != 0 {
		return
	}
	for _, other := range state.Jobs {
		if other.TimeoutRetryOf == job.ID {
			job.Error += "；已创建超时补单，不重复安排失败重补"
			return
		}
	}
	if job.RotationGeneration != state.RotationGeneration {
		job.Error += "；轮转规则已重设，取消原轮次重补"
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
	if job.Mode == "interval" && state.Config.Mode == "interval" && job.TimeoutRetryOf == "" {
		state.IntervalRemaining++
	}
}

func nextRefillFailureReplacement(state *accountRefillState) int {
	selected := -1
	for i, j := range state.Jobs {
		if !j.ReplacementPending || j.ReplacementJobID != "" || j.Status != "failed" ||
			j.SupplierStatus != "failed" || j.SupplierID == "" || !j.BudgetReleased ||
			j.ManualRequestID != "" || j.Reason == "手动补号" || j.AccountID != 0 ||
			j.GroupID != state.Config.GroupID || j.Mode != state.Config.Mode ||
			j.RotationGeneration != state.RotationGeneration {
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

// A load dip pauses purchasing, but cannot complete an unsuccessful rotation
// slot. Only an explicit rule reset cancels the original-plan replacement.
func reconcileRefillFailureReplacements(state *accountRefillState) {
	for i := range state.Jobs {
		j := &state.Jobs[i]
		if j.ReplacementPending && (j.GroupID != state.Config.GroupID || j.Mode != state.Config.Mode || j.RotationGeneration != state.RotationGeneration) {
			j.ReplacementPending = false
			j.Error = "号商确认补号失败，预算预占已释放；规则已切换或轮转已重设，取消原套餐重补"
		}
	}
}

func linkRefillFailureReplacement(state *accountRefillState, index int, job *AccountRefillJob) {
	failed := &state.Jobs[index]
	job.ReplacesJobID = failed.ID
	job.ReasonDetail = fmt.Sprintf("号商已确认原任务 %s 失败，沿用 %s 套餐自动重补，不推进套餐轮转。本次需求：%s", refillSupplierTaskID(*failed), failed.Plan, job.ReasonDetail)
	failed.ReplacementPending = false
	failed.ReplacementJobID = job.ID
	failed.Error = refillReplacementCreatedMessage + " " + refillSupplierTaskID(*job)
}
