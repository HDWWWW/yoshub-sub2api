package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

const AccountRefillSettingKey = "account_refill_v1"
const AccountImportExpiryOffsetMinutes = 60

var ErrAccountRefillBusy = errors.New("补号任务正在执行，请稍后刷新")

// AccountRefillStore serializes config changes, durable job reservations and
// execution across instances. save must be durable before any paid request.
type AccountRefillStore interface {
	Read(context.Context) (string, error)
	WithLock(context.Context, func(string, func(string) error) error) error
}

// All group stores share the same database advisory lock, including the legacy
// single-rule store. Separate keys keep rolling deployments backward compatible.
type AccountRefillGroupStore interface {
	ForGroup(int64) AccountRefillStore
	ListRules(context.Context) (map[int64]string, error)
}

type AccountRefillRotationStep struct {
	Plan  string `json:"plan"`
	Count int    `json:"count"`
}

type AccountRefillConfig struct {
	RotationEnabled           bool                            `json:"rotation_enabled"`
	Rotation                  []AccountRefillRotationStep     `json:"rotation"`
	Notifications             AccountRefillNotificationConfig `json:"notifications"`
	Enabled                   bool                            `json:"enabled"`
	MonitorEnabled            bool                            `json:"monitor_enabled"`
	Mode                      string                          `json:"mode"`
	GroupID                   int64                           `json:"group_id"`
	DeliveryGroupIDs          []int64                         `json:"delivery_group_ids"`
	PushEnabled               bool                            `json:"push_enabled"`
	MaxInFlight               int                             `json:"max_in_flight"`
	IntervalMinutes           int                             `json:"interval_minutes"`
	IntervalQuantity          int                             `json:"interval_quantity"`
	Plan                      string                          `json:"plan"`
	MinReady                  int                             `json:"min_ready"`
	MaxReady                  int                             `json:"max_ready"`
	LeadMinutes               int                             `json:"lead_minutes"`
	MinRemaining5h            float64                         `json:"min_remaining_5h"`
	MinRemaining7d            float64                         `json:"min_remaining_7d"`
	TargetConcurrency         int                             `json:"target_concurrency_percent"`
	ConcurrencySustainSeconds int                             `json:"concurrency_sustain_seconds"`
	ImportExpiryGraceMinutes  int                             `json:"import_expiry_grace_minutes"`
	CheckSeconds              int                             `json:"check_seconds"`
	CooldownSeconds           int                             `json:"cooldown_seconds"`
	DailyMaxCount             int                             `json:"daily_max_count"`
	DailyMaxPoints            float64                         `json:"daily_max_points"`
	MaxUnitPrice              float64                         `json:"max_unit_price"`
	ImportConcurrency         int                             `json:"import_concurrency"`
	WindowEnabled             bool                            `json:"window_enabled"`
	WindowStart               string                          `json:"window_start"`
	WindowEnd                 string                          `json:"window_end"`
	Delivery                  AccountRefillDeliveryConfig     `json:"delivery"`
}

type AccountRefillDeliveryConfig struct {
	WSMode             string            `json:"ws_mode"`
	ProxyMode          string            `json:"proxy_mode"`
	ProxyID            *int64            `json:"proxy_id"`
	NameTemplate       string            `json:"name_template"`
	NotesTemplate      string            `json:"notes_template"`
	Priority           int               `json:"priority"`
	LoadFactor         int               `json:"load_factor"`
	RateMultiplier     float64           `json:"rate_multiplier"`
	AutoPauseOnExpired bool              `json:"auto_pause_on_expired"`
	Status             string            `json:"status"`
	Schedulable        bool              `json:"schedulable"`
	FingerprintMode    string            `json:"fingerprint_mode"`
	ExpiryMode         string            `json:"expiry_mode"`
	FixedExpiresAt     *int64            `json:"fixed_expires_at"`
	ModelMapping       map[string]string `json:"model_mapping"`
}

