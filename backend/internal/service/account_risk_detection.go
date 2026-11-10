package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
)

const AccountRiskSettingKey = "account_risk_detection_v1"

const (
	accountRiskProbeCapacity = "capacity_probe"
	accountRiskProbeSingle   = "single_probe"
)

var ErrAccountRiskBusy = errors.New("风控检测正在更新，请稍后重试")
var ErrAccountRiskConflict = errors.New("配置已被修改，请刷新后重试")

type AccountRiskConfig struct {
	RateLimit429Enabled       bool   `json:"rate_limit_429_enabled"`
	RateLimit429WindowSeconds int    `json:"rate_limit_429_window_seconds"`
	RateLimit429Threshold     int    `json:"rate_limit_429_threshold"`
	Enabled                   bool   `json:"enabled"`
	NotifyEnabled             bool   `json:"notify_enabled"`
	CapacityNotifyOnly        bool   `json:"capacity_notify_only"`
	ProbeNotifyOnly           bool   `json:"probe_notify_only"`
	Probe429StopEnabled       bool   `json:"probe_429_stop_enabled"`
	Probe429MaxRetries        int    `json:"probe_429_max_retries"`
	ModelMismatchAction       string `json:"model_mismatch_action"`
	FirstBelowPercent         int    `json:"first_below_percent"`
	SecondAtLeastPercent      int    `json:"second_at_least_percent"`
	SustainSeconds            int    `json:"sustain_seconds"`
	ProbeIntervalSeconds      int    `json:"probe_interval_seconds"`
	ProbePrompt               string `json:"probe_prompt"`
	ExpectedText              string `json:"expected_text"`
	Model                     string `json:"model"`
	ReasoningEffort           string `json:"reasoning_effort"`
}

func DefaultAccountRiskConfig() AccountRiskConfig {
	return AccountRiskConfig{RateLimit429Enabled: true, RateLimit429WindowSeconds: 60, RateLimit429Threshold: 20, Probe429MaxRetries: 3, ModelMismatchAction: "off", FirstBelowPercent: 60, SecondAtLeastPercent: 90, SustainSeconds: 10, ProbeIntervalSeconds: 300,
		ProbePrompt: "请只回复数字 21，不要输出其他内容。", ExpectedText: "21", Model: "gpt-6.1-sol", ReasoningEffort: "high"}
}

func (c *AccountRiskConfig) Normalize429() {
	if c.RateLimit429WindowSeconds == 0 {
		c.RateLimit429WindowSeconds = 60
	}
	if c.RateLimit429Threshold == 0 {
		c.RateLimit429Threshold = 20
	}
	if c.Probe429MaxRetries == 0 {
		c.Probe429MaxRetries = 3
	}
}

