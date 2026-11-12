package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"sync"
	"time"
)

const AccountAutoFastSettingKey = "account_auto_fast_v1"
const accountAutoFastInterval = 10 * time.Second

var ErrAccountAutoFastBusy = errors.New("自动Fast正在更新，请稍后重试")
var ErrAccountAutoFastConflict = errors.New("配置已被修改，请刷新后重试")

type AccountAutoFastConfigError struct{ Message string }

func (e *AccountAutoFastConfigError) Error() string { return e.Message }

func invalidAutoFastConfig(message string) error {
	return &AccountAutoFastConfigError{Message: message}
}

type AccountAutoFastStage struct {
	Minutes          int     `json:"minutes"`
	UsedBelowPercent float64 `json:"used_below_percent"`
}

type AccountAutoFastConfig struct {
	Enabled                 bool                   `json:"enabled"`
	RequireMultipleAccounts bool                   `json:"require_multiple_accounts"`
	Stages                  []AccountAutoFastStage `json:"stages"`
	FinalMinutes            int                    `json:"final_minutes"`
	UsageMaxAgeSeconds      int                    `json:"usage_max_age_seconds"`
}

func DefaultAccountAutoFastConfig() AccountAutoFastConfig {
	return AccountAutoFastConfig{Stages: []AccountAutoFastStage{{45, 50}, {30, 65}, {15, 80}}, FinalMinutes: 5, UsageMaxAgeSeconds: 300}
}

func (c AccountAutoFastConfig) Validate() error {
	if len(c.Stages) < 1 || len(c.Stages) > 12 {
		return errors.New("请配置1–12个时间阶段")
	}
	previous := 10081
	for _, stage := range c.Stages {
		if stage.Minutes < 1 || stage.Minutes >= previous {
			return errors.New("阶段分钟数须在1–10080之间，按从大到小排列且不能重复")
		}
		if math.IsNaN(stage.UsedBelowPercent) || math.IsInf(stage.UsedBelowPercent, 0) || stage.UsedBelowPercent < 0 || stage.UsedBelowPercent > 100 {
			return errors.New("7D已用比例阈值须在0–100%之间")
		}
		previous = stage.Minutes
	}
	if c.FinalMinutes < 1 || c.FinalMinutes >= previous {
		return errors.New("最终强制阶段须至少1分钟，且小于最后一个普通阶段")
	}
	if c.UsageMaxAgeSeconds < 30 || c.UsageMaxAgeSeconds > 86400 {
		return errors.New("额度数据有效期须在30–86400秒之间")
	}
	return nil
}

type AccountAutoFastObservation struct {
	AccountID int64      `json:"account_id"`
	Name      string     `json:"name"`
	Deadline  *time.Time `json:"deadline,omitempty"`
	Source    string     `json:"source"`
	Used      *float64   `json:"used_percent,omitempty"`
	Stage     int        `json:"stage_minutes,omitempty"`
	Mode      string     `json:"mode"`
	Message   string     `json:"message"`
	Fast      bool       `json:"fast"`
	Managed   bool       `json:"managed"`
}

type AccountAutoFastEvent struct {
	At        time.Time `json:"at"`
	AccountID int64     `json:"account_id"`
	Enabled   bool      `json:"enabled"`
	Reason    string    `json:"reason"`
}

type AccountAutoFastRule struct {
	GroupID          int64                        `json:"group_id"`
	Revision         int64                        `json:"revision"`
	Config           AccountAutoFastConfig        `json:"config"`
	CheckedAt        time.Time                    `json:"checked_at"`
	Total            int                          `json:"total"`
	EligibleAccounts int                          `json:"eligible_accounts"`
	Accounts         []AccountAutoFastObservation `json:"accounts"`
	Events           []AccountAutoFastEvent       `json:"events"`
}

type AccountAutoFastManaged struct {
	GroupID  int64 `json:"group_id"`
	Original bool  `json:"original"`
	Applied  bool  `json:"applied"`
}

type AccountAutoFastState struct {
	Rules     map[int64]*AccountAutoFastRule   `json:"rules"`
	Managed   map[int64]AccountAutoFastManaged `json:"managed"`
	CheckedAt time.Time                        `json:"checked_at"`
}

// groupID=0 is restoration after leaving a rule; all writes compare a fresh
// account version and atomically commit the flag, ownership and scheduler event.
type AccountAutoFastApply func(groupID int64, account *Account, enabled bool) (bool, error)
type AccountAutoFastStore interface {
	Read(context.Context) (*AccountAutoFastState, error)
	Update(context.Context, func(*AccountAutoFastState, AccountAutoFastApply) error) error
}
type autoFastAccounts interface {
	ListByGroup(context.Context, int64) ([]Account, error)
	GetByID(context.Context, int64) (*Account, error)
}
type autoFastGroups interface {
	GetByID(context.Context, int64) (*Group, error)
}

