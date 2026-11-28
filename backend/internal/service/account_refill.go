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
const accountRefillMinIntervalSeconds = 5

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
	Provider string `json:"provider,omitempty"` // Empty inherits the group primary.
	Plan     string `json:"plan"`
	Count    int    `json:"count"`
}

type AccountRefillConfig struct {
	Supplier558669            *Refill558669Limits                    `json:"supplier_558669,omitempty"`
	PrimaryProvider           string                                 `json:"primary_provider,omitempty"`
	MuzeFallback              *AccountRefillMuzeFallbackConfig       `json:"muze_fallback,omitempty"`
	DailySchedules            []AccountRefillDailySchedule           `json:"daily_schedules"`
	DailySchedule             *AccountRefillDailySchedule            `json:"daily_schedule,omitempty"`
	TimeoutRetryEnabled       *bool                                  `json:"timeout_retry_enabled,omitempty"`
	TimeoutRetryNotify        *bool                                  `json:"timeout_retry_notify,omitempty"`
	TimeoutRetryMinutes       *int                                   `json:"timeout_retry_minutes,omitempty"`
	ImmediateFailureRetry     *bool                                  `json:"immediate_failure_retry,omitempty"`
	ForecastEnabled           *bool                                  `json:"forecast_enabled,omitempty"`
	RotationEnabled           bool                                   `json:"rotation_enabled"`
	Rotation                  []AccountRefillRotationStep            `json:"rotation"`
	Notifications             AccountRefillNotificationConfig        `json:"notifications"`
	Enabled                   bool                                   `json:"enabled"`
	MonitorEnabled            bool                                   `json:"monitor_enabled"`
	Mode                      string                                 `json:"mode"`
	GroupID                   int64                                  `json:"group_id"`
	DeliveryGroupIDs          []int64                                `json:"delivery_group_ids"`
	PushEnabled               bool                                   `json:"push_enabled"`
	MaxInFlight               int                                    `json:"max_in_flight"`
	IntervalMinutes           int                                    `json:"interval_minutes"`
	IntervalQuantity          int                                    `json:"interval_quantity"`
	Plan                      string                                 `json:"plan"`
	MinReady                  int                                    `json:"min_ready"`
	MaxReady                  int                                    `json:"max_ready"`
	LeadMinutes               int                                    `json:"lead_minutes"`
	MinRemaining5h            float64                                `json:"min_remaining_5h"`
	MinRemaining7d            float64                                `json:"min_remaining_7d"`
	PlanQuotaThresholds       map[string]AccountRefillQuotaThreshold `json:"plan_quota_thresholds,omitempty"`
	TargetConcurrency         int                                    `json:"target_concurrency_percent"`
	ConcurrencySustainSeconds int                                    `json:"concurrency_sustain_seconds"`
	ImportExpiryGraceMinutes  int                                    `json:"import_expiry_grace_minutes"`
	CheckSeconds              int                                    `json:"check_seconds"`
	CooldownSeconds           int                                    `json:"cooldown_seconds"`
	DailyMaxCount             int                                    `json:"daily_max_count"`
	DailyMaxPoints            float64                                `json:"daily_max_points"`
	MaxUnitPrice              float64                                `json:"max_unit_price"`
	ImportConcurrency         int                                    `json:"import_concurrency"`
	PlanImportConcurrency     map[string]int                         `json:"plan_import_concurrency,omitempty"`
	WindowEnabled             bool                                   `json:"window_enabled"`
	WindowStart               string                                 `json:"window_start"`
	WindowEnd                 string                                 `json:"window_end"`
	Delivery                  AccountRefillDeliveryConfig            `json:"delivery"`
}

