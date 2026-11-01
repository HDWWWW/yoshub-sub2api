package service

import (
	"errors"
	"math"
	"strings"
)

type AccountRefillQuotaThreshold struct {
	MinRemaining5h float64 `json:"min_remaining_5h"`
	MinRemaining7d float64 `json:"min_remaining_7d"`
}

func validateRefillPlanQuotas(c AccountRefillConfig) error {
	for plan, threshold := range c.PlanQuotaThresholds {
		if plan != "pro5x" && plan != "pro20x" {
			return errors.New("套餐额度阈值仅支持 Pro5x 和 Pro20x")
		}
		for _, value := range []float64{threshold.MinRemaining5h, threshold.MinRemaining7d} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
				return errors.New("套餐剩余额度下限须为 0–100%")
			}
		}
	}
	return nil
}

func normalizeRefillPlan(value string) string {
	value = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	switch value {
	case "pro5", "pro5x", "prolite":
		return "pro5x"
	case "pro20", "pro20x", "pro", "chatgptpro":
		return "pro20x"
	}
	return ""
}

func refillAccountPlan(a *Account, jobs []AccountRefillJob) string {
	if value, _ := a.Extra["refill_plan"].(string); normalizeRefillPlan(value) != "" {
		return normalizeRefillPlan(value)
	}
	// Historical API imports have the original purchase in their durable jobs.
	if jobID, _ := a.Extra["refill_job_id"].(string); jobID != "" {
		for _, job := range jobs {
			if job.ID == jobID && job.AccountID == a.ID && normalizeRefillPlan(job.Plan) != "" {
				return normalizeRefillPlan(job.Plan)
			}
		}
	}
	return normalizeRefillPlan(a.GetCredential("plan_type"))
}

func refillQuotaThreshold(c AccountRefillConfig, plan string) AccountRefillQuotaThreshold {
	if override, ok := c.PlanQuotaThresholds[plan]; ok {
		return override
	}
	return AccountRefillQuotaThreshold{MinRemaining5h: c.MinRemaining5h, MinRemaining7d: c.MinRemaining7d}
}
