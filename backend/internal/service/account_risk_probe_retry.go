package service

import (
	"context"
	"net/http"
	"time"
)

// Only scheduled probes opt into retrying explicit upstream
// rejections. Capacity verification and manual diagnostics keep their semantics.
// One slot and the existing 90-second deadline bound the whole detection cycle.
func (s *AccountRiskService) runProbeAttempts(ctx context.Context, job *accountRiskJob) (AccountRiskProbeResult, error) {
	attempts := 1
	if isScheduledRiskProbe(job.pending.Kind) && job.config.Probe429StopEnabled {
		attempts += job.config.Probe429MaxRetries
	}
	var result AccountRiskProbeResult
	var err error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			// The operator explicitly chooses immediate retries for this probe.
			// Do not add a backoff or carry the count into another scheduled run.
			// A retry must still belong to the same enabled rule and account.
			if readErr := s.validateProbeReservation(ctx, job); readErr != nil {
				return result, readErr
			}
			if _, readErr := s.probeAccount(ctx, job, time.Now()); readErr != nil {
				return result, readErr
			}
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		result, err = s.probe.RunRiskProbe(ctx, &job.account, job.config)
		if ctx.Err() != nil {
			result.Consecutive429 = 0
			return result, ctx.Err()
		}
		if err == nil || result.UpstreamStatus != http.StatusTooManyRequests {
			return result, err
		}
		result.Consecutive429 = attempt + 1
	}
	return result, err
}