type AccountAutoFastService struct {
	store               AccountAutoFastStore
	accounts            autoFastAccounts
	groups              autoFastGroups
	cancel              context.CancelFunc
	wg                  sync.WaitGroup
	startOnce, stopOnce sync.Once
}

func NewAccountAutoFastService(store AccountAutoFastStore, accounts AccountRepository, groups GroupRepository) *AccountAutoFastService {
	return &AccountAutoFastService{store: store, accounts: accounts, groups: groups}
}

func ProvideAccountAutoFastService(store AccountAutoFastStore, accounts AccountRepository, groups GroupRepository) *AccountAutoFastService {
	s := NewAccountAutoFastService(store, accounts, groups)
	s.Start()
	return s
}

func (s *AccountAutoFastService) Start() {
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(accountAutoFastInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runCtx, stop := context.WithTimeout(ctx, 8*time.Second)
					err := s.tick(runCtx, time.Now().UTC())
					stop()
					if err != nil && !errors.Is(err, ErrAccountAutoFastBusy) && ctx.Err() == nil {
						slog.Warn("account auto fast evaluation failed")
					}
				}
			}
		}()
	})
}

func (s *AccountAutoFastService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	s.wg.Wait()
}

func (s *AccountAutoFastService) View(ctx context.Context, groupID int64) (*AccountAutoFastRule, error) {
	g, err := s.groups.GetByID(ctx, groupID)
	if errors.Is(err, ErrGroupNotFound) || (err == nil && (g == nil || g.Platform != PlatformOpenAI)) {
		return nil, invalidAutoFastConfig("请选择有效的OpenAI分组")
	}
	if err != nil {
		return nil, err
	}
	state, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	if rule := state.Rules[groupID]; rule != nil {
		return rule, nil
	}
	return &AccountAutoFastRule{GroupID: groupID, Config: DefaultAccountAutoFastConfig(), Accounts: []AccountAutoFastObservation{}, Events: []AccountAutoFastEvent{}}, nil
}

func (s *AccountAutoFastService) Configure(ctx context.Context, groupID, revision int64, c AccountAutoFastConfig) error {
	if err := c.Validate(); err != nil {
		return invalidAutoFastConfig(err.Error())
	}
	g, err := s.groups.GetByID(ctx, groupID)
	if errors.Is(err, ErrGroupNotFound) || (err == nil && (g == nil || g.Platform != PlatformOpenAI || (c.Enabled && g.Status != StatusActive))) {
		return invalidAutoFastConfig("请选择有效的OpenAI分组")
	}
	if err != nil {
		return err
	}
	return s.store.Update(ctx, func(state *AccountAutoFastState, apply AccountAutoFastApply) error {
		rule := state.Rules[groupID]
		if rule == nil {
			if len(state.Rules) >= 32 {
				return invalidAutoFastConfig("最多配置32个自动Fast分组")
			}
			rule = &AccountAutoFastRule{GroupID: groupID, Accounts: []AccountAutoFastObservation{}, Events: []AccountAutoFastEvent{}}
		}
		if rule.Revision != revision {
			return ErrAccountAutoFastConflict
		}
		if c.Enabled {
			pool, err := s.accounts.ListByGroup(ctx, groupID)
			if err != nil {
				return err
			}
			if len(pool) > 2000 {
				return invalidAutoFastConfig("自动Fast单个分组最多支持2000个账号")
			}
			for _, a := range pool {
				if !a.IsOpenAI() {
					continue
				}
				for _, id := range a.GroupIDs {
					if other := state.Rules[id]; id != groupID && other != nil && other.Config.Enabled {
						return invalidAutoFastConfig(fmt.Sprintf("账号#%d已属于启用自动Fast的分组#%d，请先关闭其中一组", a.ID, id))
					}
				}
			}
		}
		rule.Config = c
		rule.Config.Stages = slices.Clone(c.Stages)
		rule.Revision++
		state.Rules[groupID] = rule
		// Disabling restores only flags still owned by this automation. A manual
		// edit made since the last applied value is retained.
		if !c.Enabled {
			for id, managed := range state.Managed {
				if managed.GroupID != groupID {
					continue
				}
				if err := s.restore(ctx, state, apply, id, managed, time.Now().UTC(), "自动Fast已关闭，恢复接管前状态"); err != nil {
					return err
				}
			}
			for i := range rule.Accounts {
				o := &rule.Accounts[i]
				if !o.Managed {
					o.Mode, o.Message = "disabled", "自动Fast已关闭"
				}
			}
		}
		state.CheckedAt = time.Time{}
		return nil
	})
}

