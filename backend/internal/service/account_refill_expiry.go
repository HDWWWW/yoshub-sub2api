package service

import (
	"fmt"
	"slices"
	"time"
)

// The supplier deadline distinguishes a renewed lease from an already covered
// one. Receipts survive job-history pruning and do not depend on live load.
type AccountRefillExpiryTarget struct {
	AccountID       int64 `json:"account_id"`
	SourceExpiresAt int64 `json:"source_expires_at"`
}

type accountRefillExpiryDemand struct {
	AccountRefillExpiryTarget
	JobID            string  `json:"job_id,omitempty"`
	OwnerGroupID     int64   `json:"owner_group_id,omitempty"`
	DeliveryGroupIDs []int64 `json:"delivery_group_ids,omitempty"`
	Completed        bool    `json:"completed,omitempty"`
}

func evaluateRefillExpiryDemands(state *accountRefillState, jobs []AccountRefillJob, claims []accountRefillExpiryDemand, now time.Time) {
	c, e := state.Config, &state.Evaluation
	e.ExpiryTargets = nil
	byID := make(map[int64]AccountRefillAccount, len(e.Accounts))
	for _, account := range e.Accounts {
		byID[account.ID] = account
	}
	demands := make(map[AccountRefillExpiryTarget]accountRefillExpiryDemand)
	for _, demand := range state.ExpiryDemands {
		account, exists := byID[demand.AccountID]
		if exists && account.SourceExpiresAt != nil && account.SourceExpiresAt.Unix() == demand.SourceExpiresAt {
			demands[demand.AccountRefillExpiryTarget] = demand
		}
	}
	if c.Mode != "interval" {
		for _, account := range e.Accounts {
			deadline := account.SourceExpiresAt
			if deadline == nil || deadline.After(now.Add(time.Duration(c.LeadMinutes)*time.Minute)) {
				continue
			}
			// Do not backfill old expired accounts when upgrading. A previously
			// observed pool may cross its deadline during provider backoff.
			future := deadline.After(now) && account.State != "unavailable"
			crossed := !state.ExpiryCheckedAt.IsZero() && deadline.After(state.ExpiryCheckedAt) && account.State == "expired"
			if !future && !crossed {
				continue
			}
			target := AccountRefillExpiryTarget{AccountID: account.ID, SourceExpiresAt: deadline.Unix()}
			if _, exists := demands[target]; !exists {
				demands[target] = accountRefillExpiryDemand{AccountRefillExpiryTarget: target}
			}
		}
	}
	state.ExpiryCheckedAt = now
	byJob := make(map[string]AccountRefillJob, len(jobs))
	for _, job := range jobs {
		byJob[job.ID] = job
	}
	// Another rule may already own or have fulfilled this exact lease. Failed
	// orders remain with their owner so overlapping groups cannot both retry.
	for _, claim := range claims {
		demand, exists := demands[claim.AccountRefillExpiryTarget]
		job, found := byJob[claim.JobID]
		if exists && !demand.Completed && (claim.Completed || found && !refillJobTimedOut(c, job, now) && (job.pending() || job.ReplacementPending || job.Status == "imported")) {
			demands[claim.AccountRefillExpiryTarget] = claim
		}
	}
	state.ExpiryDemands = make([]accountRefillExpiryDemand, 0, len(demands))
	for _, demand := range demands {
		state.ExpiryDemands = append(state.ExpiryDemands, demand)
	}
	slices.SortFunc(state.ExpiryDemands, func(a, b accountRefillExpiryDemand) int {
		if a.SourceExpiresAt != b.SourceExpiresAt {
			if a.SourceExpiresAt < b.SourceExpiresAt {
				return -1
			}
			return 1
		}
		if a.AccountID < b.AccountID {
			return -1
		}
		if a.AccountID > b.AccountID {
			return 1
		}
		return 0
	})
	usedJobs := make(map[string]bool)
	for _, demand := range state.ExpiryDemands {
		usedJobs[demand.JobID] = true
	}
	for _, claim := range claims {
		usedJobs[claim.JobID] = true
	}
	// Existing deliveries can cover a lease too, including orders created by
	// the previous version. One order can cover at most one lease in this pool.
	orderedJobs := slices.Clone(jobs)
	slices.SortFunc(orderedJobs, func(a, b AccountRefillJob) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for i := range state.ExpiryDemands {
		demand := &state.ExpiryDemands[i]
		job, found := byJob[demand.JobID]
		if found && job.Status == "imported" && job.AccountID > 0 {
			demand.Completed = true
		}
		if !demand.Completed && (demand.JobID == "" || !found || refillJobTimedOut(c, job, now) || !job.pending() && !job.ReplacementPending) {
			windowStart := time.Unix(demand.SourceExpiresAt, 0).Add(-time.Duration(c.LeadMinutes) * time.Minute)
			for _, candidate := range orderedJobs {
				committed := candidate.pending() || candidate.ReplacementPending
				if refillJobTimedOut(c, candidate, now) || usedJobs[candidate.ID] || candidate.AccountID == demand.AccountID ||
					(!committed && (candidate.CreatedAt.Before(windowStart) || candidate.Status != "imported" || candidate.AccountID <= 0)) ||
					(candidate.ExpiryTarget != nil && *candidate.ExpiryTarget != demand.AccountRefillExpiryTarget) {
					continue
				}
				demand.JobID, demand.OwnerGroupID = candidate.ID, candidate.GroupID
				demand.DeliveryGroupIDs = slices.Clone(candidate.DeliveryGroupIDs)
				demand.Completed = candidate.Status == "imported"
				usedJobs[candidate.ID] = true
				job, found = candidate, true
				break
			}
		}
		covered := demand.Completed || found && !refillJobTimedOut(c, job, now) && (job.pending() || job.ReplacementPending && demand.OwnerGroupID != c.GroupID)
		if !covered && c.Mode != "interval" {
			e.ExpiryTargets = append(e.ExpiryTargets, demand.AccountRefillExpiryTarget)
		}
	}
	// Receipts describe which lease an otherwise authorized purchase can cover.
	// They must not add demand or replace the reserve/concurrency reason: doing
	// so bypasses both the actual capacity gap and its observation window.
}

func claimRefillExpiryDemand(state *accountRefillState, job *AccountRefillJob, replacement int) {
	if len(state.Evaluation.ExpiryTargets) == 0 {
		return
	}
	target := state.Evaluation.ExpiryTargets[0]
	if replacement >= 0 {
		// Adopted legacy orders have no ExpiryTarget on the job itself. The
		// durable receipt also identifies their lease when earlier exits arrive.
		for _, demand := range state.ExpiryDemands {
			if demand.JobID == state.Jobs[replacement].ID && slices.Contains(state.Evaluation.ExpiryTargets, demand.AccountRefillExpiryTarget) {
				target = demand.AccountRefillExpiryTarget
				break
			}
		}
	}
	for i := range state.ExpiryDemands {
		demand := &state.ExpiryDemands[i]
		if demand.AccountRefillExpiryTarget != target {
			continue
		}
		job.ExpiryTarget = &target
		demand.JobID, demand.OwnerGroupID = job.ID, job.GroupID
		demand.DeliveryGroupIDs = slices.Clone(job.DeliveryGroupIDs)
		demand.Completed = false
		job.ReasonDetail += fmt.Sprintf(" 接替账号 #%d，号商回收时间 %s；该次租期仅补入一个接替账号。", target.AccountID, formatNotificationTime(time.Unix(target.SourceExpiresAt, 0)))
		return
	}
}

func completeRefillExpiryDemand(state *accountRefillState, job AccountRefillJob) {
	for i := range state.ExpiryDemands {
		if state.ExpiryDemands[i].JobID == job.ID {
			state.ExpiryDemands[i].Completed = true
		}
	}
}
