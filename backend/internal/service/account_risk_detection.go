package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"reflect"
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
	accountRiskProbeAll      = "all_probe"
)

var ErrAccountRiskBusy = errors.New("风控检测正在更新，请稍后重试")
var ErrAccountRiskConflict = errors.New("配置已被修改，请刷新后重试")

type AccountRiskErrorMatch struct {
	Status  int    `json:"status,omitempty"`
	Keyword string `json:"keyword,omitempty"`
}

type AccountRiskConfig struct {
	CustomErrorEnabled       bool                    `json:"custom_error_enabled"`
	CustomErrorWindowSeconds int                     `json:"custom_error_window_seconds"`
	CustomErrorThreshold     int                     `json:"custom_error_threshold"`
	CustomErrorRules         []AccountRiskErrorMatch `json:"custom_error_rules,omitempty"`

	RateLimit429Enabled       bool   `json:"rate_limit_429_enabled"`
	RateLimit429WindowSeconds int    `json:"rate_limit_429_window_seconds"`
	RateLimit429Threshold     int    `json:"rate_limit_429_threshold"`
	Enabled                   bool   `json:"enabled"`
	NotifyEnabled             bool   `json:"notify_enabled"`
	CapacityNotifyOnly        bool   `json:"capacity_notify_only"`
	ProbeEnabled              bool   `json:"probe_enabled"`
	ProbeScope                string `json:"probe_scope"`
	ProbeNotifyOnly           bool   `json:"probe_notify_only"`
	ProbeMismatchMaxRetries   int    `json:"probe_mismatch_max_retries"`
	Probe401Enabled           bool   `json:"probe_401_enabled"`
	Notify401Enabled          bool   `json:"notify_401_enabled"`
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
	return AccountRiskConfig{CustomErrorWindowSeconds: 60, CustomErrorThreshold: 20, RateLimit429Enabled: true, RateLimit429WindowSeconds: 60, RateLimit429Threshold: 20, ProbeEnabled: true, ProbeScope: "single", Probe429MaxRetries: 3, ModelMismatchAction: "off", FirstBelowPercent: 60, SecondAtLeastPercent: 90, SustainSeconds: 10, ProbeIntervalSeconds: 300,
		ProbePrompt: "请只回复数字 21，不要输出其他内容。", ExpectedText: "21", Model: "gpt-6.1-sol", ReasoningEffort: "high"}
}

// Missing fields in persisted rules and older API clients retain the existing
// single-account schedule. An explicit false must survive a save/reload.
func (c *AccountRiskConfig) UnmarshalJSON(data []byte) error {
	type configAlias AccountRiskConfig
	decoded := configAlias{ProbeEnabled: true, ProbeScope: "single"}
	// The original probe switch also sent 401 alerts. Only an absent new
	// field inherits it, so explicitly disabling alerts survives reload.
	var fields struct {
		*configAlias
		Notify401 *bool `json:"notify_401_enabled"`
	}
	fields.configAlias = &decoded
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	decoded.Notify401Enabled = decoded.Probe401Enabled
	if fields.Notify401 != nil {
		decoded.Notify401Enabled = *fields.Notify401
	}
	*c = AccountRiskConfig(decoded)
	return nil
}