// Unknown/expired snapshots do not become zero-percent quota. The final time
// window deliberately requires no quota sample and never sends a probe.
func autoFastUsed(a *Account, c AccountAutoFastConfig, now time.Time) (float64, bool) {
	used, ok := resolveAccountExtraNumber(a.Extra, "codex_7d_used_percent")
	if !ok || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return 0, false
	}
	at, err := parseTime(fmt.Sprint(a.Extra["codex_usage_updated_at"]))
	if err != nil || at.After(now.Add(time.Minute)) || now.Sub(at) > time.Duration(c.UsageMaxAgeSeconds)*time.Second {
		return 0, false
	}
	if raw, exists := a.Extra["codex_7d_reset_at"]; exists {
		reset, err := parseTime(fmt.Sprint(raw))
		if err != nil || !reset.After(now) {
			return 0, false
		}
	} else if _, exists := a.Extra["codex_7d_reset_after_seconds"]; exists {
		seconds, valid := resolveAccountExtraNumber(a.Extra, "codex_7d_reset_after_seconds")
		if !valid || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > 31*86400 || !at.Add(time.Duration(seconds)*time.Second).After(now) {
			return 0, false
		}
	}
	return used, true
}

func evaluateAutoFast(a *Account, c AccountAutoFastConfig, now time.Time) (AccountAutoFastObservation, *bool) {
	o := AccountAutoFastObservation{AccountID: a.ID, Name: a.Name, Fast: a.IsOpenAIForceFastEnabled()}
	o.Deadline, o.Source = a.SourceExpiresAt()
	used, valid := autoFastUsed(a, c, now)
	if valid {
		o.Used = &used
	}
	if !a.IsOpenAI() || a.Status != StatusActive {
		o.Mode, o.Message = "inactive", "账号未启用，退出自动接管"
		return o, nil
	}
	if o.Deadline == nil {
		o.Mode, o.Message = "no_deadline", "没有回收或到期时间"
		return o, nil
	}
	remaining := o.Deadline.Sub(now)
	if remaining <= 0 || (a.ExpiresAt != nil && !a.ExpiresAt.After(now)) {
		o.Mode, o.Message = "expired", "已到回收或到期时间，退出自动接管"
		return o, nil
	}
	if remaining <= time.Duration(c.FinalMinutes)*time.Minute {
		on := true
		o.Mode, o.Message, o.Stage = "final", "最终阶段，持续强制Fast", c.FinalMinutes
		return o, &on
	}
	var stage *AccountAutoFastStage
	for i := range c.Stages {
		if remaining <= time.Duration(c.Stages[i].Minutes)*time.Minute {
			stage = &c.Stages[i]
		}
	}
	if stage == nil {
		o.Mode, o.Message = "waiting", "尚未进入规则时间段"
		return o, nil
	}
	o.Stage = stage.Minutes
	if !valid {
		o.Mode, o.Message = "quota_unknown", "7D数据缺失或过期，保持当前状态等待新数据"
		return o, nil
	}
	on := used < stage.UsedBelowPercent
	if on {
		o.Mode, o.Message = "threshold_met", "7D已用比例低于当前阶段阈值，开启Fast"
	} else {
		o.Mode, o.Message = "threshold_not_met", "7D已用比例达到当前阶段阈值，关闭Fast"
	}
	return o, &on
}

func autoFastEvent(rule *AccountAutoFastRule, now time.Time, id int64, on bool, reason string) {
	if rule == nil {
		return
	}
	rule.Events = append([]AccountAutoFastEvent{{At: now, AccountID: id, Enabled: on, Reason: reason}}, rule.Events...)
	if len(rule.Events) > 50 {
		rule.Events = rule.Events[:50]
	}
}

// Count the whole group, independently of the time stage and quota snapshot
// needed to turn Fast on. Expired/reclaimed or currently blocked accounts cannot
// act as the second available account. Busy concurrency slots do not remove one.
func autoFastEligibleAccounts(pool []Account, now time.Time) int {
	count := 0
	for i := range pool {
		a := &pool[i]
		if !a.IsOpenAI() || !a.isSchedulableAt(now) || EvaluateAccountSchedulingThreshold(a, nil, now).ShouldPause {
			continue
		}
		deadline, _ := a.SourceExpiresAt()
		if (deadline != nil && !deadline.After(now)) || (a.ExpiresAt != nil && !a.ExpiresAt.After(now)) {
			continue
		}
		count++
	}
	return count
}

