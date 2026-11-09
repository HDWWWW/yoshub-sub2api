package service

import (
	"context"
	"fmt"
	"time"
)

func (s *AccountRiskService) tickCapacity(ctx context.Context, state *AccountRiskState, r *AccountRiskRule, pool []Account, previousAt, now time.Time, scheduling AccountRiskScheduling) (*accountRiskJob, error) {
	o := &r.Observation
	requests := make([]AccountWithConcurrency, 0, len(pool))
	checks := make(map[int64]*AccountRiskCapacityCheck, len(pool))
	positions := make(map[int64]int, len(pool))
	for i := range pool {
		a := &pool[i]
		positions[a.ID] = i
		if a.Concurrency <= 0 {
			continue
		}
		sig := riskAccountSignature(a)
		check := o.CapacityChecks[a.ID]
		if check == nil || check.TargetSignature != sig {
			check = &AccountRiskCapacityCheck{TargetSignature: sig}
			// Preserve a previous-version pair's cooldown during an online upgrade.
			if o.First != nil && o.First.AccountID == a.ID && len(o.ProbeSignature) >= len(sig) && o.ProbeSignature[:len(sig)] == sig {
				check.NextProbeAt = o.NextProbeAt
			}
		}
		checks[a.ID] = check
		requests = append(requests, AccountWithConcurrency{ID: a.ID, MaxConcurrency: a.Concurrency})
	}
	o.CapacityChecks = checks // Drop removed accounts and bound persisted state to the eligible pool.
	o.First, o.Second = nil, nil
	if len(requests) < 2 {
		riskReset(o, "waiting", "至少需要两个设置有限并发容量的可调度账号")
		return nil, nil
	}
	loads, err := s.loads.GetAccountsLoadBatchFresh(ctx, requests)
	if err != nil {
		riskReset(o, "unavailable", "容量读取失败，本次不判定")
		return nil, nil
	}
	for i := range pool {
		a := &pool[i]
		check := checks[a.ID]
		if check == nil {
			continue
		}
		load := loads[a.ID]
		if load == nil || load.CurrentConcurrency < 0 {
			riskReset(o, "unavailable", "容量采样缺失，本次不判定")
			return nil, nil
		}
		check.Target = AccountRiskLoad{AccountID: a.ID, Name: a.Name, Priority: a.SchedulingPriority(), Used: load.CurrentConcurrency, Capacity: a.Concurrency,
			Percent: 100 * float64(load.CurrentConcurrency) / float64(a.Concurrency)}
	}

	// Walk priority tiers backwards in O(n). A full account only serves as
	// evidence for strictly earlier tiers, never for a peer of equal priority.
	var reference *AccountRiskCapacityCheck
	for end := len(pool); end > 0; {
		start := end - 1
		for start > 0 && pool[start-1].SchedulingPriority() == pool[end-1].SchedulingPriority() {
			start--
		}
		for i := start; i < end; i++ {
			check := checks[pool[i].ID]
			if check == nil {
				continue
			}
			if reference == nil || check.Target.Percent >= float64(r.Config.FirstBelowPercent) {
				check.AnomalySince, check.Reference, check.Signature = nil, nil, ""
				continue
			}
			witness := reference
			// Keep valid evidence stable if an intermediate account briefly
			// becomes busy; it must not repeatedly reset an ongoing anomaly.
			if check.Reference != nil {
				previous := checks[check.Reference.AccountID]
				if previous != nil && positions[previous.Target.AccountID] > i && previous.Target.Priority != check.Target.Priority && previous.Target.Percent >= float64(r.Config.SecondAtLeastPercent) {
					witness = previous
				}
			}
			sig := check.TargetSignature + witness.TargetSignature
			if check.AnomalySince == nil || check.Signature != sig || check.Worker != s.worker || now.Sub(previousAt) > 3*time.Second || now.Before(previousAt) {
				startAt := now
				check.AnomalySince = &startAt
			}
			check.Reference = &witness.Target
			check.Signature, check.Worker = sig, s.worker
		}
		// The nearest later high-load tier is the reference. Within a tier use
		// the stable account-ID order already supplied by candidates().
		for i := end - 1; i >= start; i-- {
			if check := checks[pool[i].ID]; check != nil && check.Target.Percent >= float64(r.Config.SecondAtLeastPercent) {
				reference = check
			}
		}
		end = start
	}

	o.Mode, o.Message = "capacity", "容量正常，未连续满足触发条件"
	o.AnomalySince, o.NextProbeAt, o.Signature, o.ProbeSignature = nil, nil, "", ""
	var selected, due *AccountRiskCapacityCheck
	var dueAccount *Account
	count := 0
	for i := range pool {
		check := checks[pool[i].ID]
		if check == nil || check.AnomalySince == nil {
			continue
		}
		count++
		if selected == nil {
			selected = check
		}
		if now.Sub(*check.AnomalySince) < time.Duration(r.Config.SustainSeconds)*time.Second ||
			(check.NextProbeAt != nil && check.NextProbeAt.After(now)) {
			continue
		}
		if !r.Config.CapacityNotifyOnly {
			if err := pauseCapacityAccount(state, r, &pool[i], check, scheduling, now); err != nil {
				return nil, err
			}
			continue
		}
		// Never-tested accounts first, then the oldest completed probe. Priority
		// order breaks ties; a healthy first account cannot monopolize retests.
		if due == nil || (check.LastProbeAt == nil && due.LastProbeAt != nil) ||
			(check.LastProbeAt != nil && due.LastProbeAt != nil && check.LastProbeAt.Before(*due.LastProbeAt)) {
			due, dueAccount = check, &pool[i]
		}
	}
	if selected == nil {
		// Keep the familiar two-account load summary when there is no anomaly.
		if first := checks[pool[0].ID]; first != nil {
			o.First = &first.Target
		}
		if second := checks[pool[1].ID]; second != nil {
			o.Second = &second.Target
		}
		return nil, nil
	}
	if due != nil {
		selected = due
	}
	o.First, o.Second = &selected.Target, selected.Reference
	o.AnomalySince, o.NextProbeAt = selected.AnomalySince, selected.NextProbeAt
	o.Signature, o.ProbeSignature, o.Worker = selected.Signature, selected.Signature, s.worker
	o.Message = fmt.Sprintf("%d 个账号容量异常，独立计时并依次复测", count)
	if due == nil {
		return nil, nil
	}
	if state.Pending != nil {
		o.Message = "容量异常已确认，等待当前消息复测完成"
		return nil, nil
	}
	reason := fmt.Sprintf("容量异常持续 %d 秒：账号 #%d %.1f%% < %d%%，后位账号 #%d %.1f%% ≥ %d%%", r.Config.SustainSeconds,
		due.Target.AccountID, due.Target.Percent, r.Config.FirstBelowPercent, due.Reference.AccountID, due.Reference.Percent, r.Config.SecondAtLeastPercent)
	job := reserveAccountRiskProbe(state, r, *dueAccount, due.Signature, accountRiskProbeCapacity, reason, now)
	job.referenceID = due.Reference.AccountID
	due.NextProbeAt = o.NextProbeAt
	o.Message = fmt.Sprintf("正在复测账号 #%d，后位高负载账号 #%d", due.Target.AccountID, due.Reference.AccountID)
	return job, nil
}