func (c AccountRiskConfig) Validate() error {
	if (c.Probe429StopEnabled && c.Probe429MaxRetries < 1) || c.Probe429MaxRetries < 0 || c.Probe429MaxRetries > 20 {
		return errors.New("单账号测试 429 最大重试次数须为 1–20")
	}
	if c.RateLimit429Enabled && (c.RateLimit429WindowSeconds < 1 || c.RateLimit429Threshold < 1) || c.RateLimit429WindowSeconds < 0 || c.RateLimit429WindowSeconds > 3600 || c.RateLimit429Threshold < 0 || c.RateLimit429Threshold > 10000 {
		return errors.New("429 时间窗口须为 1–3600 秒，次数须为 1–10000")
	}
	if !slices.Contains([]string{"", "off", "stop", "notify_only"}, c.ModelMismatchAction) {
		return errors.New("模型响应不一致的处理方式无效")
	}
	if c.FirstBelowPercent < 1 || c.FirstBelowPercent > 100 || c.SecondAtLeastPercent < 1 || c.SecondAtLeastPercent > 100 || c.FirstBelowPercent > c.SecondAtLeastPercent {
		return errors.New("容量阈值须为 1–100%，待检测账号阈值不能大于后位账号阈值")
	}
	if c.SustainSeconds < 1 || c.SustainSeconds > 3600 || c.ProbeIntervalSeconds < 10 || c.ProbeIntervalSeconds > 86400 {
		return errors.New("持续时间须为 1–3600 秒，测试间隔须为 10–86400 秒")
	}
	if strings.TrimSpace(c.ProbePrompt) == "" || len(c.ProbePrompt) > 8192 || strings.TrimSpace(c.ExpectedText) == "" || len(c.ExpectedText) > 256 {
		return errors.New("测试消息和期望内容不能为空，分别最多 8192 和 256 字节")
	}
	if strings.TrimSpace(c.Model) != c.Model || c.Model == "" || len(c.Model) > 200 || strings.ContainsAny(c.Model, "\r\n\x00") || isOpenAIImageModel(c.Model) {
		return errors.New("请填写有效的文本模型")
	}
	if !slices.Contains([]string{"", "none", "minimal", "low", "medium", "high", "xhigh"}, c.ReasoningEffort) {
		return errors.New("思考强度无效")
	}
	return nil
}

type AccountRiskLoad struct {
	AccountID int64   `json:"account_id"`
	Name      string  `json:"name"`
	Priority  int     `json:"priority"`
	Used      int     `json:"used"`
	Capacity  int     `json:"capacity"`
	Percent   float64 `json:"percent"`
}

type AccountRiskObservation struct {
	CheckedAt        time.Time        `json:"checked_at"`
	Mode             string           `json:"mode"`
	Message          string           `json:"message"`
	EligibleAccounts int              `json:"eligible_accounts"`
	First            *AccountRiskLoad `json:"first,omitempty"`
	Second           *AccountRiskLoad `json:"second,omitempty"`
	AnomalySince     *time.Time       `json:"anomaly_since,omitempty"`
	NextProbeAt      *time.Time       `json:"next_probe_at,omitempty"`
	LastProbeAt      *time.Time       `json:"last_probe_at,omitempty"`
	LastProbeResult  string           `json:"last_probe_result,omitempty"`
	InputTokens      int              `json:"input_tokens"`
	OutputTokens     int              `json:"output_tokens"`
	// Changes of identity, capacity, order, worker or sample continuity reset the timer.
	Signature string `json:"signature,omitempty"`
	Worker    string `json:"worker,omitempty"`
	// Keep the probe cooldown across interruptions in the capacity anomaly.
	ProbeSignature string                              `json:"probe_signature,omitempty"`
	CapacityChecks map[int64]*AccountRiskCapacityCheck `json:"capacity_checks,omitempty"`
}

// Each target has its own continuous sample window and probe cooldown. Keeping
// these separate prevents a passing higher-priority account from hiding others.
type AccountRiskCapacityCheck struct {
	Target          AccountRiskLoad  `json:"target"`
	Reference       *AccountRiskLoad `json:"reference,omitempty"`
	AnomalySince    *time.Time       `json:"anomaly_since,omitempty"`
	NextProbeAt     *time.Time       `json:"next_probe_at,omitempty"`
	LastProbeAt     *time.Time       `json:"last_probe_at,omitempty"`
	TargetSignature string           `json:"target_signature"`
	Signature       string           `json:"signature,omitempty"`
	Worker          string           `json:"worker,omitempty"`
}

type AccountRiskEvent struct {
	ID             string     `json:"id,omitempty"`
	At             time.Time  `json:"at"`
	AccountID      int64      `json:"account_id"`
	AccountName    string     `json:"account_name,omitempty"`
	Reason         string     `json:"reason"`
	Kind           string     `json:"kind,omitempty"`
	Action         string     `json:"action,omitempty"`
	Occurrences    int        `json:"occurrences,omitempty"`
	Notification   string     `json:"notification,omitempty"`
	NotificationAt *time.Time `json:"notification_at,omitempty"`
}