func DefaultAccountRefillConfig() AccountRefillConfig {
	return AccountRefillConfig{Notifications: AccountRefillNotificationConfig{LowBalanceThreshold: 100}, MonitorEnabled: true, Mode: "capacity", DeliveryGroupIDs: []int64{}, PushEnabled: true, MaxInFlight: 1, IntervalMinutes: 60, IntervalQuantity: 1,
		Plan: "pro5x", MinReady: 1, MaxReady: 20, LeadMinutes: 30,
		MinRemaining5h: 15, MinRemaining7d: 15, TargetConcurrency: 85, ConcurrencySustainSeconds: 30, ImportExpiryGraceMinutes: 60, CheckSeconds: 60,
		CooldownSeconds: 300, DailyMaxCount: 50, DailyMaxPoints: 3000, MaxUnitPrice: 60,
		ImportConcurrency: 60, WindowStart: "09:00", WindowEnd: "23:00",
		Delivery: AccountRefillDeliveryConfig{WSMode: "http_bridge", ProxyMode: "direct", AutoPauseOnExpired: true, Priority: 1, RateMultiplier: 1, Status: StatusActive, Schedulable: true, FingerprintMode: "full", ExpiryMode: "rental", ModelMapping: map[string]string{}}}
}

func (c AccountRefillConfig) Validate() error {
	if err := validateRefillRotation(c); err != nil {
		return err
	}
	if math.IsNaN(c.Notifications.LowBalanceThreshold) || math.IsInf(c.Notifications.LowBalanceThreshold, 0) || c.Notifications.LowBalanceThreshold < 0 || c.Notifications.LowBalanceThreshold > 1e7 {
		return errors.New("余额预警阈值须为 0–10000000 积分")
	}
	if c.GroupID <= 0 || !slices.Contains([]string{"pro5x", "pro20x"}, c.Plan) ||
		!slices.Contains([]string{"capacity", "fixed", "interval"}, c.Mode) || !slices.Contains(c.DeliveryGroupIDs, c.GroupID) || len(c.DeliveryGroupIDs) > 100 ||
		c.MaxInFlight < 1 || c.MaxInFlight > 20 || c.IntervalMinutes < 1 || c.IntervalMinutes > 10080 || c.IntervalQuantity < 1 || c.IntervalQuantity > 100 ||
		c.MinReady < 1 || c.MaxReady < c.MinReady || c.MaxReady > 100 ||
		c.LeadMinutes < 0 || c.LeadMinutes > 1440 || c.CheckSeconds < 15 || c.CheckSeconds > 3600 ||
		c.CooldownSeconds < 60 || c.CooldownSeconds > 86400 || c.TargetConcurrency < 1 || c.TargetConcurrency > 100 ||
		c.ConcurrencySustainSeconds < 1 || c.ConcurrencySustainSeconds > 3600 || c.ImportExpiryGraceMinutes < 0 || c.ImportExpiryGraceMinutes > 1440 ||
		c.DailyMaxCount < 1 || c.DailyMaxCount > 1000 || c.ImportConcurrency < 1 || c.ImportConcurrency > 10000 {
		return errors.New("补号参数超出允许范围")
	}
	for _, v := range []float64{c.MinRemaining5h, c.MinRemaining7d, c.DailyMaxPoints, c.MaxUnitPrice} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e7 {
			return errors.New("额度或积分参数无效")
		}
	}
	if c.MinRemaining5h > 100 || c.MinRemaining7d > 100 || c.DailyMaxPoints <= 0 {
		return errors.New("额度阈值须为 0–100，每日积分上限须大于 0")
	}
	if _, err := time.Parse("15:04", c.WindowStart); err != nil {
		return errors.New("开始时间格式应为 HH:mm")
	}
	if _, err := time.Parse("15:04", c.WindowEnd); err != nil || (c.WindowEnabled && c.WindowStart == c.WindowEnd) {
		return errors.New("结束时间格式应为 HH:mm，限定时段的开始和结束不能相同")
	}
	d := c.Delivery
	if !slices.Contains([]string{"off", "ctx_pool", "http_bridge", "passthrough"}, d.WSMode) || !slices.Contains([]string{"off", "session", "full"}, d.FingerprintMode) ||
		!slices.Contains([]string{StatusActive, "inactive"}, d.Status) || d.Priority < 0 || d.LoadFactor < 0 || d.LoadFactor > 10000 ||
		math.IsNaN(d.RateMultiplier) || math.IsInf(d.RateMultiplier, 0) || d.RateMultiplier < 0 || d.RateMultiplier > 1000 || len(d.NameTemplate) > 256 || len(d.NotesTemplate) > 2000 || len(d.ModelMapping) > 200 {
		return errors.New("账号推送参数无效")
	}
	if !slices.Contains([]string{"direct", "specified", "random"}, d.ProxyMode) {
		return errors.New("代理模式无效")
	}
	if _, err := NormalizeAccountProxyMode(d.ProxyMode, d.ProxyID, false); err != nil {
		return err
	}
	if !slices.Contains([]string{"rental", "fixed"}, d.ExpiryMode) || (d.ExpiryMode == "fixed" && (d.FixedExpiresAt == nil || (c.Enabled && *d.FixedExpiresAt <= time.Now().Unix()))) {
		return errors.New("指定截止时间必须在未来")
	}
	for k, v := range d.ModelMapping {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" || len(k) > 256 || len(v) > 256 {
			return errors.New("模型映射须包含有效的来源和目标模型")
		}
	}
	if c.Enabled && (!d.Schedulable || d.Status != StatusActive) {
		return errors.New("自动补号须将新账号设为正常且允许调度，避免持续补入不可用账号")
	}
	return nil
}