func (c *AccountRiskConfig) Normalize429() {
	if c.CustomErrorWindowSeconds == 0 {
		c.CustomErrorWindowSeconds = 60
	}
	if c.CustomErrorThreshold == 0 {
		c.CustomErrorThreshold = 20
	}
	if len(c.CustomErrorRules) == 0 {
		c.CustomErrorRules = nil
	}
	for i := range c.CustomErrorRules {
		c.CustomErrorRules[i].Keyword = strings.TrimSpace(c.CustomErrorRules[i].Keyword)
	}
	if c.ProbeScope == "" {
		c.ProbeScope = "single"
	}
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
	if err := c.validateCustomErrors(); err != nil {
		return err
	}
	if c.ProbeMismatchMaxRetries < 0 || c.ProbeMismatchMaxRetries > 20 {
		return errors.New("定时测试回复不匹配最大重试次数须为 0–20")
	}
	if !slices.Contains([]string{"", "single", "all"}, c.ProbeScope) {
		return errors.New("定时检测范围须为单账号或全部账号")
	}
	if (c.Probe429StopEnabled && c.Probe429MaxRetries < 1) || c.Probe429MaxRetries < 0 || c.Probe429MaxRetries > 20 {
		return errors.New("定时测试 429 最大重试次数须为 1–20")
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
	ProbeSignature  string                               `json:"probe_signature,omitempty"`
	CapacityChecks  map[int64]*AccountRiskCapacityCheck  `json:"capacity_checks,omitempty"`
	ScheduledChecks map[int64]*AccountRiskScheduledCheck `json:"scheduled_checks,omitempty"`
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
	Signature string    `json:"signature,omitempty"`
}

type AccountRiskState struct {
	ActiveProbes       map[int64]*AccountRiskPending       `json:"active_probes,omitempty"`
	CapacityPauses     map[int64]*AccountRiskCapacityPause `json:"capacity_pauses,omitempty"`
	Rules              map[int64]*AccountRiskRule          `json:"rules"`
	Pending            *AccountRiskPending                 `json:"pending,omitempty"`
	NextNotificationAt time.Time                           `json:"next_notification_at,omitempty"`
}

type AccountRiskScheduling struct {
	ApplyProbe401 func(groupID int64, source, owner *Account, reason string, cooldownUntil *time.Time) (bool, error)
	Stop          func(groupID int64, account *Account) (bool, error)
	Pause         func(groupID int64, account *Account, token string) (bool, error)
	Resume        func(groupID int64, account *Account, token string) (bool, error)
}
type AccountRiskStore interface {
	Read(context.Context) (*AccountRiskState, error)
	Update(context.Context, func(*AccountRiskState, AccountRiskScheduling) error) error
}

type accountRiskAccounts interface {
	GetByIDs(context.Context, []int64) ([]*Account, error)
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
	// exhaustion, transport errors and error text never count as a 401 or 429.
	UpstreamStatus        int
	auth401               *riskProbe401Evidence
	Consecutive429        int
	ConsecutiveMismatches int
	MismatchRetry429      bool
}
type accountRiskProber interface {
	RunRiskProbe(context.Context, *Account, AccountRiskConfig) (AccountRiskProbeResult, error)
}

type AccountRiskService struct {
	onProbe401Committed func(context.Context, *Account, time.Time, bool)
	customErrorCache    accountRiskCustomCache
	counter429          AccountRisk429Counter
	store               AccountRiskStore
	accounts            accountRiskAccounts
	groups              accountRiskGroups
	loads               accountRiskLoads
	probe               accountRiskProber
	notifier            accountRiskNotifier
	notificationWake    chan struct{}
	cfg                 *config.Config
	worker              string
	cancel              context.CancelFunc
	wg                  sync.WaitGroup
	startOnce           sync.Once
	stopOnce            sync.Once
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
					jobs, err := s.tick(tickCtx, time.Now().UTC())
					stop()
					if err == nil {
						s.dispatchProbes(ctx, jobs)
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
	visible := *r
	visible.Events = slices.Clone(r.Events[:min(len(r.Events), 50)])
	for i, event := range visible.Events {
		visible.Events[i].Reason = riskNotificationReason(event)
	}
	v := &AccountRiskView{AccountRiskRule: &visible, WebhookStatus: "unavailable", TestAccounts: []AccountRiskTestAccount{}}
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
	defer s.customErrorCache.invalidate()
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
		// An auth-policy-only edit invalidates in-flight probes as usual, but it
		// must not strand accounts already waiting for a capacity recheck.
		previous := r.Config
		previous.Probe401Enabled = c.Probe401Enabled
		previous.Notify401Enabled = c.Notify401Enabled
		if r.Revision > 0 && reflect.DeepEqual(previous, c) && !reflect.DeepEqual(r.Config, c) {
			for _, p := range state.CapacityPauses {
				if p.GroupID == groupID && p.Revision == revision {
					p.Revision++
				}
			}
			r.Config = c
			r.Revision++
			return nil
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
	owners, err := s.probeCredentialOwners(ctx, accounts)
	if err != nil {
		return nil, err
	}
	pool := make([]Account, 0, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		if a.Platform != PlatformOpenAI || !a.isSchedulableAt(now) || EvaluateAccountSchedulingThreshold(a, nil, now).ShouldPause {
			continue
		}
		var owner *Account
		if a.IsShadow() {
			owner = owners[*a.ParentAccountID]
			if owner == nil {
				continue
			}
		}
		if !supplierReclaimed(riskProbeDeadline(a, owner), now) {
			pool = append(pool, *a)
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

func (s *AccountRiskService) tick(ctx context.Context, now time.Time) ([]*accountRiskJob, error) {
	var jobs []*accountRiskJob
	var scheduled []accountRiskScheduledCandidate
	newlyPaused := map[int64]Account{}
	err := s.store.Update(ctx, func(state *AccountRiskState, scheduling AccountRiskScheduling) error {
		expireRiskProbes(state, now)
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
			if !r.Config.ProbeEnabled || r.Config.ProbeScope != "all" {
				o.ScheduledChecks = nil
			}
			if len(pool) == 0 {
				o.First, o.Second = nil, nil
				o.CapacityChecks = nil
				o.ScheduledChecks = nil
				riskReset(o, "waiting", "没有可调度账号")
				o.NextProbeAt = nil
				continue
			}
			if len(pool) == 1 {
				o.Second = nil
				o.CapacityChecks = nil
				if !r.Config.ProbeEnabled {
					riskReset(o, "waiting", "定时检测已关闭")
					o.First, o.NextProbeAt = nil, nil
					continue
				}
				if r.Config.ProbeScope == "all" {
					riskReset(o, "scheduled", "全部账号定时检测")
					scheduled = append(scheduled, refreshAllAccountRiskProbes(r, pool, now)...)
					continue
				}
				a := pool[0]
				sig := riskAccountSignature(&a)
				if o.Mode != "single" || o.Signature != sig {
					o.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
				}
				riskReset(o, "single", "单账号定时检测")
				o.Signature = sig
				o.ProbeSignature = sig
				o.First = &AccountRiskLoad{AccountID: a.ID, Name: a.Name, Priority: a.SchedulingPriority(), Capacity: a.Concurrency}
				if riskSerialProbeBusy(state) || riskAccountProbeBusy(state, a.ID) || o.NextProbeAt == nil || o.NextProbeAt.After(now) {
					continue
				}
				jobs = append(jobs, reserveAccountRiskProbe(state, r, a, sig, accountRiskProbeSingle, "", now))
				o.Message = "正在发送单账号测试"
				continue
			}
			next, err := s.tickCapacity(ctx, state, r, pool, previousAt, now, scheduling)
			if err != nil {
				return err
			}
			if next != nil {
				jobs = append(jobs, next)
			}
			for _, a := range pool {
				if AccountHasRiskCapacityPause(&a) {
					newlyPaused[a.ID] = a
				}
			}
			scheduled = append(scheduled, refreshAllAccountRiskProbes(r, pool, now)...)
		}
		if next := s.tickCapacityPauses(ctx, state, newlyPaused, now); next != nil {
			jobs = append(jobs, next)
		}
		jobs = append(jobs, reserveAllAccountRiskProbes(state, scheduled, now)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.wakeNotifications()
	return jobs, nil
}

func (s *AccountRiskService) dispatchProbes(ctx context.Context, jobs []*accountRiskJob) {
	for _, job := range jobs {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.runProbe(ctx, job) }()
	}
}

func reserveAccountRiskProbe(state *AccountRiskState, r *AccountRiskRule, account Account, signature, kind, reason string, now time.Time) *accountRiskJob {
	pending := AccountRiskPending{ID: uuid.NewString(), GroupID: r.GroupID, AccountID: account.ID, Revision: r.Revision, ExpiresAt: now.Add(riskProbeTimeout(r.Config, kind) + 30*time.Second), Kind: kind, Signature: signature}
	if state.ActiveProbes == nil {
		state.ActiveProbes = make(map[int64]*AccountRiskPending)
	}
	state.ActiveProbes[account.ID] = &pending
	r.Observation.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
	return &accountRiskJob{pending: pending, account: account, config: r.Config, signature: signature, reason: reason}
}

// A capacity probe belongs to the same ordered pair that triggered it. A
// single-account probe remains valid only while that account is the sole target.
// An all-account probe depends only on its own account, never on a peer's result.
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
	if job.pending.Kind == accountRiskProbeAll {
		for i := range pool {
			if pool[i].ID == job.account.ID && riskScheduledAccountSignature(&pool[i]) == job.signature {
				return &pool[i], nil
			}
		}
		return nil, errors.New("scheduled account changed")
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
	trimAccountRiskEvents(r)
}

func (s *AccountRiskService) runProbe(parent context.Context, job *accountRiskJob) {
	ctx, cancel := context.WithTimeout(parent, riskProbeTimeout(job.config, job.pending.Kind))
	defer cancel()
	var result AccountRiskProbeResult
	// Check the persisted rule and current membership immediately before dispatch.
	err := s.validateProbeReservation(ctx, job)
	if err == nil {
		result, err = s.runProbeAttempts(ctx, job)
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(parent), 15*time.Second)
	defer finishCancel()
	// A busy transaction has not saved anything. Retry only saving the result,
	// never the upstream generation (including an uncertain transport failure).
	for {
		finishErr := s.finishProbe(finishCtx, job, result, err, time.Now().UTC())
		if finishErr == nil {
			return
		}
		if !errors.Is(finishErr, ErrAccountRiskBusy) || finishCtx.Err() != nil {
			slog.Warn("account risk result could not be saved", "group_id", job.pending.GroupID, "account_id", job.account.ID)
			return
		}
		select {
		case <-finishCtx.Done():
			return
		case <-time.After(time.Duration(25+rand.IntN(50)) * time.Millisecond):
		}
	}
}

func (s *AccountRiskService) finishProbe(ctx context.Context, job *accountRiskJob, result AccountRiskProbeResult, probeErr error, now time.Time) error {
	recorded := false
	var authOwner *Account
	var cooldownUntil time.Time
	err := s.store.Update(ctx, func(state *AccountRiskState, stop AccountRiskScheduling) error {
		released := releaseRiskProbe(state, job.pending)
		r := state.Rules[job.pending.GroupID]
		if r == nil {
			return nil
		}
		current := released && r.Config.Enabled && r.Revision == job.pending.Revision && (!isScheduledRiskProbe(job.pending.Kind) || r.Config.ProbeEnabled)
		observed401 := result.UpstreamStatus == http.StatusUnauthorized && result.auth401 != nil
		if !current || (observed401 && !job.pending.ExpiresAt.After(now)) {
			if observed401 && r.Config.Notify401Enabled {
				appendProbe401Event(r, job, result.auth401, now, "notify_only", "自动风控探测收到上游 401，探测任务已变更或结束")
				recorded = true
			}
			return nil
		}
		o := &r.Observation
		if errors.Is(probeErr, errRiskSupplierReclaimed) {
			o.LastProbeResult = errRiskSupplierReclaimed.Error()
			return nil
		}
		auth401 := (r.Config.Probe401Enabled || r.Config.Notify401Enabled) && observed401
		if auth401 {
			at := result.auth401.observedAt(now)
			reclaimed, err := s.risk401Reclaimed(ctx, &job.account, result.auth401.ownerID, at)
			if err != nil {
				return err
			}
			if reclaimed || supplierReclaimed(result.auth401.reclaimAt, at) {
				o.LastProbeResult = errRiskSupplierReclaimed.Error()
				if r.Config.Notify401Enabled {
					appendProbe401Event(r, job, result.auth401, now, "notify_only", "已发出的自动探测在号商回收后收到上游 401")
					recorded = true
				}
				return nil
			}
		}
		if job.pauseToken != "" {
			if !auth401 {
				return s.finishCapacityPause(ctx, state, stop, job, result, probeErr, now)
			}
			p := state.CapacityPauses[job.account.ID]
			if p == nil || p.Token != job.pauseToken || p.Revision != r.Revision || r.Config.CapacityNotifyOnly {
				if r.Config.Notify401Enabled {
					appendProbe401Event(r, job, result.auth401, now, "notify_only", "自动风控探测收到上游 401，容量暂停任务已变更")
					recorded = true
				}
				return nil
			}
			p.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
			o.NextProbeAt = p.NextProbeAt
		}
		o.LastProbeAt = &now
		if o.ProbeSignature == job.signature {
			o.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
		}
		if check := o.CapacityChecks[job.account.ID]; job.pending.Kind == accountRiskProbeCapacity && check != nil && check.TargetSignature == riskAccountSignature(&job.account) {
			check.NextProbeAt = riskAfter(now, r.Config.ProbeIntervalSeconds)
			check.LastProbeAt = &now
		}
		if check := o.ScheduledChecks[job.account.ID]; job.pending.Kind == accountRiskProbeAll && check != nil && check.Signature == job.signature {
			check.NextProbeAt = *riskAfter(now, r.Config.ProbeIntervalSeconds)
			check.LastProbeAt = &now
			defer func() { check.LastProbeResult = o.LastProbeResult }()
		}
		o.InputTokens = result.InputTokens
		o.OutputTokens = result.OutputTokens
		if auth401 {
			var applyErr error
			authOwner, cooldownUntil, applyErr = s.applyProbe401(ctx, r, stop, job, result.auth401, now)
			recorded = authOwner != nil || r.Config.Notify401Enabled
			return applyErr
		}
		exhausted429 := isScheduledRiskProbe(job.pending.Kind) && r.Config.Probe429StopEnabled &&
			r.Config.Probe429MaxRetries > 0 && result.Consecutive429 > r.Config.Probe429MaxRetries && result.UpstreamStatus == 429 && probeErr != nil
		mismatchRetry429 := isScheduledRiskProbe(job.pending.Kind) && r.Config.ProbeMismatchMaxRetries > 0 &&
			result.MismatchRetry429 && result.ConsecutiveMismatches > 0 && result.UpstreamStatus == 429 && probeErr != nil
		stop429 := exhausted429 || mismatchRetry429
		if !stop429 && (probeErr != nil || !result.Completed) {
			o.LastProbeResult = "检测未完成（网络、上游错误或账号繁忙），未据此停调度"
			return nil
		}
		account, err := s.probeAccount(ctx, job, now)
		if err != nil {
			o.LastProbeResult = "账号或分组已变化，本次结果作废"
			return nil
		}
		if !stop429 && strings.Contains(result.Text, r.Config.ExpectedText) {
			o.LastProbeResult = "回复包含期望内容，检测通过"
			return nil
		}
		if !stop429 && isScheduledRiskProbe(job.pending.Kind) && r.Config.ProbeMismatchMaxRetries > 0 && result.ConsecutiveMismatches <= r.Config.ProbeMismatchMaxRetries {
			o.LastProbeResult = "回复不匹配复测未完成，未据此停调度"
			return nil
		}
		kind, notifyOnly := job.pending.Kind, r.Config.ProbeNotifyOnly
		reason := "完整回复不含期望内容"
		if mismatchRetry429 {
			kind, notifyOnly = job.pending.Kind+"_mismatch_429", false
			reason = fmt.Sprintf("定时测试消息连续 %d 次完整回复不含期望内容，立即复测收到上游 429", result.ConsecutiveMismatches)
		} else if exhausted429 {
			kind, notifyOnly = job.pending.Kind+"_429", false
			reason = fmt.Sprintf("定时测试消息首次收到上游 429 后立即重试 %d 次，仍连续收到 429，无法完成检测", r.Config.Probe429MaxRetries)
		} else if job.pending.Kind == accountRiskProbeCapacity {
			kind, notifyOnly = accountRiskProbeCapacity, r.Config.CapacityNotifyOnly
			reason = job.reason + "；消息复测完整回复不含期望内容"
		} else if r.Config.ProbeMismatchMaxRetries > 0 {
			reason = fmt.Sprintf("定时测试消息首次完整回复不含期望内容后立即重试 %d 次，连续 %d 次均不匹配", r.Config.ProbeMismatchMaxRetries, result.ConsecutiveMismatches)
		}
		if notifyOnly {
			o.LastProbeResult = reason + "，仅提醒，未停止调度"
			event := AccountRiskEvent{ID: uuid.NewString(), At: now, AccountID: account.ID, AccountName: riskNotificationName(account.Name), Reason: o.LastProbeResult, Kind: kind, Action: "notify_only", Notification: "pending"}
			r.Events = append([]AccountRiskEvent{event}, r.Events...)
			trimAccountRiskEvents(r)
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
		if authOwner != nil && s.onProbe401Committed != nil {
			invalidate := result.auth401.failure == credential401Refreshable || result.auth401.failure == credential401MissingRefresh
			s.onProbe401Committed(ctx, authOwner, cooldownUntil, invalidate)
		}
		s.wakeNotifications()
	}
	return err
}
