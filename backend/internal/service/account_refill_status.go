package service

import (
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

var errRefillDeliveryExpired = errors.New("号商交付已过租期")

// Refill/recharge states are distinct from refund states (cancelled, banned,
// settlement, etc.). Unknown states must never release a reservation or buy a
// replacement. See the supplier's /console/developer/docs#states contract.
func refillProviderStatusKnown(status string) bool {
	return slices.Contains([]string{"queued", "processing", "succeeded", "failed", "review"}, status)
}

func validateRefillProviderJob(remote *RefillProviderJob, job AccountRefillJob) error {
	if remote == nil || strings.TrimSpace(remote.ID) == "" || len(remote.ID) > 128 || len(remote.Status) > 128 ||
		(job.SupplierID != "" && remote.ID != job.SupplierID) ||
		(remote.Service != "" && remote.Service != "refill") ||
		(remote.Plan != "" && remote.Plan != job.Plan) ||
		(remote.Format != "" && remote.Format != "sub2api") ||
		(remote.Quantity != nil && *remote.Quantity != 1) ||
		math.IsNaN(remote.Price) || math.IsInf(remote.Price, 0) || remote.Price < 0 ||
		(remote.ExpiresAt != nil && (*remote.ExpiresAt < 0 || *remote.ExpiresAt > 253402214399)) {
		return errors.New("号商任务响应无效，保留原任务核对")
	}
	return nil
}

func refillNeedsAttention(status string) bool {
	return slices.Contains([]string{"blocked", "review", "import_failed", "supplier_unknown"}, status)
}

func refillDeliveryExpired(expires *int64, now time.Time) bool {
	return expires != nil && *expires <= now.Unix()
}

func finishRefillDeliveryExpired(job *AccountRefillJob, now time.Time) {
	job.Status = "delivery_expired"
	job.Error = "号商交付已过租期"
	job.ResultReady = false
	job.FinishedAt = &now
	// A successful but expired delivery is still charged by the supplier.
}
