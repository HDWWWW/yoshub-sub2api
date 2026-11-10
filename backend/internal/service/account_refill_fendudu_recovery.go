package service

import (
	"context"
	"errors"
	"time"
)

var errFenduduPushPending = errors.New("粉嘟嘟原订单已确认，等待推送")

// Find exactly one order in the submission window, including across pages.
// Missing delivery metadata still counts as a possible competing order. Never
// choose the first delivered match while a second candidate is still pending.
func (s *AccountRefillService) recoverFenduduOrder(ctx context.Context, token string, state *accountRefillState, j AccountRefillJob) (*fenduduCycle, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	reader, ok := s.fenduduClient().(fenduduCycleReader)
	if !ok || j.SubmissionStartedAt == nil {
		return nil, errors.New("粉嘟嘟下单结果不确定，请核对并关联原订单 ID；不会重复下单")
	}
	cursor := ""
	seenCursors, seenIDs := map[string]bool{}, map[string]bool{}
	var candidates []fenduduCycle
	var previous time.Time
	complete := false
	for page := 0; page < 5; page++ {
		if seenCursors[cursor] {
			return nil, errors.New("粉嘟嘟订单分页重复，暂不自动关联")
		}
		seenCursors[cursor] = true
		rows, err := reader.Cycles(ctx, token, cursor)
		if err != nil {
			return nil, err
		}
		if rows == nil || !rows.Success || rows.Items == nil || len(rows.Items) > 200 || (rows.HasMore && (rows.NextCursor == "" || len(rows.Items) == 0)) {
			return nil, errors.New("粉嘟嘟订单列表不完整，暂不自动关联")
		}
		pastWindow := false
		for _, c := range rows.Items {
			created, err := time.Parse(time.RFC3339Nano, c.CreatedAt)
			if err != nil || !validFenduduIdentifier(c.ID, false) || seenIDs[c.ID] || (!previous.IsZero() && created.After(previous)) {
				return nil, errors.New("粉嘟嘟订单列表顺序或内容异常，暂不自动关联")
			}
			seenIDs[c.ID], previous = true, created
			if created.Before(j.SubmissionStartedAt.Add(-30 * time.Second)) {
				pastWindow = true
			}
			if validateFenduduCycle(&c, j) == nil {
				candidates = append(candidates, c)
			}
		}
		if !rows.HasMore || pastWindow {
			complete = true
			break
		}
		cursor = rows.NextCursor
	}
	if !complete {
		return nil, errors.New("粉嘟嘟订单较多，尚未完整核对下单时段，请手动关联原订单 ID")
	}
	if len(candidates) != 1 {
		return nil, errors.New("粉嘟嘟尚未找到唯一匹配订单，继续只读核对；有多笔时请手动关联订单 ID")
	}
	cycle, err := s.fenduduClient().Cycle(ctx, token, candidates[0].ID)
	if err != nil {
		return nil, err
	}
	if err := validateFenduduCycle(cycle, j); err != nil {
		return nil, err
	}
	if cycle.ID != candidates[0].ID {
		return nil, errors.New("粉嘟嘟候选订单编号不匹配")
	}
	if err := s.fenduduRecoveryUnambiguous(ctx, state, j, cycle); err != nil {
		return nil, err
	}
	if !cycle.Pushed {
		return cycle, errFenduduPushPending
	}
	if _, err := s.fenduduPushedAccount(ctx, &j, cycle); err != nil {
		return nil, err
	}
	return cycle, nil
}

func (s *AccountRefillService) fenduduRecoveryUnambiguous(ctx context.Context, state *accountRefillState, job AccountRefillJob, cycle *fenduduCycle) error {
	check := func(other accountRefillState) error {
		for _, j := range other.Jobs {
			if j.ID == job.ID || j.Provider != refillProviderFendudu {
				continue
			}
			if j.SupplierID == cycle.ID {
				return errors.New("粉嘟嘟订单已关联其他任务")
			}
			if j.SupplierID == "" && j.pending() && other.Fendudu.TokenHash == state.Fendudu.TokenHash && validateFenduduCycle(cycle, j) == nil {
				return errors.New("同一时段存在其他待核对补号任务，请手动关联原订单 ID")
			}
		}
		return nil
	}
	if err := check(*state); err != nil {
		return err
	}
	if stores, ok := s.rootService().store.(AccountRefillGroupStore); ok {
		rows, err := stores.ListRules(ctx)
		if err != nil {
			return errors.New("无法核对其他分组的订单，暂不自动关联")
		}
		for id, raw := range rows {
			if id == s.ruleGroupID {
				continue
			}
			other, err := decodeAccountRefillState(raw)
			if err != nil {
				return err
			}
			if err := check(other); err != nil {
				return err
			}
		}
	}
	return nil
}
