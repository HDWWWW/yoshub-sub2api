package service

import (
	"context"
	"errors"
	"time"
)

// Older workers understand only Pending. The repository writes this guard while
// new workers own per-account reservations, so a draining old slot cannot launch
// another probe against one of those accounts.
const AccountRiskConcurrentGuard = "concurrent_probe_guard"

func legacyRiskProbeActive(state *AccountRiskState) bool {
	return state.Pending != nil && state.Pending.Kind != AccountRiskConcurrentGuard
}

func riskSerialProbeBusy(state *AccountRiskState) bool {
	if legacyRiskProbeActive(state) {
		return true
	}
	for _, pending := range state.ActiveProbes {
		if pending.Kind != accountRiskProbeAll {
			return true
		}
	}
	return false
}

func riskAccountProbeBusy(state *AccountRiskState, accountID int64) bool {
	return legacyRiskProbeActive(state) || state.ActiveProbes[accountID] != nil
}

func releaseRiskProbe(state *AccountRiskState, pending AccountRiskPending) bool {
	if active := state.ActiveProbes[pending.AccountID]; active != nil && active.ID == pending.ID {
		delete(state.ActiveProbes, pending.AccountID)
		return true
	}
	if legacyRiskProbeActive(state) && state.Pending.ID == pending.ID {
		state.Pending = nil
		return true
	}
	return false
}

func (s *AccountRiskService) validateProbeReservation(ctx context.Context, job *accountRiskJob) error {
	state, err := s.store.Read(ctx)
	if err != nil {
		return err
	}
	active := state.ActiveProbes[job.account.ID]
	if active == nil && legacyRiskProbeActive(state) {
		active = state.Pending
	}
	if active == nil || active.ID != job.pending.ID || !active.ExpiresAt.After(time.Now()) {
		return errors.New("probe reservation changed")
	}
	r := state.Rules[job.pending.GroupID]
	if r == nil || !r.Config.Enabled || r.Revision != job.pending.Revision ||
		(isScheduledRiskProbe(job.pending.Kind) && !r.Config.ProbeEnabled) {
		return errors.New("configuration changed")
	}
	return nil
}

func expireRiskProbes(state *AccountRiskState, now time.Time) {
	expire := func(p *AccountRiskPending) {
		r := state.Rules[p.GroupID]
		if r == nil || r.Revision != p.Revision || p.Kind == "manual_probe" {
			return
		}
		o := &r.Observation
		o.LastProbeResult = "检测中断，未据此停调度"
		if pause := state.CapacityPauses[p.AccountID]; p.Kind == accountRiskProbeCapacity && pause != nil {
			o.LastProbeResult = "检测中断，保持暂停，等待下次复测"
			pause.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
		}
		o.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
		if check := o.CapacityChecks[p.AccountID]; p.Kind == accountRiskProbeCapacity && check != nil {
			check.NextProbeAt = o.NextProbeAt
		}
		if check := o.ScheduledChecks[p.AccountID]; p.Kind == accountRiskProbeAll && check != nil &&
			(p.Signature == "" || check.Signature == p.Signature) {
			check.NextProbeAt = *riskAfter(now, r.Config.ProbeIntervalSeconds)
			check.LastProbeAt, check.LastProbeResult = &now, o.LastProbeResult
		}
	}
	if legacyRiskProbeActive(state) && !state.Pending.ExpiresAt.After(now) {
		expire(state.Pending)
		state.Pending = nil
	}
	for id, pending := range state.ActiveProbes {
		if !pending.ExpiresAt.After(now) {
			expire(pending)
			delete(state.ActiveProbes, id)
		}
	}
}