type AccountRiskRule struct {
	GroupID     int64                  `json:"group_id"`
	Revision    int64                  `json:"revision"`
	Config      AccountRiskConfig      `json:"config"`
	Observation AccountRiskObservation `json:"observation"`
	Events      []AccountRiskEvent     `json:"events"`
}

type AccountRiskPending struct {
	ID        string    `json:"id"`
	GroupID   int64     `json:"group_id"`
	AccountID int64     `json:"account_id"`
	Revision  int64     `json:"revision"`
	ExpiresAt time.Time `json:"expires_at"`
	Kind      string    `json:"kind,omitempty"`
}

type AccountRiskState struct {
	CapacityPauses     map[int64]*AccountRiskCapacityPause `json:"capacity_pauses,omitempty"`
	Rules              map[int64]*AccountRiskRule          `json:"rules"`
	Pending            *AccountRiskPending                 `json:"pending,omitempty"`
	NextNotificationAt time.Time                           `json:"next_notification_at,omitempty"`
}

type AccountRiskScheduling struct {
	Stop   func(groupID int64, account *Account) (bool, error)
	Pause  func(groupID int64, account *Account, token string) (bool, error)
	Resume func(groupID int64, account *Account, token string) (bool, error)
}
type AccountRiskStore interface {
	Read(context.Context) (*AccountRiskState, error)
	Update(context.Context, func(*AccountRiskState, AccountRiskScheduling) error) error
}

type accountRiskAccounts interface {
	ListByGroup(context.Context, int64) ([]Account, error)
	GetByID(context.Context, int64) (*Account, error)
}
type accountRiskGroups interface {
	GetByID(context.Context, int64) (*Group, error)
}
type accountRiskLoads interface {
	GetAccountsLoadBatchFresh(context.Context, []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error)
	AcquireAccountSlot(context.Context, int64, int) (*AcquireResult, error)
}
type AccountRiskProbeResult struct {
	Completed    bool
	Text         string
	InputTokens  int
	OutputTokens int
	// Only an actual HTTP response can populate UpstreamStatus. Local slot
	// exhaustion, transport errors and error text must never count as a 429.
	UpstreamStatus int
	Consecutive429 int
}
type accountRiskProber interface {
	RunRiskProbe(context.Context, *Account, AccountRiskConfig) (AccountRiskProbeResult, error)
}

type AccountRiskService struct {
	counter429       AccountRisk429Counter
	store            AccountRiskStore
	accounts         accountRiskAccounts
	groups           accountRiskGroups
	loads            accountRiskLoads
	probe            accountRiskProber
	notifier         accountRiskNotifier
	notificationWake chan struct{}
	cfg              *config.Config
	worker           string
	cancel           context.CancelFunc
	wg               sync.WaitGroup
	startOnce        sync.Once
	stopOnce         sync.Once
}

func NewAccountRiskService(store AccountRiskStore, accounts AccountRepository, groups GroupRepository, loads *ConcurrencyService, probe *AccountTestService, cfg *config.Config) *AccountRiskService {
	return &AccountRiskService{store: store, accounts: accounts, groups: groups, loads: loads, probe: probe, cfg: cfg, worker: uuid.NewString(), notificationWake: make(chan struct{}, 1)}
}

func (s *AccountRiskService) Start() {
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		s.wg.Add(1)
		go s.notificationLoop(ctx)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					tickCtx, stop := context.WithTimeout(ctx, 4*time.Second)
					job, err := s.tick(tickCtx, time.Now().UTC())
					stop()
					if err == nil && job != nil {
						s.wg.Add(1)
						go func() { defer s.wg.Done(); s.runProbe(ctx, job) }()
					}
				}
			}
		}()
	})
}
func (s *AccountRiskService) Stop() {
	if s != nil {
		s.stopOnce.Do(func() {
			if s.cancel != nil {
				s.cancel()
			}
		})
		s.wg.Wait()
	}
}