type AccountRefillDeliveryConfig struct {
	SameAccountRetryCount            *int              `json:"same_account_retry_count,omitempty"`
	PrivacyEnabled                   *bool             `json:"privacy_enabled,omitempty"`
	OpenAIResponsesFlattenNamespaces *bool             `json:"openai_responses_flatten_namespaces,omitempty"`
	WSMode                           string            `json:"ws_mode"`
	ProxyMode                        string            `json:"proxy_mode"`
	ProxyID                          *int64            `json:"proxy_id"`
	NameTemplate                     string            `json:"name_template"`
	NotesTemplate                    string            `json:"notes_template"`
	Priority                         int               `json:"priority"`
	LoadFactor                       int               `json:"load_factor"`
	RateMultiplier                   float64           `json:"rate_multiplier"`
	AutoPauseOnExpired               bool              `json:"auto_pause_on_expired"`
	Status                           string            `json:"status"`
	Schedulable                      bool              `json:"schedulable"`
	FingerprintMode                  string            `json:"fingerprint_mode"`
	ExpiryMode                       string            `json:"expiry_mode"`
	FixedExpiresAt                   *int64            `json:"fixed_expires_at"`
	HealthDurationMinutes            int               `json:"health_duration_minutes,omitempty"`
	ModelMapping                     map[string]string `json:"model_mapping"`
}

func DefaultAccountRefillConfig() AccountRefillConfig {
	timeoutEnabled, timeoutMinutes := true, defaultRefillTimeoutRetryMinutes
	return AccountRefillConfig{PrimaryProvider: "1208", TimeoutRetryEnabled: &timeoutEnabled, TimeoutRetryMinutes: &timeoutMinutes, Notifications: AccountRefillNotificationConfig{LowBalanceThreshold: 100}, MonitorEnabled: true, Mode: "capacity", DeliveryGroupIDs: []int64{}, PushEnabled: true, MaxInFlight: 1, IntervalMinutes: 60, IntervalQuantity: 1,
		Plan: "pro5x", MinReady: 1, MaxReady: 20, LeadMinutes: 30,
		MinRemaining5h: 15, MinRemaining7d: 15, TargetConcurrency: 85, ConcurrencySustainSeconds: 30, ImportExpiryGraceMinutes: 60, CheckSeconds: 60,
		CooldownSeconds: 300, DailyMaxCount: 50, DailyMaxPoints: 3000, MaxUnitPrice: 60,
		ImportConcurrency: 60, WindowStart: "09:00", WindowEnd: "23:00",
		Delivery: AccountRefillDeliveryConfig{WSMode: "http_bridge", ProxyMode: "direct", AutoPauseOnExpired: true, Priority: 1, RateMultiplier: 1, Status: StatusActive, Schedulable: true, FingerprintMode: "full", ExpiryMode: "rental", ModelMapping: map[string]string{}}}
}

