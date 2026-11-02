package service

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const refillForecastReason = "预计账号退出后并发达到目标"

// Rules saved before the switch existed retain their enabled behavior.
func refillForecastEnabled(c AccountRefillConfig) bool {
	return c.ForecastEnabled == nil || *c.ForecastEnabled
}

// The current demand is retained in full. Only future capacity is reduced;
// pending deliveries contribute their saved capacity, never live capacity.
func refillForecastHigh(c AccountRefillConfig, e AccountRefillEvaluation) bool {
	return refillForecastEnabled(c) && e.ForecastAccounts > 0 && e.InUse > 0 &&
		float64(e.InUse)*100 >= float64(e.ForecastCapacity+e.PendingCapacity)*float64(c.TargetConcurrency)
}

// Project future orders without advancing the persisted rotation or consuming
// failed-order replacements. Existing pending jobs retain their saved capacity.
func refillPlannedPurchasePlans(state *accountRefillState) []string {
	projection := *state
	projection.Jobs = slices.Clone(state.Jobs)
	plans := make([]string, max(1, state.Config.MaxReady))
	for i := range plans {
		plans[i] = nextRefillPurchasePlan(&projection)
		if replacement := nextRefillFailureReplacement(&projection); replacement >= 0 {
			projection.Jobs[replacement].ReplacementPending = false
		} else {
			advanceRefillRotation(&projection)
		}
	}
	return plans
}

func refillCapacityGap(c AccountRefillConfig, inUse, capacity int, plannedPlans ...string) int {
	if inUse <= 0 || float64(inUse)*100 < float64(capacity)*float64(c.TargetConcurrency) {
		return 0
	}
	gap := float64(inUse)*100/float64(max(1, c.TargetConcurrency)) - float64(capacity)
	added := 0
	for needed := 1; needed <= max(1, c.MaxReady); needed++ {
		plan := refillNextPlan(c, needed-1)
		if needed <= len(plannedPlans) {
			plan = plannedPlans[needed-1]
		}
		added += max(1, c.importConcurrencyForPlan(plan))
		if float64(added) >= gap {
			return needed
		}
	}
	return max(1, c.MaxReady)
}

// A different set of departing accounts must earn its own observation window.
// Sorting prevents database row order from resetting a valid observation.
func refillForecastBasis(e AccountRefillEvaluation) string {
	rows := make([]string, 0, e.ForecastAccounts)
	for _, a := range e.Accounts {
		if a.ForecastLoss {
			rows = append(rows, fmt.Sprintf("%d:%d", a.ID, a.Concurrency))
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, ",")
}

func refillNeedsCapacityObservation(reason string) bool {
	return reason == "并发利用率超过目标" || reason == refillForecastReason
}

func (s *AccountRefillService) refillObservationReady(c AccountRefillConfig, revision int64, e AccountRefillEvaluation, now time.Time) bool {
	if e.Reason == refillForecastReason {
		return s.forecastObservation(c, revision, e, now).Ready
	}
	return refillCapacityHigh(c, e) && s.capacityObservation(c, revision, now).Ready
}