func (s *AccountRiskService) View(ctx context.Context, groupID int64) (*AccountRiskView, error) {
	state, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	r := state.Rules[groupID]
	if r == nil {
		r = &AccountRiskRule{GroupID: groupID, Config: DefaultAccountRiskConfig(), Events: []AccountRiskEvent{}, Observation: AccountRiskObservation{Mode: "disabled", Message: "检测未启用"}}
	}
	v := &AccountRiskView{AccountRiskRule: r, WebhookStatus: "unavailable", TestAccounts: []AccountRiskTestAccount{}}
	accounts, err := s.accounts.ListByGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		if a.Platform == PlatformOpenAI {
			v.TestAccounts = append(v.TestAccounts, AccountRiskTestAccount{ID: a.ID, Name: a.Name, Schedulable: a.Schedulable})
		}
	}
	sort.Slice(v.TestAccounts, func(i, j int) bool { return v.TestAccounts[i].ID < v.TestAccounts[j].ID })
	if s.notifier != nil {
		ready, err := s.notifier.RiskWebhookConfigured(ctx, groupID)
		if err == nil {
			v.WebhookStatus = "missing"
			if ready {
				v.WebhookStatus = "ready"
			}
		}
	}
	return v, nil
}

func (s *AccountRiskService) Configure(ctx context.Context, groupID, revision int64, c AccountRiskConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.Normalize429()
	g, err := s.groups.GetByID(ctx, groupID)
	if err != nil || g == nil || g.Platform != PlatformOpenAI || (c.Enabled && g.Status != StatusActive) {
		return errors.New("请选择有效的 OpenAI 分组")
	}
	return s.store.Update(ctx, func(state *AccountRiskState, _ AccountRiskScheduling) error {
		r := state.Rules[groupID]
		if r == nil {
			if len(state.Rules) >= 32 {
				return errors.New("最多配置 32 个检测分组")
			}
			r = &AccountRiskRule{GroupID: groupID, Events: []AccountRiskEvent{}}
		}
		if r.Revision != revision {
			return ErrAccountRiskConflict
		}
		r.Config = c
		r.Revision++
		r.Observation = AccountRiskObservation{Mode: "disabled", Message: "检测未启用"}
		if c.Enabled {
			r.Observation.Mode = "waiting"
			r.Observation.Message = "等待采样"
		}
		state.Rules[groupID] = r
		// Keep any in-flight reservation until it completes/expires. A revision
		// mismatch discards its result; toggling cannot cause overlapping probes.
		return nil
	})
}