func (c AccountRefillConfig) Validate() error {
	if c.PrimaryProvider != "" && c.PrimaryProvider != "1208" && c.PrimaryProvider != refillProviderMuze && c.PrimaryProvider != refillProvider558669 {
		return errors.New("无效的主号商")
	}
	if err := c.MuzeFallback.validate(); err != nil {
		return err
	}
	if err := c.validate558669(); err != nil {
		return err
	}
	if err := validateRefillDailySchedules(c); err != nil {
		return err
	}
	if c.TimeoutRetryMinutes != nil && (*c.TimeoutRetryMinutes < 1 || *c.TimeoutRetryMinutes > 1440) {
		return errors.New("补号超时重试时间须为 1–1440 分钟")
	}
	for plan, concurrency := range c.PlanImportConcurrency {
		if (plan != "pro5x" && plan != "pro20x") || concurrency < 1 || concurrency > 10000 {
			return errors.New("套餐账号并发仅支持 Pro5x 和 Pro20x，须为 1–10000")
		}
	}
	if err := validateRefillPlanQuotas(c); err != nil {
		return err
	}
	if err := validateRefillRotation(c); err != nil {
		return err
	}
	if err := validateRefillQQConfig(c.Notifications); err != nil {
		return err
	}
	if err := c.Notifications.MuzeRemaining.validate(); err != nil {
		return err
	}
	if math.IsNaN(c.Notifications.LowBalanceThreshold) || math.IsInf(c.Notifications.LowBalanceThreshold, 0) || c.Notifications.LowBalanceThreshold < 0 || c.Notifications.LowBalanceThreshold > 1e7 {
		return errors.New("余额预警阈值须为 0–10000000 积分")
	}
	if c.GroupID <= 0 || !slices.Contains([]string{"pro5x", "pro20x"}, c.Plan) ||
		!slices.Contains([]string{"capacity", "fixed", "interval"}, c.Mode) || !slices.Contains(c.DeliveryGroupIDs, c.GroupID) || len(c.DeliveryGroupIDs) > 100 ||
		c.MaxInFlight < 1 || c.MaxInFlight > 20 || c.IntervalMinutes < 1 || c.IntervalMinutes > 10080 || c.IntervalQuantity < 1 || c.IntervalQuantity > 100 ||
		c.MinReady < 1 || c.MaxReady < c.MinReady || c.MaxReady > 100 ||
		c.LeadMinutes < 0 || c.LeadMinutes > 1440 || c.CheckSeconds < accountRefillMinIntervalSeconds || c.CheckSeconds > 3600 ||
		c.CooldownSeconds < accountRefillMinIntervalSeconds || c.CooldownSeconds > 86400 || c.TargetConcurrency < 1 || c.TargetConcurrency > 100 ||
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
	if d.SameAccountRetryCount != nil && (*d.SameAccountRetryCount < 0 || *d.SameAccountRetryCount > maxPoolModeRetryCount) {
		return errors.New("同账号重试次数须为 0–10")
	}
	if !slices.Contains([]string{"off", "ctx_pool", "http_bridge", "passthrough"}, d.WSMode) ||
		!slices.Contains([]codexFingerprintMode{codexFingerprintOff, codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull}, codexFingerprintMode(d.FingerprintMode)) ||
		!slices.Contains([]string{StatusActive, "inactive"}, d.Status) || d.Priority < 0 || d.LoadFactor < 0 || d.LoadFactor > 10000 ||
		math.IsNaN(d.RateMultiplier) || math.IsInf(d.RateMultiplier, 0) || d.RateMultiplier < 0 || d.RateMultiplier > 1000 || len(d.NameTemplate) > 256 || len(d.NotesTemplate) > 2000 || len(d.ModelMapping) > 200 {
		return errors.New("账号推送参数无效")
	}
	if !slices.Contains([]string{"direct", "specified", "random", "ovh_ipv6", "ovh_ipv6_backup", "ovh_ipv6_next"}, d.ProxyMode) {
		return errors.New("代理模式无效")
	}
	if _, err := NormalizeAccountProxyMode(d.ProxyMode, d.ProxyID, false); err != nil {
		return err
	}
	if !slices.Contains([]string{"rental", "fixed", "health_duration"}, d.ExpiryMode) || (d.ExpiryMode == "fixed" && (d.FixedExpiresAt == nil || ((c.Enabled || refillDailyScheduleEnabled(c)) && *d.FixedExpiresAt <= time.Now().Unix()))) {
		return errors.New("指定截止时间必须在未来")
	}
	if d.ExpiryMode == "health_duration" && (d.HealthDurationMinutes < 1 || d.HealthDurationMinutes > 525600 || !d.AutoPauseOnExpired) {
		return errors.New("健康时长须为 1–525600 的整数分钟，且必须开启到期自动暂停")
	}
	for k, v := range d.ModelMapping {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" || len(k) > 256 || len(v) > 256 {
			return errors.New("模型映射须包含有效的来源和目标模型")
		}
	}
	if (c.Enabled || refillDailyScheduleEnabled(c)) && (!d.Schedulable || d.Status != StatusActive) {
		return errors.New("自动补号须将新账号设为正常且允许调度，避免持续补入不可用账号")
	}
	return nil
}

func (d AccountRefillDeliveryConfig) sameAccountRetryCount() int {
	if d.SameAccountRetryCount == nil {
		return defaultPoolModeRetryCount
	}
	return *d.SameAccountRetryCount
}

func (c AccountRefillConfig) importConcurrencyForPlan(plan string) int {
	if concurrency, ok := c.PlanImportConcurrency[plan]; ok {
		return concurrency
	}
	return c.ImportConcurrency
}

