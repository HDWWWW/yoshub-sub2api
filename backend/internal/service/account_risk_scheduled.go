package service

import (
	"fmt"
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
		o.Message = fmt.Sprintf("全部账号定时检测：%d 个可调度账号独立计时，依次检测", len(checks))
	}
	return due
}

func reserveAllAccountRiskProbe(state *AccountRiskState, candidates []accountRiskScheduledCandidate, now time.Time) *accountRiskJob {
	// Preserve capacity verification and the global one-probe reservation.
	if state.Pending != nil {
		return nil
	}
	var selected *accountRiskScheduledCandidate
	for i := range candidates {
		candidate := &candidates[i]
		if state.CapacityPauses[candidate.account.ID] != nil {
			continue
		}
		// Earliest due first across all groups. A slow or repeatedly failing
		// account cannot monopolize the queue when others are already due.
		if selected == nil || candidate.check.NextProbeAt.Before(selected.check.NextProbeAt) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil
	}
	r, check := selected.rule, selected.check
	job := reserveAccountRiskProbe(state, r, selected.account, check.Signature, accountRiskProbeAll, "", now)
	check.NextProbeAt = *r.Observation.NextProbeAt
	r.Observation.Message = fmt.Sprintf("正在定时检测账号 #%d；其他账号按各自间隔继续检测", selected.account.ID)
	return job
}