type AccountRefillJob struct {
	StartedNotificationAt *time.Time                  `json:"started_notification_at,omitempty"`
	ManualRequestID       string                      `json:"manual_request_id,omitempty"`
	ConfigRevision        int64                       `json:"config_revision,omitempty"`
	ExpiryGraceMinutes    *int                        `json:"expiry_grace_minutes,omitempty"`
	ID                    string                      `json:"id"`
	SupplierID            string                      `json:"supplier_id,omitempty"`
	Status                string                      `json:"status"`
	SupplierStatus        string                      `json:"supplier_status,omitempty"`
	Reason                string                      `json:"reason"`
	ReasonDetail          string                      `json:"reason_detail,omitempty"`
	Plan                  string                      `json:"plan"`
	Price                 float64                     `json:"price"`
	PriceVersion          int64                       `json:"price_version"`
	GroupID               int64                       `json:"group_id"`
	Concurrency           int                         `json:"concurrency"`
	DeliveryGroupIDs      []int64                     `json:"delivery_group_ids"`
	DeliveryConfig        AccountRefillDeliveryConfig `json:"delivery_config"`
	BudgetReleased        bool                        `json:"budget_released"`
	AccountID             int64                       `json:"account_id,omitempty"`
	CreatedAt             time.Time                   `json:"created_at"`
	UpdatedAt             time.Time                   `json:"updated_at"`
	FinishedAt            *time.Time                  `json:"finished_at,omitempty"`
	ExpiresAt             *int64                      `json:"expires_at,omitempty"`
	ResultReady           bool                        `json:"result_ready"`
	Error                 string                      `json:"error,omitempty"`
	Attempts              int                         `json:"attempts"`
	NextAttemptAt         time.Time                   `json:"next_attempt_at"`
}

func (j AccountRefillJob) pending() bool {
	return !slices.Contains([]string{"imported", "failed", "delivery_expired", "dismissed"}, j.Status)
}

type AccountRefillAccount struct {
	ExpirySource    string     `json:"expiry_source"`
	SourceSyncedAt  string     `json:"source_synced_at,omitempty"`
	ID              int64      `json:"id"`
	Name            string     `json:"name"`
	State           string     `json:"state"`
	ExpiresAt       *time.Time `json:"expires_at"`
	SourceExpiresAt *time.Time `json:"source_expires_at"`
	Remaining5h     *float64   `json:"remaining_5h"`
	Remaining7d     *float64   `json:"remaining_7d"`
	Concurrency     int        `json:"concurrency"`
	InUse           int        `json:"in_use"`
}

type AccountRefillEvaluation struct {
	Error     string                 `json:"error,omitempty"`
	CheckedAt time.Time              `json:"checked_at"`
	Ready     int                    `json:"ready"`
	Healthy   int                    `json:"healthy"`
	Expiring  int                    `json:"expiring"`
	Pending   int                    `json:"pending"`
	InUse     int                    `json:"in_use"`
	Capacity  int                    `json:"capacity"`
	Needed    int                    `json:"needed"`
	Reason    string                 `json:"reason"`
	Accounts  []AccountRefillAccount `json:"accounts"`
}

