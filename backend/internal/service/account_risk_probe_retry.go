package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

func riskProbeTimeout(c AccountRiskConfig, kind string) time.Duration {
	if isScheduledRiskProbe(kind) {
		return time.Duration(1+c.ProbeMismatchMaxRetries) * 90 * time.Second
	}
	return 90 * time.Second
}

// Only complete mismatched scheduled replies enter this retry sequence. Each
// retry gets a fresh 90-second budget and concurrency slot. The account lease
// spans the entire sequence so no other worker can test the same account.
func (s *AccountRiskService) runProbeAttempts(ctx context.Context, job *accountRiskJob) (AccountRiskProbeResult, error) {
	retries := 0
	if isScheduledRiskProbe(job.pending.Kind) {
		retries = job.config.ProbeMismatchMaxRetries
	}
	var result AccountRiskProbeResult
	var err error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			if err := s.validateProbeReservation(ctx, job); err != nil {
				return AccountRiskProbeResult{}, err
			}
			if _, err := s.probeAccount(ctx, job, time.Now()); err != nil {
				return AccountRiskProbeResult{}, err
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		result, err = s.runProbe429Attempts(attemptCtx, job, attempt == 0)
		cancel()
		if ctx.Err() != nil {
			return AccountRiskProbeResult{}, ctx.Err()
		}
		result.ConsecutiveMismatches = attempt
		if attempt > 0 && err != nil && result.UpstreamStatus == http.StatusTooManyRequests {
			result.MismatchRetry429 = true
			return result, err
		}
		if err != nil || !result.Completed || strings.Contains(result.Text, job.config.ExpectedText) {
			return result, err
		}
		result.ConsecutiveMismatches++
	}
	return result, err
}

// The initial probe keeps the existing consecutive-429 policy. Once a complete
// mismatch has triggered a retry, a real 429 ends that sequence immediately.
func (s *AccountRiskService) runProbe429Attempts(ctx context.Context, job *accountRiskJob, retry429 bool) (AccountRiskProbeResult, error) {
	if err := ctx.Err(); err != nil {
		return AccountRiskProbeResult{}, err
	}
	slot, err := s.loads.AcquireAccountSlot(ctx, job.account.ID, job.account.Concurrency)
	if err != nil {
		return AccountRiskProbeResult{}, err
	}
	if slot == nil || !slot.Acquired {
		return AccountRiskProbeResult{}, errors.New("account busy")
	}
	defer slot.ReleaseFunc()
	attempts := 1
	if retry429 && isScheduledRiskProbe(job.pending.Kind) && job.config.Probe429StopEnabled {
		attempts += job.config.Probe429MaxRetries
	}
	var result AccountRiskProbeResult
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
			return AccountRiskProbeResult{}, ctx.Err()
		}
		if err == nil || result.UpstreamStatus != http.StatusTooManyRequests {
			return result, err
		}
		result.Consecutive429 = attempt + 1
	}
	return result, err
}