func (s *AccountRiskService) candidates(ctx context.Context, groupID int64, now time.Time) ([]Account, error) {
	g, err := s.groups.GetByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if g == nil || g.Platform != PlatformOpenAI || g.Status != StatusActive {
		return nil, errors.New("group unavailable")
	}
	accounts, err := s.accounts.ListByGroup(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if len(accounts) > 2000 {
		return nil, errors.New("account sample exceeds limit")
	}
	pool := make([]Account, 0, len(accounts))
	for _, a := range accounts {
		if a.Platform == PlatformOpenAI && a.IsSchedulable() && !EvaluateAccountSchedulingThreshold(&a, nil, now).ShouldPause {
			pool = append(pool, a)
		}
	}
	pool = accountsWithExpiryPriority(pool, s.cfg, now)
	sort.SliceStable(pool, func(i, j int) bool {
		p, q := pool[i].SchedulingPriority(), pool[j].SchedulingPriority()
		if p == q {
			return pool[i].ID < pool[j].ID
		}
		if g.ReverseAccountPriority {
			return p > q
		}
		return p < q
	})
	return pool, nil
}

type accountRiskJob struct {
	pending     AccountRiskPending
	account     Account
	config      AccountRiskConfig
	signature   string
	reason      string
	referenceID int64
	pauseToken  string
}

func riskAccountSignature(a *Account) string {
	manual, overridden := a.ManualSchedulingPriority()
	data, _ := json.Marshal([]any{a.ID, a.Credentials, a.ProxyID, a.Priority, a.SchedulingPriority(), a.Concurrency, a.ExpiresAt, manual, overridden, a.GroupIDs})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func riskReset(o *AccountRiskObservation, mode, message string) {
	o.AnomalySince = nil
	o.Signature = ""
	o.Mode = mode
	o.Message = message
	if mode != "stopped" {
		for _, check := range o.CapacityChecks {
			check.AnomalySince = nil
			check.Reference = nil
			check.Signature = ""
		}
	}
}

func (s *AccountRiskService) tick(ctx context.Context, now time.Time) (*accountRiskJob, error) {
	var job *accountRiskJob
	newlyPaused := map[int64]Account{}
	err := s.store.Update(ctx, func(state *AccountRiskState, scheduling AccountRiskScheduling) error {
		if state.Pending != nil && !state.Pending.ExpiresAt.After(now) {
			if r := state.Rules[state.Pending.GroupID]; r != nil && state.Pending.Kind != "manual_probe" {
				r.Observation.LastProbeResult = "检测中断，未据此停调度"
				if pause := state.CapacityPauses[state.Pending.AccountID]; pause != nil {
					r.Observation.LastProbeResult = "检测中断，保持暂停，等待下次复测"
					pause.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
				}
				r.Observation.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
				if check := r.Observation.CapacityChecks[state.Pending.AccountID]; check != nil {
					check.NextProbeAt = r.Observation.NextProbeAt
				}
			}
			state.Pending = nil
		}
		ids := make([]int64, 0, len(state.Rules))
		for id := range state.Rules {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			r := state.Rules[id]
			if !r.Config.Enabled {
				continue
			}
			o := &r.Observation
			if !o.CheckedAt.IsZero() && now.Sub(o.CheckedAt) < time.Second {
				continue
			}
			previousAt := o.CheckedAt
			o.CheckedAt = now
			pool, err := s.candidates(ctx, id, now)
			if err != nil {
				riskReset(o, "unavailable", "账号或分组读取失败，本次不判定")
				continue
			}
			o.EligibleAccounts = len(pool)
			if len(pool) == 0 {
				o.First, o.Second = nil, nil
				o.CapacityChecks = nil
				riskReset(o, "waiting", "没有可调度账号")
				o.NextProbeAt = nil
				continue
			}
			if len(pool) == 1 {
				o.Second = nil
				o.CapacityChecks = nil
				a := pool[0]
				sig := riskAccountSignature(&a)
				if o.Mode != "single" || o.Signature != sig {
					o.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
				}
				riskReset(o, "single", "单账号定时检测")
				o.Signature = sig
				o.ProbeSignature = sig
				o.First = &AccountRiskLoad{AccountID: a.ID, Name: a.Name, Priority: a.SchedulingPriority(), Capacity: a.Concurrency}
				if state.Pending != nil || o.NextProbeAt == nil || o.NextProbeAt.After(now) {
					continue
				}
				job = reserveAccountRiskProbe(state, r, a, sig, accountRiskProbeSingle, "", now)
				o.Message = "正在发送单账号测试"
				continue
			}
			next, err := s.tickCapacity(ctx, state, r, pool, previousAt, now, scheduling)
			if err != nil {
				return err
			}
			if next != nil {
				job = next
			}
			for _, a := range pool {
				if AccountHasRiskCapacityPause(&a) {
					newlyPaused[a.ID] = a
				}
			}
		}
		if next := s.tickCapacityPauses(ctx, state, newlyPaused, now); next != nil {
			job = next
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.wakeNotifications()
	return job, nil
}

func reserveAccountRiskProbe(state *AccountRiskState, r *AccountRiskRule, account Account, signature, kind, reason string, now time.Time) *accountRiskJob {
	pending := AccountRiskPending{ID: uuid.NewString(), GroupID: r.GroupID, AccountID: account.ID, Revision: r.Revision, ExpiresAt: now.Add(120 * time.Second), Kind: kind}
	state.Pending = &pending
	r.Observation.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
	return &accountRiskJob{pending: pending, account: account, config: r.Config, signature: signature, reason: reason}
}

// A capacity probe belongs to the same ordered pair that triggered it. A
// single-account probe remains valid only while that account is the sole target.
func (s *AccountRiskService) probeAccount(ctx context.Context, job *accountRiskJob, now time.Time) (*Account, error) {
	if job.pauseToken != "" {
		return s.pausedProbeAccount(ctx, job.pending.GroupID, job.account.ID, job.pauseToken, job.signature, now)
	}
	pool, err := s.candidates(ctx, job.pending.GroupID, now)
	if err != nil {
		return nil, err
	}
	if job.pending.Kind == accountRiskProbeCapacity {
		for i := range pool {
			if pool[i].ID != job.account.ID {
				continue
			}
			for j := i + 1; j < len(pool); j++ {
				if pool[j].ID == job.referenceID && pool[j].SchedulingPriority() != pool[i].SchedulingPriority() &&
					riskAccountSignature(&pool[i])+riskAccountSignature(&pool[j]) == job.signature {
					return &pool[i], nil
				}
			}
		}
		return nil, errors.New("capacity pair changed")
	}
	if len(pool) != 1 || pool[0].ID != job.account.ID || riskAccountSignature(&pool[0]) != job.signature {
		return nil, errors.New("account identity or priority changed")
	}
	return &pool[0], nil
}

func riskAfter(now time.Time, seconds int) *time.Time {
	v := now.Add(time.Duration(seconds) * time.Second)
	return &v
}
func riskRecordStop(r *AccountRiskRule, account *Account, now time.Time, reason string) {
	riskReset(&r.Observation, "stopped", reason)
	event := AccountRiskEvent{ID: uuid.NewString(), At: now, AccountID: account.ID, AccountName: riskNotificationName(account.Name), Reason: reason}
	if r.Config.NotifyEnabled {
		event.Notification = "pending"
	}
	r.Events = append([]AccountRiskEvent{event}, r.Events...)
	if len(r.Events) > 50 {
		r.Events = r.Events[:50]
	}
}

func (s *AccountRiskService) runProbe(parent context.Context, job *accountRiskJob) {
	ctx, cancel := context.WithTimeout(parent, 90*time.Second)
	defer cancel()
	var result AccountRiskProbeResult
	// Check the persisted rule and current membership immediately before dispatch.
	r, err := s.View(ctx, job.pending.GroupID)
	if err == nil && (!r.Config.Enabled || r.Revision != job.pending.Revision) {
		err = errors.New("configuration changed")
	}
	if err == nil {
		_, err = s.probeAccount(ctx, job, time.Now())
	}
	if err == nil {
		var slot *AcquireResult
		slot, err = s.loads.AcquireAccountSlot(ctx, job.account.ID, job.account.Concurrency)
		if err == nil && slot != nil && slot.Acquired {
			defer slot.ReleaseFunc()
			result, err = s.runProbeAttempts(ctx, job)
		} else if err == nil {
			err = errors.New("account busy")
		}
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer finishCancel()
	// A busy transaction has not saved anything. Retry only saving the result,
	// never the upstream generation (including an uncertain transport failure).
	for attempt := 0; attempt < 5; attempt++ {
		finishErr := s.finishProbe(finishCtx, job, result, err, time.Now().UTC())
		if finishErr == nil {
			return
		}
		if !errors.Is(finishErr, ErrAccountRiskBusy) || attempt == 4 {
			slog.Warn("account risk result could not be saved", "group_id", job.pending.GroupID, "account_id", job.account.ID)
			return
		}
		select {
		case <-finishCtx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
		}
	}
}

func (s *AccountRiskService) finishProbe(ctx context.Context, job *accountRiskJob, result AccountRiskProbeResult, probeErr error, now time.Time) error {
	recorded := false
	err := s.store.Update(ctx, func(state *AccountRiskState, stop AccountRiskScheduling) error {
		if state.Pending == nil || state.Pending.ID != job.pending.ID {
			return nil
		}
		state.Pending = nil
		if job.pauseToken != "" {
			return s.finishCapacityPause(ctx, state, stop, job, result, probeErr, now)
		}
		r := state.Rules[job.pending.GroupID]
		if r == nil || !r.Config.Enabled || r.Revision != job.pending.Revision {
			return nil
		}
		o := &r.Observation
		o.LastProbeAt = &now
		if o.ProbeSignature == job.signature {
			o.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
		}
		if check := o.CapacityChecks[job.account.ID]; job.pending.Kind == accountRiskProbeCapacity && check != nil && check.TargetSignature == riskAccountSignature(&job.account) {
			check.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
			check.LastProbeAt = &now
		}
		o.InputTokens = result.InputTokens
		o.OutputTokens = result.OutputTokens
		exhausted429 := job.pending.Kind == accountRiskProbeSingle && r.Config.Probe429StopEnabled &&
			r.Config.Probe429MaxRetries > 0 && result.Consecutive429 > r.Config.Probe429MaxRetries && result.UpstreamStatus == 429 && probeErr != nil
		if !exhausted429 && (probeErr != nil || !result.Completed) {
			o.LastProbeResult = "检测未完成（网络、上游错误或账号繁忙），未据此停调度"
			return nil
		}
		account, err := s.probeAccount(ctx, job, now)
		if err != nil {
			o.LastProbeResult = "账号或分组已变化，本次结果作废"
			return nil
		}
		if !exhausted429 && strings.Contains(result.Text, r.Config.ExpectedText) {
			o.LastProbeResult = "回复包含期望内容，检测通过"
			return nil
		}
		kind, notifyOnly := accountRiskProbeSingle, r.Config.ProbeNotifyOnly
		reason := "完整回复不含期望内容"
		if exhausted429 {
			kind, notifyOnly = "single_probe_429", false
			reason = fmt.Sprintf("单账号定时测试消息首次收到上游 429 后立即重试 %d 次，仍连续收到 429，无法完成检测", r.Config.Probe429MaxRetries)
		} else if job.pending.Kind == accountRiskProbeCapacity {
			kind, notifyOnly = accountRiskProbeCapacity, r.Config.CapacityNotifyOnly
			reason = job.reason + "；消息复测完整回复不含期望内容"
		}
		if notifyOnly {
			o.LastProbeResult = reason + "，仅提醒，未停止调度"
			event := AccountRiskEvent{ID: uuid.NewString(), At: now, AccountID: account.ID, AccountName: riskNotificationName(account.Name), Reason: o.LastProbeResult, Kind: kind, Action: "notify_only", Notification: "pending"}
			r.Events = append([]AccountRiskEvent{event}, r.Events...)
			if len(r.Events) > 50 {
				r.Events = r.Events[:50]
			}
			recorded = true
			return nil
		}
		changed, err := stop.Stop(r.GroupID, account)
		if err != nil {
			return err
		}
		if changed {
			o.LastProbeResult = reason + "，已关闭账号调度"
			riskRecordStop(r, account, now, o.LastProbeResult)
			r.Events[0].Kind, r.Events[0].Action = kind, "stop"
			recorded = true
		} else {
			o.LastProbeResult = "账号已变化，本次结果作废"
		}
		return nil
	})
	if err == nil && recorded {
		s.wakeNotifications()
	}
	return err
}