type accountRefillState struct {
	RotationCursor     int                                `json:"rotation_cursor"`
	EncryptedWebhook   string                             `json:"encrypted_webhook,omitempty"`
	NotificationStatus AccountRefillNotificationStatus    `json:"notification_status"`
	LowBalanceAlerted  bool                               `json:"low_balance_alerted"`
	LowBalanceRequired float64                            `json:"low_balance_required"`
	NextBalanceCheckAt time.Time                          `json:"next_balance_check_at"`
	LeaseSyncAt        *time.Time                         `json:"lease_sync_at,omitempty"`
	LeaseSyncMessage   string                             `json:"lease_sync_message"`
	LeaseSyncCursor    int                                `json:"lease_sync_cursor"`
	Config             AccountRefillConfig                `json:"config"`
	EncryptedToken     string                             `json:"encrypted_token,omitempty"`
	Revision           int64                              `json:"revision"`
	Jobs               []AccountRefillJob                 `json:"jobs"`
	Catalog            *RefillCatalog                     `json:"catalog,omitempty"`
	CatalogAt          *time.Time                         `json:"catalog_at,omitempty"`
	Evaluation         AccountRefillEvaluation            `json:"evaluation"`
	NextCheckAt        time.Time                          `json:"next_check_at"`
	LastSubmitAt       time.Time                          `json:"last_submit_at"`
	DemandSince        *time.Time                         `json:"demand_since,omitempty"`
	Usage              map[string]AccountRefillDailyUsage `json:"usage"`
	NextIntervalAt     time.Time                          `json:"next_interval_at"`
	IntervalRemaining  int                                `json:"interval_remaining"`
	Wallet             *RefillWallet                      `json:"wallet,omitempty"`
	Key                *RefillKeyInfo                     `json:"key,omitempty"`
	IdentityAt         *time.Time                         `json:"identity_at,omitempty"`
	Message            string                             `json:"message"`
	ProviderRetryAt    time.Time                          `json:"provider_retry_at"`
}

type AccountRefillDailyUsage struct {
	Count          int     `json:"count"`
	Points         float64 `json:"points"`
	ReservedPoints float64 `json:"reserved_points"`
	SpentPoints    float64 `json:"spent_points"`
}

func decodeAccountRefillState(raw string) (accountRefillState, error) {
	state := accountRefillState{Config: DefaultAccountRefillConfig(), Jobs: []AccountRefillJob{}, Usage: map[string]AccountRefillDailyUsage{}}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return state, errors.New("补号状态读取失败，已停止提交")
		}
	}
	if state.Usage == nil {
		state.Usage = map[string]AccountRefillDailyUsage{}
	}
	return state, nil
}

// ImportedAccountExpiry preserves the native timestamp and records only a private
// scheduling grace. Legacy exports with an exact original+offset marker normalize
// once; subsequent imports never accumulate another offset.
func ImportedAccountExpiry(expires *int64, extra map[string]any) (*int64, map[string]any) {
	return ImportedAccountExpiryWithGrace(expires, extra, AccountImportExpiryOffsetMinutes)
}

func ImportedAccountExpiryWithGrace(expires *int64, extra map[string]any, minutes int) (*int64, map[string]any) {
	if minutes < 0 || minutes > 1440 {
		minutes = AccountImportExpiryOffsetMinutes
	}
	if expires == nil || *expires <= 0 || *expires > 253402214399 {
		return expires, extra
	}
	original := *expires
	if _, current := extra[AccountImportExpiryGraceExtraKey]; !current {
		if v, ok := resolveAccountExtraNumber(extra, "import_original_expires_at"); ok && v > 0 {
			offset, marked := resolveAccountExtraNumber(extra, "import_expiry_offset_seconds")
			if marked && offset > 0 && offset <= 86400 && v+offset == float64(original) {
				original = int64(v)
			}
		}
	}
	copyExtra := NativeAccountExtra(extra)
	if copyExtra == nil {
		copyExtra = make(map[string]any)
	}
	copyExtra[AccountImportExpiryGraceExtraKey] = minutes * 60
	copyExtra[AccountSourceExpiryExtraKey] = original
	copyExtra[AccountSourceExpiryOriginExtraKey] = "import"
	return &original, copyExtra
}

var refillTimezone = time.FixedZone("Asia/Shanghai", 8*60*60)

func refillInWindow(c AccountRefillConfig, now time.Time) bool {
	if !c.WindowEnabled {
		return true
	}
	clock := now.In(refillTimezone).Format("15:04")
	if c.WindowStart < c.WindowEnd {
		return clock >= c.WindowStart && clock < c.WindowEnd
	}
	return clock >= c.WindowStart || clock < c.WindowEnd
}

