package service

import (
	"slices"
	"strings"
)

const refillReplacementCreatedMessage = "号商确认补号失败，预算预占已释放；已创建同套餐替补任务"

func refillSupplierTaskID(job AccountRefillJob) string {
	if job.SupplierID != "" {
		return job.SupplierID
	}
	return "号商暂未返回 ID"
}

// Resolve historical internal references when reading, without rewriting stored
// jobs or changing the IDs used by idempotency, retries and delivery imports.
func refillJobsForDisplay(jobs []AccountRefillJob) []AccountRefillJob {
	supplierIDs := make(map[string]string, len(jobs))
	for _, job := range jobs {
		supplierIDs[job.ID] = refillSupplierTaskID(job)
	}
	reference := func(id string) string {
		if supplierID, ok := supplierIDs[id]; ok {
			return supplierID
		}
		return "号商 ID 不可用（历史记录已清理）"
	}
	result := slices.Clone(jobs)
	for i := range result {
		job := &result[i]
		// Historical 1208 jobs used expires_at for the timestamp returned by 1208.
		if job.Provider == "" && job.SupplierExpiresAt == nil && job.ExpiresAt != nil && *job.ExpiresAt > 0 && *job.ExpiresAt <= 253402214399 {
			job.SupplierExpiresAt = job.ExpiresAt
		}
		if job.ReplacesJobID != "" {
			job.ReasonDetail = strings.Replace(job.ReasonDetail,
				"号商已确认原任务 "+job.ReplacesJobID+" 失败",
				"号商已确认原任务 "+reference(job.ReplacesJobID)+" 失败", 1)
		}
		if job.ReplacementJobID != "" && strings.HasPrefix(job.Error, refillReplacementCreatedMessage) {
			job.Error = refillReplacementCreatedMessage + " " + reference(job.ReplacementJobID)
		}
	}
	return result
}
