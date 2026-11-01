package service

import (
	"fmt"
	"sort"
	"strings"
)

// Capture the final pre-purchase evaluation. Later polling, configuration edits
// and retries must not rewrite why this particular order was reserved.
func accountRefillReasonDetail(c AccountRefillConfig, e AccountRefillEvaluation) string {
	switch e.Reason {
	case "定时补号":
		return fmt.Sprintf("定时周期已到：每 %d 分钟补 %d 个，本次提交 1 个。", c.IntervalMinutes, c.IntervalQuantity)
	case "并发利用率超过目标":
		utilization := float64(e.InUse) * 100 / float64(refillMax(1, e.Capacity))
		return fmt.Sprintf("运行中并发 %d / %d（利用率 %.2f%%），达到设定 %d%%，持续观察已满 %d 秒。", e.InUse, e.Capacity, utilization, c.TargetConcurrency, c.ConcurrencySustainSeconds)
	case refillForecastReason:
		capacity := e.ForecastCapacity + e.PendingCapacity
		utilization := "无剩余可用容量"
		if capacity > 0 {
			utilization = fmt.Sprintf("预计利用率 %.2f%%", float64(e.InUse)*100/float64(capacity))
		}
		return fmt.Sprintf("%d 个账号已达剩余额度下限或将在 %d 分钟内到租期；保留全部运行中并发 %d，容量由 %d 预判为 %d，另抵扣在途容量 %d（%s），达到设定 %d%%，持续观察已满 %d 秒。原账号调度不变。", e.ForecastAccounts, c.LeadMinutes, e.InUse, e.Capacity, e.ForecastCapacity, e.PendingCapacity, utilization, c.TargetConcurrency, c.ConcurrencySustainSeconds)
	case "健康储备不足", "账号临期，提前补充储备":
		counts := map[string]int{}
		for _, account := range e.Accounts {
			counts[account.State]++
		}
		detail := fmt.Sprintf("未来健康储备 %d 个 + 在途 %d 个，低于最低储备 %d 个。", e.Ready, e.Pending, c.MinReady)
		var excluded []string
		if n := counts["expired"]; n > 0 {
			excluded = append(excluded, fmt.Sprintf("%d 个已到租期", n))
		}
		if n := counts["expiring"]; n > 0 {
			excluded = append(excluded, fmt.Sprintf("%d 个将在 %d 分钟内到期", n, c.LeadMinutes))
		}
		if n := counts["quota_low"]; n > 0 {
			if len(c.PlanQuotaThresholds) == 0 {
				excluded = append(excluded, fmt.Sprintf("%d 个额度低于下限（5h %.2f%% / 7d %.2f%%）", n, c.MinRemaining5h, c.MinRemaining7d))
			} else {
				byThreshold := map[string]int{}
				for _, account := range e.Accounts {
					if account.State == "quota_low" {
						plan := account.Plan
						if plan == "" {
							plan = "未识别套餐"
						}
						byThreshold[fmt.Sprintf("%s 额度低于下限（5h %.2f%% / 7d %.2f%%）", plan, account.MinRemaining5h, account.MinRemaining7d)]++
					}
				}
				keys := make([]string, 0, len(byThreshold))
				for key := range byThreshold {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				for _, key := range keys {
					excluded = append(excluded, fmt.Sprintf("%d 个 %s", byThreshold[key], key))
				}
			}
		}
		if n := counts["unavailable"]; n > 0 {
			excluded = append(excluded, fmt.Sprintf("%d 个暂不可调度", n))
		}
		if len(excluded) > 0 {
			detail += "未计入储备：" + strings.Join(excluded, "；") + "。"
		}
		return detail
	default:
		return ""
	}
}