func evaluateAccountRefill(c AccountRefillConfig, accounts []Account, loads map[int64]*AccountLoadInfo, jobs []AccountRefillJob, now time.Time) AccountRefillEvaluation {
	e := AccountRefillEvaluation{CheckedAt: now, Accounts: []AccountRefillAccount{}}
	for _, job := range jobs {
		if job.pending() {
			e.Pending++
		}
	}
	for i := range accounts {
		a := &accounts[i]
		if !a.IsOpenAIOAuth() || a.IsShadow() || !slices.Contains(a.GroupIDs, c.GroupID) {
			continue
		}
		sourceExpiry, origin := a.SourceExpiresAt()
		synced, _ := a.Extra[AccountSourceExpiryCheckedExtraKey].(string)
		row := AccountRefillAccount{ID: a.ID, Name: a.Name, State: "ready", ExpiresAt: a.SchedulingExpiresAt(), SourceExpiresAt: sourceExpiry, ExpirySource: origin, SourceSyncedAt: synced, Concurrency: a.Concurrency}
		if load := loads[a.ID]; load != nil {
			row.InUse = load.CurrentConcurrency
		}
		quotaLow := false
		for _, w := range []struct {
			name        string
			minimum     float64
			destination **float64
		}{{"5h", c.MinRemaining5h, &row.Remaining5h}, {"7d", c.MinRemaining7d, &row.Remaining7d}} {
			used, valid := resolveAccountExtraNumber(a.Extra, "codex_"+w.name+"_used_percent")
			if !valid || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
				continue
			}
			if candidate := openAIThresholdCandidate(a.Extra, w.name, now); candidate != nil {
				remaining := math.Max(0, 100-candidate.usedPercent)
				*w.destination = &remaining
				if remaining < w.minimum {
					quotaLow = true
				}
			}
		}
		switch {
		case row.SourceExpiresAt != nil && !row.SourceExpiresAt.After(now):
			row.State = "expired"
		case !a.IsActive() || !a.Schedulable || (a.OverloadUntil != nil && now.Before(*a.OverloadUntil)) || (a.RateLimitResetAt != nil && now.Before(*a.RateLimitResetAt)) || (a.TempUnschedulableUntil != nil && now.Before(*a.TempUnschedulableUntil)):
			row.State = "unavailable"
		case quotaLow:
			row.State = "quota_low"
		case row.SourceExpiresAt != nil && !row.SourceExpiresAt.After(now.Add(time.Duration(c.LeadMinutes)*time.Minute)):
			row.State = "expiring"
			e.Expiring++
			e.Healthy++
		default:
			e.Ready++
			e.Healthy++
		}
		if row.State == "ready" || row.State == "expiring" {
			e.Capacity += refillMax(0, a.Concurrency)
			e.InUse += row.InUse
		}
		e.Accounts = append(e.Accounts, row)
	}
	minimumGap := refillMax(0, c.MinReady-e.Ready-e.Pending)
	capacityGap := 0
	if c.Mode == "capacity" && refillCapacityHigh(c, e) {
		capacityGap = refillMax(1, int(math.Ceil((float64(e.InUse)*100/float64(refillMax(1, c.TargetConcurrency))-float64(e.Capacity))/float64(refillMax(1, c.ImportConcurrency))))) - e.Pending
	}
	e.Needed = refillMax(minimumGap, capacityGap, 0)
	if minimumGap > 0 {
		e.Reason = "健康储备不足"
		if e.Expiring > 0 {
			e.Reason = "账号临期，提前补充储备"
		}
	} else if capacityGap > 0 {
		e.Reason = "并发利用率超过目标"
	} else {
		e.Reason = "储备充足"
	}
	for _, j := range jobs {
		if slices.Contains([]string{"blocked", "review", "import_failed"}, j.Status) {
			e.Needed = 0
			e.Reason = "原任务需要处理，暂停新增补号"
		}
	}
	if e.Pending >= c.MaxInFlight {
		e.Needed = 0
		e.Reason = "在途任务已抵扣缺口，等待交付"
	}
	e.Needed = refillMin(e.Needed, refillMax(0, c.MaxInFlight-e.Pending), refillMax(0, c.MaxReady-e.Ready-e.Pending))
	if e.Ready+e.Pending >= c.MaxReady {
		e.Needed = 0
		e.Reason = "已达健康账号上限"
	}
	if c.Mode == "interval" {
		e.Needed = 0
		e.Reason = "等待下一轮定时补号"
	}
	return e
}

// Named helpers avoid package-level min/max helpers in legacy unit tests.
func refillMax[T ~int | ~int64 | ~float64](values ...T) T {
	v := values[0]
	for _, n := range values[1:] {
		if n > v {
			v = n
		}
	}
	return v
}
func refillMin[T ~int | ~int64 | ~float64](values ...T) T {
	v := values[0]
	for _, n := range values[1:] {
		if n < v {
			v = n
		}
	}
	return v
}