type AccountRefillJob struct {
	EntitlementStatus      string                      `json:"entitlement_status,omitempty"`
	DeliveryVersion        int64                       `json:"delivery_version,omitempty"`
	AppliedDeliveryVersion int64                       `json:"applied_delivery_version,omitempty"`
	BillingStatus          string                      `json:"billing_status,omitempty"`
	SupplierEmail          string                      `json:"supplier_email,omitempty"`
	CredentialsSyncStopped bool                        `json:"credentials_sync_stopped,omitempty"`
	MuzeFallbackJobID      string                      `json:"muze_fallback_job_id,omitempty"`
	SupplyFallback         bool                        `json:"supply_fallback,omitempty"`
	FallbackForJobID       string                      `json:"fallback_for_job_id,omitempty"`
	Provider               string                      `json:"provider,omitempty"`
	MuzeProductID          string                      `json:"muze_product_id,omitempty"`
	MuzeStudio             bool                        `json:"muze_studio,omitempty"`
	MuzeConnectionID       string                      `json:"muze_connection_id,omitempty"`
	SubmissionStartedAt    *time.Time                  `json:"submission_started_at,omitempty"`
	TimeoutIgnoredAt       *time.Time                  `json:"timeout_ignored_at,omitempty"`
	TimeoutNotificationAt  *time.Time                  `json:"timeout_notification_at,omitempty"`
	DailyBatchID           string                      `json:"daily_batch_id,omitempty"`
	Force                  bool                        `json:"force,omitempty"`
	TimeoutRetryOf         string                      `json:"timeout_retry_of,omitempty"`
	RotationGeneration     int64                       `json:"rotation_generation,omitempty"`
	ExpiryTarget           *AccountRefillExpiryTarget  `json:"expiry_target,omitempty"`
	Mode                   string                      `json:"mode,omitempty"`
	ReplacementPending     bool                        `json:"replacement_pending,omitempty"`
	ReplacesJobID          string                      `json:"replaces_job_id,omitempty"`
	ReplacementJobID       string                      `json:"replacement_job_id,omitempty"`
	StartedNotificationAt  *time.Time                  `json:"started_notification_at,omitempty"`
	FailedNotificationAt   *time.Time                  `json:"failed_notification_at,omitempty"`
	ImportedNotificationAt *time.Time                  `json:"imported_notification_at,omitempty"`
	DelayedNotificationAt  *time.Time                  `json:"delayed_notification_at,omitempty"`
	ManualRequestID        string                      `json:"manual_request_id,omitempty"`
	ConfigRevision         int64                       `json:"config_revision,omitempty"`
	ExpiryGraceMinutes     *int                        `json:"expiry_grace_minutes,omitempty"`
	ID                     string                      `json:"id"`
	SupplierID             string                      `json:"supplier_id,omitempty"`
	Status                 string                      `json:"status"`
	SupplierStatus         string                      `json:"supplier_status,omitempty"`
	SupplierRefundPhase    string                      `json:"supplier_refund_phase,omitempty"`
	SupplierError          string                      `json:"supplier_error,omitempty"`
	Reason                 string                      `json:"reason"`
	ReasonDetail           string                      `json:"reason_detail,omitempty"`
	Plan                   string                      `json:"plan"`
	Price                  float64                     `json:"price"`
	PriceVersion           int64                       `json:"price_version"`
	GroupID                int64                       `json:"group_id"`
	Concurrency            int                         `json:"concurrency"`
	DeliveryGroupIDs       []int64                     `json:"delivery_group_ids"`
	DeliveryConfig         AccountRefillDeliveryConfig `json:"delivery_config"`
	BudgetReleased         bool                        `json:"budget_released"`
	AccountID              int64                       `json:"account_id,omitempty"`
	CreatedAt              time.Time                   `json:"created_at"`
	UpdatedAt              time.Time                   `json:"updated_at"`
	FinishedAt             *time.Time                  `json:"finished_at,omitempty"`
	ExpiresAt              *int64                      `json:"expires_at,omitempty"`
	SupplierExpiresAt      *int64                      `json:"supplier_expires_at,omitempty"`
	ResultReady            bool                        `json:"result_ready"`
	Error                  string                      `json:"error,omitempty"`
	Attempts               int                         `json:"attempts"`
	NextAttemptAt          time.Time                   `json:"next_attempt_at"`
}

