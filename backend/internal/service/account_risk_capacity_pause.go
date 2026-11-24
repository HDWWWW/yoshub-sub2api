package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// This token is owned by one capacity incident. Explicit scheduling changes
// revoke it, so a delayed passing probe cannot undo a manual or independent stop.
const AccountRiskCapacityPauseExtraKey = "yoshub_risk_capacity_pause"

type AccountRiskCapacityPause struct {
	ReferenceID     int64      `json:"reference_id"`
	Sequence        int        `json:"sequence"`
	Token           string     `json:"token"`
	GroupID         int64      `json:"group_id"`
	AccountID       int64      `json:"account_id"`
	Revision        int64      `json:"revision"`
	Signature       string     `json:"signature"`
	TargetSignature string     `json:"target_signature"`
	Reason          string     `json:"reason"`
	At              time.Time  `json:"at"`
	NextProbeAt     *time.Time `json:"next_probe_at,omitempty"`
}

func riskCapacityIdentity(a *Account) string {
	copy := *a
	copy.schedulingPriority = 0 // Expiry ranks change when a paused peer leaves the pool.
	return fmt.Sprintf("%x:%s", riskModelAccountIdentity(a), riskAccountSignature(&copy))
}

func pauseCapacityAccount(state *AccountRiskState, r *AccountRiskRule, a *Account, check *AccountRiskCapacityCheck, scheduling AccountRiskScheduling, now time.Time) error {
	if state.CapacityPauses[a.ID] != nil {
		return nil
	}
	token := uuid.NewString()
	changed, err := scheduling.Pause(r.GroupID, a, token)
	if err != nil || !changed {
		return err
	}
	reason := fmt.Sprintf("容量异常持续 %d 秒：账号 #%d %.1f%% < %d%%，后位账号 #%d %.1f%% ≥ %d%%", r.Config.SustainSeconds,
		a.ID, check.Target.Percent, r.Config.FirstBelowPercent, check.Reference.AccountID, check.Reference.Percent, r.Config.SecondAtLeastPercent)
	if state.CapacityPauses == nil {
		state.CapacityPauses = make(map[int64]*AccountRiskCapacityPause)
	}
	state.CapacityPauses[a.ID] = &AccountRiskCapacityPause{ReferenceID: check.Reference.AccountID, Sequence: len(state.CapacityPauses), Token: token, GroupID: r.GroupID, AccountID: a.ID, Revision: r.Revision,
		Signature: riskCapacityIdentity(a), TargetSignature: check.TargetSignature, Reason: reason, At: now}
	// Keep a copy visible in this transaction, without changing cached snapshots.
	a.Schedulable = false
	a.Extra = maps.Clone(a.Extra)
	if a.Extra == nil {
		a.Extra = make(map[string]any)
	}
	a.Extra[AccountRiskCapacityPauseExtraKey] = token
	riskCapacityEvent(r, a, now, reason+"；已暂停调度，等待消息复测，通过后自动恢复", "pause")
	return nil
}

func riskCapacityEvent(r *AccountRiskRule, a *Account, now time.Time, reason, action string) {
	event := AccountRiskEvent{ID: uuid.NewString(), At: now, AccountID: a.ID, AccountName: riskNotificationName(a.Name),
		Kind: accountRiskProbeCapacity, Action: action, Reason: reason}
	if action == "pause" && r.Config.NotifyEnabled {
		event.Notification = "pending"
	}
	r.Events = append([]AccountRiskEvent{event}, r.Events...)
	trimAccountRiskEvents(r)
}

func (s *AccountRiskService) pausedProbeAccount(ctx context.Context, groupID, accountID int64, token, signature string, now time.Time) (*Account, error) {
	a, err := s.manualProbeAccount(ctx, groupID, accountID)
	if err != nil {
		return nil, err
	}
	if a.Schedulable || a.Extra[AccountRiskCapacityPauseExtraKey] != token || riskCapacityIdentity(a) != signature {
		return nil, errors.New("capacity pause ownership changed")
	}
	if err := s.capacityProbeAllowed(ctx, a, now); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *AccountRiskService) capacityProbeAllowed(ctx context.Context, a *Account, now time.Time) error {
	deadline, err := s.accountProbeDeadline(ctx, a)
	if err != nil {
		return err
	}
	if supplierReclaimed(deadline, now) {
		return errRiskSupplierReclaimed
	}
	check := *a
	check.Schedulable = true
	if !check.isSchedulableAt(now) || EvaluateAccountSchedulingThreshold(&check, nil, now).ShouldPause {
		return errors.New("account has another scheduling restriction")
	}
	return nil
}

