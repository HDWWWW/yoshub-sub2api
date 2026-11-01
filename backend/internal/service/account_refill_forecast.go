package service

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const refillForecastReason = "预计账号退出后并发达到目标"

// The current demand is retained in full. Only future capacity is reduced;
// pending deliveries contribute their saved capacity, never live capacity.
func refillForecastHigh(c AccountRefillConfig, e AccountRefillEvaluation) bool {
	return e.ForecastAccounts > 0 && e.InUse > 0 &&
		float64(e.InUse)*100 >= float64(e.ForecastCapacity+e.PendingCapacity)*float64(c.TargetConcurrency)
}

func refillCapacityGap(c AccountRefillConfig, inUse, capacity int) int {
	if inUse <= 0 || float64(inUse)*100 < float64(capacity)*float64(c.TargetConcurrency) {
		return 0
	}
	return refillMax(1, int(math.Ceil((float64(inUse)*100/float64(refillMax(1, c.TargetConcurrency))-float64(capacity))/float64(refillMax(1, c.ImportConcurrency)))))
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