func (j AccountRefillJob) pending() bool {
	return !slices.Contains([]string{"imported", "failed", "delivery_expired", "dismissed"}, j.Status)
}

type AccountRefillAccount struct {
	Plan            string     `json:"plan"`
	MinRemaining5h  float64    `json:"min_remaining_5h"`
	MinRemaining7d  float64    `json:"min_remaining_7d"`
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
	ForecastLoss    bool       `json:"forecast_loss"`
}

type AccountRefillEvaluation struct {
	TimeoutRetryJobID    string `json:"timeout_retry_job_id,omitempty"`
	TimeoutRetryProvider string `json:"timeout_retry_provider,omitempty"`
	TimeoutRetryPlan     string `json:"timeout_retry_plan,omitempty"`
	waitingForReview     bool
	ExpiryTargets        []AccountRefillExpiryTarget `json:"expiry_targets,omitempty"`
	Error                string                      `json:"error,omitempty"`
	CheckedAt            time.Time                   `json:"checked_at"`
	Ready                int                         `json:"ready"`
	Healthy              int                         `json:"healthy"`
	Expiring             int                         `json:"expiring"`
	Pending              int                         `json:"pending"`
	TimedOutPending      int                         `json:"timed_out_pending"`
	InUse                int                         `json:"in_use"`
	Capacity             int                         `json:"capacity"`
	PendingCapacity      int                         `json:"pending_capacity"`
	ForecastAccounts     int                         `json:"forecast_accounts"`
	ForecastCapacity     int                         `json:"forecast_capacity"`
	ForecastReady        int                         `json:"forecast_ready"`
	Needed               int                         `json:"needed"`
	Reason               string                      `json:"reason"`
	Accounts             []AccountRefillAccount      `json:"accounts"`
}

type accountRefillState struct {
	Supplier558669        refill558669State                           `json:"supplier_558669,omitempty"`
	MuzeFallback          accountRefillMuzeFallbackState              `json:"muze_fallback"`
	WorkdayCalendar       *refillWorkdayCalendar                      `json:"workday_calendar,omitempty"`
	Muze                  muzeRefillState                             `json:"muze"`
	DailyScheduleStatus   AccountRefillDailyScheduleStatus            `json:"daily_schedule_status"`
	DailyScheduleStatuses map[string]AccountRefillDailyScheduleStatus `json:"daily_schedule_statuses,omitempty"`
	RotationGeneration    int64                                       `json:"rotation_generation,omitempty"`
	ExpiryDemands         []accountRefillExpiryDemand                 `json:"expiry_demands,omitempty"`
	ExpiryCheckedAt       time.Time                                   `json:"expiry_checked_at,omitempty"`
	RotationCursor        int                                         `json:"rotation_cursor"`
	EncryptedWebhook      string                                      `json:"encrypted_webhook,omitempty"`
	EncryptedQQSecret     string                                      `json:"encrypted_qq_secret,omitempty"`
	LastNotificationTest  time.Time                                   `json:"last_notification_test,omitempty"`
	NotificationStatus    AccountRefillNotificationStatus             `json:"notification_status"`
	MuzeRemainingAlerted  map[string]bool                             `json:"muze_remaining_alerted,omitempty"`
	LowBalanceAlerted     bool                                        `json:"low_balance_alerted"`
	RefillNeededAlerted   bool                                        `json:"refill_needed_alerted"`
	RefillNeededRetryAt   time.Time                                   `json:"refill_needed_retry_at"`
	NextNeededCheckAt     time.Time                                   `json:"next_needed_check_at"`
	NextNeededIntervalAt  time.Time                                   `json:"next_needed_interval_at"`
	LowBalanceRequired    float64                                     `json:"low_balance_required"`
	NextBalanceCheckAt    time.Time                                   `json:"next_balance_check_at"`
	LeaseSyncAt           *time.Time                                  `json:"lease_sync_at,omitempty"`
	LeaseSyncMessage      string                                      `json:"lease_sync_message"`
	LeaseSyncCursor       int                                         `json:"lease_sync_cursor"`
	Config                AccountRefillConfig                         `json:"config"`
	EncryptedToken        string                                      `json:"encrypted_token,omitempty"`
	Revision              int64                                       `json:"revision"`
	Jobs                  []AccountRefillJob                          `json:"jobs"`
	Catalog               *RefillCatalog                              `json:"catalog,omitempty"`
	CatalogAt             *time.Time                                  `json:"catalog_at,omitempty"`
	Evaluation            AccountRefillEvaluation                     `json:"evaluation"`
	NextCheckAt           time.Time                                   `json:"next_check_at"`
	TimeoutCheckedAt      time.Time                                   `json:"timeout_checked_at"`
	TimeoutNoticeKey      string                                      `json:"timeout_notice_key,omitempty"`
	TimeoutNoticeContent  string                                      `json:"timeout_notice_content,omitempty"`
	TimeoutNoticeRetryAt  time.Time                                   `json:"timeout_notice_retry_at"`
	LastSubmitAt          time.Time                                   `json:"last_submit_at"`
	DemandSince           *time.Time                                  `json:"demand_since,omitempty"`
	Usage                 map[string]AccountRefillDailyUsage          `json:"usage"`
	NextIntervalAt        time.Time                                   `json:"next_interval_at"`
	IntervalRemaining     int                                         `json:"interval_remaining"`
	Wallet                *RefillWallet                               `json:"wallet,omitempty"`
	Key                   *RefillKeyInfo                              `json:"key,omitempty"`
	IdentityAt            *time.Time                                  `json:"identity_at,omitempty"`
	Message               string                                      `json:"message"`
	ProviderRetryAt       time.Time                                   `json:"provider_retry_at"`
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
	if strings.Contains(raw, `"fendudu`) {
		if err := migrateRetiredRefillProvider(raw, &state); err != nil {
			return state, err
		}
	}
	if state.Usage == nil {
		state.Usage = map[string]AccountRefillDailyUsage{}
	}
	return state, nil
}

