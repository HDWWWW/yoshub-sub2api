package service

import (
	"fmt"
	"sort"
	"time"
)

type AccountRiskScheduledCheck struct {
	AccountID       int64      `json:"account_id"`
	AccountName     string     `json:"account_name"`
	Signature       string     `json:"signature"`
	NextProbeAt     time.Time  `json:"next_probe_at"`
	LastProbeAt     *time.Time `json:"last_probe_at,omitempty"`
	LastProbeResult string     `json:"last_probe_result,omitempty"`
}

type accountRiskScheduledCandidate struct {
	rule    *AccountRiskRule
	account Account
	check   *AccountRiskScheduledCheck
}

func isScheduledRiskProbe(kind string) bool {
	return kind == accountRiskProbeSingle || kind == accountRiskProbeAll
}

func riskScheduledAccountSignature(a *Account) string {
	copy := *a
	// Removing a peer can change expiry-based ranks. That must not reset this
	// account's timer or invalidate its own in-flight result.
	copy.schedulingPriority = 0
	return riskAccountSignature(&copy)
}

func refreshAllAccountRiskProbes(r *AccountRiskRule, pool []Account, now time.Time) []accountRiskScheduledCandidate {
	if !r.Config.ProbeEnabled || r.Config.ProbeScope != "all" {
		return nil
	}
	o := &r.Observation
	checks := make(map[int64]*AccountRiskScheduledCheck, len(pool))
	var due []accountRiskScheduledCandidate
	var next *time.Time
	for _, a := range pool {
		// Capacity sampling may have paused this account earlier in this tick.
		if !a.Schedulable {
			continue
		}
		sig := riskScheduledAccountSignature(&a)
		check := o.ScheduledChecks[a.ID]
		if check == nil || check.Signature != sig {
			check = &AccountRiskScheduledCheck{AccountID: a.ID, Signature: sig, NextProbeAt: *riskAfter(now, r.Config.ProbeIntervalSeconds)}
		}
		check.AccountName = a.Name
		checks[a.ID] = check
		if next == nil || check.NextProbeAt.Before(*next) {
			next = &check.NextProbeAt
		}
		if !check.NextProbeAt.After(now) {
			due = append(due, accountRiskScheduledCandidate{rule: r, account: a, check: check})
		}
	}
	o.ScheduledChecks = checks
	if o.Mode == "scheduled" || o.NextProbeAt == nil || (next != nil && next.Before(*o.NextProbeAt)) {
		o.NextProbeAt = next
	}
	if o.Mode == "scheduled" {
		o.First = nil
	}
	if o.Mode == "scheduled" || (o.Mode == "capacity" && o.AnomalySince == nil) {
		o.Message = fmt.Sprintf("全部账号定时检测：%d 个可调度账号独立计时，到期并发检测", len(checks))
	}
	return due
}

func reserveAllAccountRiskProbes(state *AccountRiskState, candidates []accountRiskScheduledCandidate, now time.Time) []*accountRiskJob {
	// Wait for a genuine old-version reservation to finish before installing
	// the compatibility guard. New reservations coordinate per account.
	if legacyRiskProbeActive(state) {
		return nil
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].check.NextProbeAt.Before(candidates[j].check.NextProbeAt)
	})
	var jobs []*accountRiskJob
	for _, candidate := range candidates {
		if state.CapacityPauses[candidate.account.ID] != nil || riskAccountProbeBusy(state, candidate.account.ID) {
			continue
		}
		r, check := candidate.rule, candidate.check
		job := reserveAccountRiskProbe(state, r, candidate.account, check.Signature, accountRiskProbeAll, "", now)
		check.NextProbeAt = *r.Observation.NextProbeAt
		r.Observation.Message = "到期账号正在并发检测，每个账号独立计时并处理结果"
		jobs = append(jobs, job)
	}
	return jobs
}