func (s *AccountAutoFastService) restore(ctx context.Context, state *AccountAutoFastState, apply AccountAutoFastApply, id int64, managed AccountAutoFastManaged, now time.Time, reason string) error {
	a, err := s.accounts.GetByID(ctx, id)
	if errors.Is(err, ErrAccountNotFound) {
		delete(state.Managed, id)
		return nil
	}
	if err != nil {
		return err
	}
	if a == nil {
		delete(state.Managed, id)
		return nil
	}
	fast, pending := a.IsOpenAIForceFastEnabled(), false
	defer func() {
		for _, rule := range state.Rules {
			for i := range rule.Accounts {
				o := &rule.Accounts[i]
				if o.AccountID == id {
					o.Fast, o.Managed = fast, pending
					if pending {
						o.Message = "账号同时被更新，等待下一轮恢复原状态"
					}
				}
			}
		}
	}()
	if a.IsOpenAIForceFastEnabled() == managed.Applied && managed.Applied != managed.Original {
		ok, err := apply(0, a, managed.Original)
		if err != nil {
			return err
		}
		if !ok {
			pending = true
			return nil
		}
		fast = managed.Original
		autoFastEvent(state.Rules[managed.GroupID], now, id, managed.Original, reason)
	}
	delete(state.Managed, id)
	return nil
}

func (s *AccountAutoFastService) tick(ctx context.Context, now time.Time) error {
	return s.store.Update(ctx, func(state *AccountAutoFastState, apply AccountAutoFastApply) error {
		if !state.CheckedAt.IsZero() && now.Sub(state.CheckedAt) < accountAutoFastInterval {
			return nil
		}
		ids := make([]int64, 0)
		for id, rule := range state.Rules {
			if rule.Config.Enabled {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 && len(state.Managed) == 0 {
			return nil
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		owners := map[int64][]int64{}
		pools := map[int64][]Account{}
		for _, id := range ids {
			group, err := s.groups.GetByID(ctx, id)
			if errors.Is(err, ErrGroupNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if group == nil || group.Status != StatusActive || group.Platform != PlatformOpenAI {
				continue
			}
			pool, err := s.accounts.ListByGroup(ctx, id)
			if err != nil {
				return err
			}
			if len(pool) > 2000 {
				return errors.New("自动Fast分组账号数超过2000")
			}
			pools[id] = pool
			for _, a := range pool {
				if a.IsOpenAI() {
					owners[a.ID] = append(owners[a.ID], id)
				}
			}
		}
		if len(owners) > 5000 {
			return errors.New("自动Fast账号总数超过5000")
		}
		retained := map[int64]bool{}
		for _, id := range ids {
			rule := state.Rules[id]
			rule.CheckedAt, rule.Total, rule.Accounts = now, len(pools[id]), []AccountAutoFastObservation{}
			rule.EligibleAccounts = autoFastEligibleAccounts(pools[id], now)
			for i := range pools[id] {
				a := &pools[id][i]
				o, desired := evaluateAutoFast(a, rule.Config, now)
				if rule.Config.RequireMultipleAccounts && rule.EligibleAccounts < 2 && o.Mode != "inactive" && o.Mode != "expired" {
					// This prerequisite wins over final-stage forcing and stale quota
					// retention. Keep ownership while off so original=true cannot be
					// restored by the ordinary exit path during single-account mode.
					off := false
					desired = &off
					o.Mode, o.Message = "insufficient_accounts", fmt.Sprintf("当前仅 %d 个可调度账号，少于2个，关闭Fast", rule.EligibleAccounts)
				}
				if len(owners[a.ID]) > 1 {
					o.Mode, o.Message, desired = "conflict", "账号属于多个已启用规则分组，暂停接管", nil
				}
				managed, existing := state.Managed[a.ID]
				if desired != nil {
					// A different owner is restored below before the next tick takes over.
					if !existing || managed.GroupID == id {
						if !existing {
							managed = AccountAutoFastManaged{GroupID: id, Original: o.Fast, Applied: o.Fast}
						}
						if existing && o.Fast != managed.Applied {
							managed.Original = o.Fast
						}
						ok := true
						if o.Fast != *desired {
							var err error
							ok, err = apply(id, a, *desired)
							if err != nil {
								return err
							}
							if ok {
								autoFastEvent(rule, now, a.ID, *desired, o.Message)
								o.Fast = *desired
							}
						}
						if ok {
							managed.Applied = *desired
							state.Managed[a.ID] = managed
							o.Managed = true
						}
						if !ok {
							o.Managed = existing
							o.Message = "账号同时被更新，等待下一轮重新判断"
						}
						retained[a.ID] = true
					}
				} else if o.Mode == "quota_unknown" && existing && managed.GroupID == id {
					retained[a.ID], o.Managed = true, true
				}
				if len(rule.Accounts) < 200 {
					rule.Accounts = append(rule.Accounts, o)
				}
			}
		}
		for id, managed := range state.Managed {
			if !retained[id] {
				if err := s.restore(ctx, state, apply, id, managed, now, "退出规则接管，恢复原状态"); err != nil {
					return err
				}
			}
		}
		state.CheckedAt = now
		return nil
	})
}