// ImportedAccountExpiry stores the actual local deadline and a separate source
// lease. Reprocessing marked input never accumulates another extension.
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
	if extra[AccountNativeExpiryExtraKey] == true {
		a := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: extra}
		recalculateNativeExpiry(a)
		if a.ExpiresAt != nil && a.ExpiresAt.Unix() == original {
			original = expiryTimestamp(extra[AccountExpiryBaseExtraKey])
		}
	}
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
	copyExtra[AccountNativeExpiryExtraKey] = true
	copyExtra[AccountExpiryBaseExtraKey] = original
	copyExtra[AccountExpiryManualExtraKey] = false
	deadline := original + int64(minutes*60)
	return &deadline, copyExtra
}

var refillTimezone = beijingLocation

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

func evaluateAccountRefill(c AccountRefillConfig, accounts []Account, loads map[int64]*AccountLoadInfo, jobs []AccountRefillJob, now time.Time, plannedPlans ...string) AccountRefillEvaluation {
	e := AccountRefillEvaluation{CheckedAt: now, Accounts: []AccountRefillAccount{}}
	for _, job := range jobs {
		if refillJobTimedOut(c, job, now) {
			e.TimedOutPending++
			continue
		}
		if job.pending() {
			e.Pending++
			concurrency := job.Concurrency
			if concurrency <= 0 {
				concurrency = c.ImportConcurrency // Legacy reservations had no capacity snapshot.
			}
			e.PendingCapacity += refillMax(0, concurrency)
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
		if origin == "supplier_pending" {
			row.SourceExpiresAt = nil
		}
		row.Plan = refillAccountPlan(a, jobs)
		threshold := refillQuotaThreshold(c, row.Plan)
		row.MinRemaining5h, row.MinRemaining7d = threshold.MinRemaining5h, threshold.MinRemaining7d
		if load := loads[a.ID]; load != nil {
			row.InUse = refillMax(0, load.CurrentConcurrency)
		}
		quotaLow, quotaAtFloor := false, false
		for _, w := range []struct {
			name        string
			minimum     float64
			destination **float64
		}{{"5h", threshold.MinRemaining5h, &row.Remaining5h}, {"7d", threshold.MinRemaining7d, &row.Remaining7d}} {
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
				if w.minimum > 0 && remaining <= w.minimum {
					quotaAtFloor = true
				}
			}
		}
		switch {
		case row.SourceExpiresAt != nil && !row.SourceExpiresAt.After(now):
			row.State = "expired"
		case !a.IsActive() || !a.Schedulable || a.isSchedulingExpired(now) || (a.OverloadUntil != nil && now.Before(*a.OverloadUntil)) || (a.RateLimitResetAt != nil && now.Before(*a.RateLimitResetAt)) || (a.TempUnschedulableUntil != nil && now.Before(*a.TempUnschedulableUntil)):
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
		// A refill floor does not remove an account from live scheduling. Keep
		// its capacity here, and all in-flight demand even on unavailable rows.
		e.InUse += row.InUse
		if row.State == "ready" || row.State == "expiring" || row.State == "quota_low" {
			e.Capacity += refillMax(0, a.Concurrency)
			if refillForecastEnabled(c) {
				row.ForecastLoss = a.Concurrency > 0 && (quotaAtFloor || row.State == "expiring")
				if row.ForecastLoss {
					e.ForecastAccounts++
				} else {
					e.ForecastCapacity += refillMax(0, a.Concurrency)
				}
			}
		}
		if refillForecastEnabled(c) && row.State == "ready" && !row.ForecastLoss {
			e.ForecastReady++
		}
		e.Accounts = append(e.Accounts, row)
	}
	minimumGap := refillMax(0, c.MinReady-e.Ready-e.Pending)
	capacityGap, forecastGap := 0, 0
	if c.Mode == "capacity" && refillCapacityHigh(c, e) {
		capacityGap = refillCapacityGap(c, e.InUse, e.Capacity+e.PendingCapacity, plannedPlans...)
	}
	if c.Mode == "capacity" && refillForecastHigh(c, e) {
		forecastGap = refillCapacityGap(c, e.InUse, e.ForecastCapacity+e.PendingCapacity, plannedPlans...)
	}
	// Each reason owns its gap and observation gate. A larger unobserved
	// forecast must not borrow minimum-reserve or live-concurrency authorization.
	if minimumGap > 0 {
		e.Needed = minimumGap
		e.Reason = "健康储备不足"
		if e.Expiring > 0 {
			e.Reason = "账号临期，提前补充储备"
		}
	} else if capacityGap > 0 {
		e.Needed = capacityGap
		e.Reason = "并发利用率超过目标"
	} else if forecastGap > 0 {
		e.Needed = forecastGap
		e.Reason = refillForecastReason
	} else {
		e.Reason = "储备充足"
	}
	limitAccountRefillEvaluation(c, &e, jobs)
	return e
}

// Apply hard limits without treating expiry receipts as additional demand.
func limitAccountRefillEvaluation(c AccountRefillConfig, e *AccountRefillEvaluation, jobs []AccountRefillJob) {
	for _, j := range jobs {
		if refillNeedsAttention(j.Status) && !refillJobTimedOut(c, j, e.CheckedAt) {
			e.waitingForReview = true
			e.Needed = 0
			e.Reason = "原任务需要处理，暂停新增补号"
		}
	}
	if e.Pending >= c.MaxInFlight {
		e.Needed = 0
		e.Reason = "在途任务已抵扣缺口，等待交付"
	}
	limitReady := e.Ready
	if c.Mode == "capacity" && refillForecastEnabled(c) {
		// Replacements may overlap with accounts at the quota floor, as with
		// existing expiring-account replacements, without exceeding future stock.
		limitReady = e.ForecastReady
	}
	e.Needed = refillMin(e.Needed, refillMax(0, c.MaxInFlight-e.Pending), refillMax(0, c.MaxReady-limitReady-e.Pending))
	if limitReady+e.Pending >= c.MaxReady {
		e.Needed = 0
		e.Reason = "已达健康账号上限"
	}
	if c.Mode == "interval" {
		e.Needed = 0
		e.Reason = "等待下一轮定时补号"
	}
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