// Paused accounts are excluded from regular sampling but remain in this durable
// queue. A restart, timeout, or the loss of the original witness cannot bypass
// verification or leave another confirmed anomaly waiting for the first probe.
func (s *AccountRiskService) tickCapacityPauses(ctx context.Context, state *AccountRiskState, newlyPaused map[int64]Account, now time.Time) *accountRiskJob {
	pauses := make([]*AccountRiskCapacityPause, 0, len(state.CapacityPauses))
	observed := make(map[int64]bool)
	for id, p := range state.CapacityPauses {
		r := state.Rules[p.GroupID]
		if r == nil || !r.Config.Enabled || r.Config.CapacityNotifyOnly || r.Revision != p.Revision {
			delete(state.CapacityPauses, id) // Configuration changes never turn accounts back on.
			continue
		}
		r.Observation.Mode, r.Observation.Message = "paused", "容量异常账号已暂停调度，等待消息复测通过后恢复"
		next := p.NextProbeAt
		if next == nil {
			next = &now
		}
		if !observed[p.GroupID] || r.Observation.NextProbeAt == nil || next.Before(*r.Observation.NextProbeAt) {
			r.Observation.NextProbeAt = next
		}
		observed[p.GroupID] = true
		if p.NextProbeAt == nil || !p.NextProbeAt.After(now) {
			pauses = append(pauses, p)
		}
	}
	if riskSerialProbeBusy(state) {
		return nil
	}
	sort.Slice(pauses, func(i, j int) bool {
		a, b := pauses[i], pauses[j]
		if (a.NextProbeAt == nil) != (b.NextProbeAt == nil) {
			return a.NextProbeAt == nil
		}
		if a.NextProbeAt != nil && !a.NextProbeAt.Equal(*b.NextProbeAt) {
			return a.NextProbeAt.Before(*b.NextProbeAt)
		}
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		if a.Sequence != b.Sequence {
			return a.Sequence < b.Sequence
		}
		return a.AccountID < b.AccountID
	})
	for _, p := range pauses {
		if riskAccountProbeBusy(state, p.AccountID) {
			continue
		}
		r := state.Rules[p.GroupID]
		a, err := s.manualProbeAccount(ctx, p.GroupID, p.AccountID)
		if fresh, ok := newlyPaused[p.AccountID]; ok {
			a, err = &fresh, nil
		}
		if err == nil && (a.Schedulable || a.Extra[AccountRiskCapacityPauseExtraKey] != p.Token) {
			delete(state.CapacityPauses, p.AccountID)
			continue
		}
		if err == nil {
			err = s.capacityProbeAllowed(ctx, a, now)
		}
		if err != nil {
			p.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
			r.Observation.NextProbeAt = p.NextProbeAt
			continue
		}
		// A new attempt uses current credentials; an in-flight old result still
		// has to match its original identity before restoration.
		p.Signature = riskCapacityIdentity(a)
		job := reserveAccountRiskProbe(state, r, *a, p.Signature, accountRiskProbeCapacity, p.Reason, now)
		job.pauseToken, job.referenceID = p.Token, p.ReferenceID
		p.NextProbeAt = r.Observation.NextProbeAt
		r.Observation.Message = fmt.Sprintf("账号 #%d 已暂停调度，正在消息复测，通过后自动恢复", a.ID)
		return job
	}
	return nil
}

func (s *AccountRiskService) finishCapacityPause(ctx context.Context, state *AccountRiskState, scheduling AccountRiskScheduling, job *accountRiskJob, result AccountRiskProbeResult, probeErr error, now time.Time) error {
	p := state.CapacityPauses[job.account.ID]
	r := state.Rules[job.pending.GroupID]
	if p == nil || p.Token != job.pauseToken || r == nil || !r.Config.Enabled || r.Config.CapacityNotifyOnly || r.Revision != job.pending.Revision {
		return nil
	}
	o := &r.Observation
	o.LastProbeAt, o.InputTokens, o.OutputTokens = &now, result.InputTokens, result.OutputTokens
	p.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
	o.NextProbeAt = p.NextProbeAt
	if probeErr != nil || !result.Completed {
		o.LastProbeResult = "检测未完成（网络、上游错误或账号繁忙），保持暂停，等待下次复测"
		return nil
	}
	a, err := s.pausedProbeAccount(ctx, p.GroupID, p.AccountID, p.Token, p.Signature, now)
	if err != nil {
		o.LastProbeResult = "账号或分组已变化，本次结果作废，未恢复调度"
		return nil
	}
	if !strings.Contains(result.Text, r.Config.ExpectedText) {
		o.LastProbeResult = "消息复测完整回复不含期望内容，保持暂停，等待下次复测"
		return nil
	}
	changed, err := scheduling.Resume(p.GroupID, a, p.Token)
	if err != nil {
		return err
	}
	if !changed {
		o.LastProbeResult = "账号已变化，本次结果作废，未恢复调度"
		return nil
	}
	o.LastProbeResult = "回复包含期望内容，检测通过，已恢复账号调度"
	o.Mode, o.Message = "waiting", o.LastProbeResult
	if o.CapacityChecks == nil {
		o.CapacityChecks = make(map[int64]*AccountRiskCapacityCheck)
	}
	o.CapacityChecks[a.ID] = &AccountRiskCapacityCheck{TargetSignature: p.TargetSignature, LastProbeAt: &now, NextProbeAt: p.NextProbeAt}
	// Cancel an unsent pause notice after recovery; never send a stale stop alert.
	for i := range r.Events {
		e := &r.Events[i]
		if e.AccountID == a.ID && e.Kind == accountRiskProbeCapacity && e.Action == "pause" && e.Notification == "pending" {
			e.Notification = "cancelled"
		}
	}
	riskCapacityEvent(r, a, now, o.LastProbeResult, "resume")
	delete(state.CapacityPauses, a.ID)
	return nil
}

// Kept separate from IsSchedulable: diagnostics can inspect a paused account,
// while only its capacity incident may grant automatic restoration.
func AccountHasRiskCapacityPause(a *Account) bool {
	if a == nil {
		return false
	}
	token, _ := a.Extra[AccountRiskCapacityPauseExtraKey].(string)
	return !a.Schedulable && token != "" && a.Status == StatusActive
}
