package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrAccountRiskTestAccount = errors.New("请选择此分组内可测试的 OpenAI 账号")

type AccountRiskTestAccount struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Schedulable bool   `json:"schedulable"`
}

type AccountRiskManualResult struct {
	AccountID    int64  `json:"account_id"`
	Completed    bool   `json:"completed"`
	Matched      *bool  `json:"matched"`
	Text         string `json:"text"`
	Truncated    bool   `json:"truncated"`
	Message      string `json:"message"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

func (s *AccountRiskService) manualProbeAccount(ctx context.Context, groupID, accountID int64) (*Account, error) {
	g, err := s.groups.GetByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if g == nil || g.Platform != PlatformOpenAI || g.Status != StatusActive {
		return nil, ErrAccountRiskTestAccount
	}
	a, err := s.accounts.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	// Intentionally accepts schedulable=false and temporary cooldowns: this is
	// an explicit diagnostic after a stop, never a scheduler admission.
	if a == nil || a.Platform != PlatformOpenAI || !slices.Contains(a.GroupIDs, groupID) {
		return nil, ErrAccountRiskTestAccount
	}
	return a, nil
}

// TestNow uses the current form's probe parameters without saving its settings
// or changing scheduling. The shared reservation also excludes automatic tests.
func (s *AccountRiskService) TestNow(parent context.Context, groupID, accountID, revision int64, config AccountRiskConfig) (*AccountRiskManualResult, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	a, err := s.manualProbeAccount(ctx, groupID, accountID)
	if err != nil {
		return nil, err
	}
	id, now := uuid.NewString(), time.Now().UTC()
	err = s.store.Update(ctx, func(state *AccountRiskState, _ AccountRiskScheduling) error {
		r := state.Rules[groupID]
		if (r == nil && revision != 0) || (r != nil && r.Revision != revision) {
			return ErrAccountRiskConflict
		}
		if state.Pending != nil && state.Pending.ExpiresAt.After(now) {
			return ErrAccountRiskBusy
		}
		state.Pending = &AccountRiskPending{ID: id, GroupID: groupID, AccountID: accountID, Revision: revision, ExpiresAt: now.Add(120 * time.Second), Kind: "manual_probe"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer stop()
		for attempt := 0; attempt < 5; attempt++ {
			err := s.store.Update(cleanup, func(state *AccountRiskState, _ AccountRiskScheduling) error {
				if state.Pending != nil && state.Pending.ID == id {
					state.Pending = nil
				}
				return nil
			})
			if !errors.Is(err, ErrAccountRiskBusy) {
				break
			}
			select {
			case <-cleanup.Done():
				return
			case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
			}
		}
	}()
	out := &AccountRiskManualResult{AccountID: accountID, Message: "检测未完成（网络、上游错误或账号繁忙），未更改调度"}
	slot, err := s.loads.AcquireAccountSlot(ctx, accountID, a.Concurrency)
	if err != nil || slot == nil || !slot.Acquired {
		return out, nil
	}
	defer slot.ReleaseFunc()
	result, err := s.probe.RunRiskProbe(ctx, a, config)
	out.InputTokens, out.OutputTokens = result.InputTokens, result.OutputTokens
	if err != nil || !result.Completed {
		return out, nil
	}
	current, err := s.manualProbeAccount(ctx, groupID, accountID)
	if err != nil || riskModelAccountIdentity(current) != riskModelAccountIdentity(a) {
		out.Message = "账号或分组已变化，本次结果作废"
		return out, nil
	}
	out.Completed = true
	matched := strings.Contains(result.Text, config.ExpectedText)
	out.Matched = &matched
	text := []rune(result.Text)
	if len(text) > 4096 {
		text = text[:4096]
		out.Truncated = true
	}
	out.Text = string(text)
	out.Message = "完整回复不含期望内容，未更改调度"
	if matched {
		out.Message = "回复包含期望内容，检测通过，未更改调度"
	}
	return out, nil
}
