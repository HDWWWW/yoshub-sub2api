package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type accountRefillAccounts interface {
	ListByGroup(context.Context, int64) ([]Account, error)
	GetByIDs(context.Context, []int64) ([]*Account, error)
	FindByExtraField(context.Context, string, any) ([]Account, error)
	BindGroups(context.Context, int64, []int64) error
	SetSchedulable(context.Context, int64, bool) error
}
type accountRefillAdmin interface {
	CreateAccount(context.Context, *CreateAccountInput) (*Account, error)
	UpdateAccount(context.Context, int64, *UpdateAccountInput) (*Account, error)
}
type accountRefillProxies interface {
	ListActive(context.Context) ([]Proxy, error)
}
type accountRefillGroups interface {
	GetByID(context.Context, int64) (*Group, error)
}
type accountRefillLoads interface {
	GetAccountsLoadBatch(context.Context, []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error)
}

type AccountRefillService struct {
	calendarWake  chan struct{}
	calendarFetch func(context.Context, int) (refillWorkdayYear, error)
	rulesMu       sync.Mutex
	ruleRoot      *AccountRefillService
	ruleGroupID   int64
	ruleServices  map[int64]*AccountRefillService
	notifier      refillNotifier
	qqNotifier    *qqRefillNotifier
	capacityMu    sync.Mutex
	capacity      refillCapacityTracker
	store         AccountRefillStore
	encryptor     SecretEncryptor
	accounts      accountRefillAccounts
	admin         accountRefillAdmin
	groups        accountRefillGroups
	loads         accountRefillLoads
	proxies       accountRefillProxies
	provider      RefillProvider
	muze          muzeProvider
	stop          chan struct{}
	cancel        context.CancelFunc
	once          sync.Once
	wg            sync.WaitGroup
}

func NewAccountRefillService(store AccountRefillStore, encryptor SecretEncryptor, accounts AccountRepository, admin AdminService, groups GroupRepository, loads *ConcurrencyService, proxies ProxyRepository) *AccountRefillService {
	return &AccountRefillService{store: store, encryptor: encryptor, accounts: accounts, admin: admin, groups: groups, loads: loads, proxies: proxies, provider: newHTTPRefillProvider(), muze: newHTTPMuzeProvider(), stop: make(chan struct{})}
}
func (s *AccountRefillService) Start() {
	ctx, stop := context.WithCancel(context.Background())
	s.cancel = stop
	s.calendarWake = make(chan struct{}, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runRefillCalendarSync(ctx)
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(refillCapacitySampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sampleCtx, cancel := context.WithTimeout(ctx, refillCapacityMaxSampleGap)
				s.sampleAllCapacity(sampleCtx)
				cancel()
			}
		}
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// Wake often enough to honor the shortest configurable check/cooldown.
		// Per-rule deadlines and supplier polling/backoff still gate each action.
		ticker := time.NewTicker(accountRefillMinIntervalSeconds * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				err := s.runAllRules(ctx)
				if err != nil && !errors.Is(err, ErrAccountRefillBusy) {
					slog.Warn("account_refill_tick_failed", "error", "补号检查未完成，下轮重试")
				}
			}
		}
	}()
}
func (s *AccountRefillService) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
		if s.cancel != nil {
			s.cancel()
		}
	})
	s.wg.Wait()
}

type AccountRefillView struct {
	MuzeFallbackStatus        AccountRefillMuzeFallbackStatus             `json:"muze_fallback_status"`
	WorkdayCalendar           *AccountRefillCalendarView                  `json:"workday_calendar,omitempty"`
	DailyScheduleStatuses     map[string]AccountRefillDailyScheduleStatus `json:"daily_schedule_statuses,omitempty"`
	Muze                      MuzeRefillView                              `json:"muze"`
	DailyScheduleStatus       AccountRefillDailyScheduleStatus            `json:"daily_schedule_status"`
	Rules                     []AccountRefillRuleSummary                  `json:"rules"`
	RotationCursor            int                                         `json:"rotation_cursor"`
	NextPlan                  string                                      `json:"next_plan"`
	WebhookConfigured         bool                                        `json:"webhook_configured"`
	QQSecretConfigured        bool                                        `json:"qq_secret_configured"`
	NotificationStatus        AccountRefillNotificationStatus             `json:"notification_status"`
	LeaseSyncAt               *time.Time                                  `json:"lease_sync_at"`
	LeaseSyncMessage          string                                      `json:"lease_sync_message"`
	CapacityObservation       AccountRefillCapacityObservation            `json:"capacity_observation"`
	ForecastObservation       AccountRefillCapacityObservation            `json:"forecast_observation"`
	Config                    AccountRefillConfig                         `json:"config"`
	Revision                  int64                                       `json:"revision"`
	TokenConfigured           bool                                        `json:"token_configured"`
	Key                       *RefillKeyInfo                              `json:"key"`
	Wallet                    *RefillWallet                               `json:"wallet"`
	Catalog                   *RefillCatalog                              `json:"catalog"`
	CatalogAt                 *time.Time                                  `json:"catalog_at"`
	IdentityAt                *time.Time                                  `json:"identity_at"`
	Jobs                      []AccountRefillJob                          `json:"jobs"`
	Evaluation                AccountRefillEvaluation                     `json:"evaluation"`
	Daily                     AccountRefillDailyUsage                     `json:"daily"`
	NextCheckAt               time.Time                                   `json:"next_check_at"`
	NextIntervalAt            time.Time                                   `json:"next_interval_at"`
	Message                   string                                      `json:"message"`
	ImportExpiryOffsetMinutes int                                         `json:"import_expiry_offset_minutes"`
}

func (s *AccountRefillService) View(ctx context.Context) (*AccountRefillView, error) {
	raw, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.decodeRefillWithMuze(ctx, raw)
	if err != nil {
		return nil, err
	}
	// GET is independent of the provider worker/advisory lock. Always evaluate
	// current local accounts and Redis loads, never its persisted old snapshot.
	if state.Config.GroupID > 0 {
		localCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := s.evaluate(localCtx, &state, time.Now())
		cancel()
		if err != nil {
			state.Evaluation = AccountRefillEvaluation{CheckedAt: time.Now(), Accounts: []AccountRefillAccount{}, Error: err.Error(), Reason: err.Error()}
		} else {
			reconcileRefillFailureReplacements(&state)
		}
	}
	day := time.Now().In(refillTimezone).Format("2006-01-02")
	daily := state.Usage[day]
	for _, j := range state.Jobs {
		if j.CreatedAt.In(refillTimezone).Format("2006-01-02") == day && j.pending() && j.SupplierStatus != "succeeded" {
			daily.ReservedPoints += j.Price
		}
	}
	daily.SpentPoints = refillMax(0, daily.Points-daily.ReservedPoints)
	rules, err := s.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	return &AccountRefillView{DailyScheduleStatuses: state.DailyScheduleStatuses, Muze: state.Muze.view(), DailyScheduleStatus: state.DailyScheduleStatus, WebhookConfigured: state.EncryptedWebhook != "", QQSecretConfigured: state.EncryptedQQSecret != "", NotificationStatus: state.NotificationStatus, Config: state.Config, Revision: state.Revision, TokenConfigured: state.EncryptedToken != "", Key: state.Key, Wallet: state.Wallet, LeaseSyncAt: state.LeaseSyncAt, LeaseSyncMessage: state.LeaseSyncMessage,
		Catalog: state.Catalog, CatalogAt: state.CatalogAt, IdentityAt: state.IdentityAt, Jobs: refillJobsForDisplay(state.Jobs), Evaluation: state.Evaluation,
		Daily: daily, NextCheckAt: state.NextCheckAt, NextIntervalAt: state.NextIntervalAt, Message: state.Message, ImportExpiryOffsetMinutes: state.Config.ImportExpiryGraceMinutes,
		Rules: rules, RotationCursor: state.RotationCursor, NextPlan: nextRefillPurchasePlan(&state), WorkdayCalendar: state.WorkdayCalendar.view(),
		MuzeFallbackStatus:  refillMuzeFallbackStatus(&state),
		CapacityObservation: s.capacityObservation(state.Config, state.Revision, time.Now()),
		ForecastObservation: s.forecastObservation(state.Config, state.Revision, state.Evaluation, time.Now())}, nil
}

func saveRefillState(state *accountRefillState, save func(string) error) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return save(string(raw))
}
func (s *AccountRefillService) validateGroups(ctx context.Context, c AccountRefillConfig) error {
	for _, id := range c.DeliveryGroupIDs {
		g, err := s.groups.GetByID(ctx, id)
		if err != nil || g == nil || g.Platform != PlatformOpenAI || g.Status != StatusActive {
			return errors.New("请选择有效的 OpenAI 分组，投递分组必须包含监测分组")
		}
	}
	return nil
}
func (s *AccountRefillService) Configure(ctx context.Context, c AccountRefillConfig, token string, revision int64, webhooks ...string) error {
	secrets := AccountRefillNotificationSecrets{}
	if len(webhooks) > 0 {
		secrets.WeComWebhook = webhooks[0]
	}
	return s.ConfigureWithNotificationSecrets(ctx, c, token, revision, secrets)
}

func (s *AccountRefillService) ConfigureWithNotificationSecrets(ctx context.Context, c AccountRefillConfig, token string, revision int64, secrets AccountRefillNotificationSecrets) error {
	defer s.wakeRefillCalendar()
	if s.ruleGroupID > 0 && c.GroupID != s.ruleGroupID {
		return errors.New("规则与当前分组不匹配，请重新选择分组")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	webhook := strings.TrimSpace(secrets.WeComWebhook)
	qqSecret := strings.TrimSpace(secrets.QQClientSecret)
	if qqSecret != "" {
		if err := validateRefillQQSecret(qqSecret); err != nil {
			return err
		}
	}
	if webhook != "" {
		if err := validateRefillWebhook(webhook); err != nil {
			return err
		}
	}
	if c.Enabled || refillDailyScheduleEnabled(c) {
		if err := s.validateGroups(ctx, c); err != nil {
			return err
		}
		if _, err := s.selectProxy(ctx, c.Delivery); err != nil {
			return err
		}
	}
	token = strings.TrimSpace(token)
	if token != "" && (!strings.HasPrefix(token, "rst_") || len(token) > 256 || strings.ContainsAny(token, "\r\n\t ")) {
		return errors.New("号商 API Key 格式无效")
	}
	return withAccountRefillConfigLock(ctx, s.store, 30*time.Second, func(raw string, save func(string) error) error {
		state, err := s.decodeRefillWithMuze(ctx, raw)
		if err != nil {
			return err
		}
		if revision != state.Revision {
			return errors.New("配置已被其他页面修改，请刷新后再保存")
		}
		if c.PrimaryProvider == "" {
			c.PrimaryProvider = state.Config.PrimaryProvider
		}
		if c.DailySchedules == nil && state.Config.DailySchedules != nil {
			// A stale client cannot silently erase a multi-task schedule.
			if c.DailySchedule != nil {
				return errors.New("定时补号已升级为多任务，请刷新页面后修改")
			}
			c.DailySchedules = state.Config.DailySchedules
		}
		if c.DailySchedules != nil {
			c.DailySchedule = nil
		} else if c.DailySchedule == nil {
			c.DailySchedule = state.Config.DailySchedule
		}
		if err := c.Validate(); err != nil {
			return err
		}
		if state.Config.GroupID > 0 && c.GroupID != state.Config.GroupID {
			return errors.New("请通过分组选择器切换规则，不能覆盖其他分组的规则")
		}
		if stores, ok := s.rootService().store.(AccountRefillGroupStore); ok {
			rows, err := stores.ListRules(ctx)
			if err != nil {
				return err
			}
			if s.ruleGroupID > 0 && rows[s.ruleGroupID] == "" && len(rows) >= 100 {
				return errors.New("最多支持 100 个分组规则")
			}
			if s.ruleGroupID == 0 && state.Config.GroupID == 0 && rows[c.GroupID] != "" {
				return errors.New("该分组已有规则，请通过分组选择器加载")
			}
		}
		if webhook != "" {
			cipher, err := s.encryptor.Encrypt(webhook)
			if err != nil {
				return errors.New("机器人地址加密失败")
			}
			state.EncryptedWebhook = cipher
		}
		if qqSecret != "" {
			cipher, err := s.encryptor.Encrypt(qqSecret)
			if err != nil {
				return errors.New("QQ 密钥加密失败")
			}
			state.EncryptedQQSecret = cipher
		} else if c.Notifications.QQAppID != state.Config.Notifications.QQAppID {
			state.EncryptedQQSecret = ""
		}
		if c.Notifications.Channel == "qq" && state.EncryptedQQSecret == "" {
			return errors.New("请先填写 QQ ClientSecret，更换 AppID 时须重新填写")
		}
		if c.Notifications.Channel != "qq" && (c.Notifications.RefillStarted || c.Notifications.RefillNeeded || c.Notifications.LowBalance) && state.EncryptedWebhook == "" {
			return errors.New("启用通知前请配置企业微信群机器人 Webhook")
		}
		if webhook != "" || qqSecret != "" || c.Notifications != state.Config.Notifications {
			state.LowBalanceAlerted = false
			state.LowBalanceRequired = 0
			state.NextBalanceCheckAt = time.Time{}
		}
		if c.Enabled != state.Config.Enabled || c.Notifications != state.Config.Notifications || qqSecret != "" || c.Mode != state.Config.Mode {
			state.RefillNeededAlerted = false
			state.RefillNeededRetryAt = time.Time{}
			state.NextNeededIntervalAt = time.Time{}
		}
		if c.IntervalMinutes != state.Config.IntervalMinutes || c.IntervalQuantity != state.Config.IntervalQuantity {
			state.NextNeededIntervalAt = time.Time{}
		}
		state.NextNeededCheckAt = time.Time{}
		if c.GroupID != state.Config.GroupID {
			for _, j := range state.Jobs {
				if j.pending() {
					return errors.New("请等待原监测分组的在途任务处理完成后再切换分组")
				}
			}
		}
		if token != "" {
			wallet, key, identityErr := s.provider.Identity(ctx, token)
			if identityErr != nil || wallet == nil || wallet.CustomerID == "" {
				return errors.New("无法验证新 API Key，请检查有效期和读取权限")
			}
			hasPending := false
			for _, j := range state.Jobs {
				if j.Provider == "" && j.pending() {
					hasPending = true
				}
			}
			if hasPending && state.EncryptedToken != "" {
				oldID := ""
				if state.Wallet != nil {
					oldID = state.Wallet.CustomerID
				}
				if oldID == "" {
					previous, decodeErr := s.encryptor.Decrypt(state.EncryptedToken)
					if decodeErr != nil {
						return errors.New("原密钥无法读取，请先核对在途订单")
					}
					oldWallet, _, oldErr := s.provider.Identity(ctx, previous)
					if oldErr == nil && oldWallet != nil {
						oldID = oldWallet.CustomerID
					}
				}
				if oldID == "" || oldID != wallet.CustomerID {
					return errors.New("存在在途订单，只能更换同一号商账户的 API Key")
				}
			}
			cipher, err := s.encryptor.Encrypt(token)
			if err != nil {
				return errors.New("密钥加密失败")
			}
			state.EncryptedToken = cipher
			state.Key = key
			state.Wallet = wallet
			identityAt := time.Now()
			state.IdentityAt = &identityAt
			state.Catalog = nil
			state.CatalogAt = nil
		}
		if c.automaticProvider() == refillProviderMuze && (c.Enabled || refillDailyScheduleEnabled(c)) && state.Muze.EncryptedToken == "" {
			return errors.New("请先配置 Muze API Key")
		}
		if c.automaticProvider() == "" && (c.Enabled || refillDailyScheduleEnabled(c)) && state.EncryptedToken == "" {
			return errors.New("启用前请配置号商 API Key")
		}
		if c.Mode != state.Config.Mode || c.IntervalMinutes != state.Config.IntervalMinutes || c.IntervalQuantity != state.Config.IntervalQuantity {
			state.NextIntervalAt = time.Now().Add(time.Duration(c.IntervalMinutes) * time.Minute)
			state.IntervalRemaining = 0
		}
		if c.RotationEnabled != state.Config.RotationEnabled || !slices.Equal(c.Rotation, state.Config.Rotation) {
			state.RotationCursor = 0
			state.RotationGeneration++
		}
		// An older client omitting this optional switch must not re-enable a
		// forecast that an administrator explicitly disabled.
		if c.ForecastEnabled == nil && state.Config.ForecastEnabled != nil {
			enabled := *state.Config.ForecastEnabled
			c.ForecastEnabled = &enabled
		}
		if c.ImmediateFailureRetry == nil && state.Config.ImmediateFailureRetry != nil {
			enabled := *state.Config.ImmediateFailureRetry
			c.ImmediateFailureRetry = &enabled
		}
		if c.TimeoutRetryEnabled == nil {
			c.TimeoutRetryEnabled = state.Config.TimeoutRetryEnabled
		}
		if c.TimeoutRetryMinutes == nil {
			c.TimeoutRetryMinutes = state.Config.TimeoutRetryMinutes
		}
		if c.TimeoutRetryNotify == nil {
			c.TimeoutRetryNotify = state.Config.TimeoutRetryNotify
		}
		if c.MuzeFallback == nil {
			c.MuzeFallback = state.Config.MuzeFallback
		}
		if c.MuzeFallback.enabled() && state.Muze.EncryptedToken == "" {
			return errors.New("启用备用补号前请先保存muze连接设置")
		}
		roleChanged := c.automaticProvider() != state.Config.automaticProvider()
		if roleChanged || !c.MuzeFallback.enabled() {
			state.MuzeFallback.Shortage = nil
		}
		if roleChanged {
			state.RotationGeneration++
		}
		if c.MuzeFallback.enabled() && state.EncryptedToken == "" {
			return errors.New("启用备用补号前请先配置 1208 API Key")
		}
		if c.MuzeFallback.enabled() && (roleChanged || !state.Config.MuzeFallback.enabled() || c.Enabled && !state.Config.Enabled) {
			state.MuzeFallback.Since = time.Now()
		}
		state.MuzeFallback.NextAttemptAt = time.Time{}
		state.MuzeFallback.Message = ""
		if c.Delivery.SameAccountRetryCount == nil && state.Config.Delivery.SameAccountRetryCount != nil {
			count := *state.Config.Delivery.SameAccountRetryCount
			c.Delivery.SameAccountRetryCount = &count
		}
		if c.Delivery.OpenAIResponsesFlattenNamespaces == nil && state.Config.Delivery.OpenAIResponsesFlattenNamespaces != nil {
			enabled := *state.Config.Delivery.OpenAIResponsesFlattenNamespaces
			c.Delivery.OpenAIResponsesFlattenNamespaces = &enabled
		}
		configureRefillDailySchedule(&state, c, time.Now())
		state.Config = c
		reconcileRefillFailureReplacements(&state)
		state.Revision++
		state.NextCheckAt = time.Time{}
		state.ProviderRetryAt = time.Time{}
		state.LeaseSyncAt = nil
		state.DemandSince = nil
		state.Message = "配置已保存"
		return saveRefillState(&state, save)
	})
}

// Check refreshes the saved configuration and provider metadata without placing
// an order. GET never invokes a paid endpoint.
func (s *AccountRefillService) Check(ctx context.Context) error { return s.run(ctx, true) }

func (s *AccountRefillService) evaluate(ctx context.Context, state *accountRefillState, now time.Time) error {
	if state.Config.GroupID <= 0 {
		return errors.New("请先选择监测分组")
	}
	accounts, err := s.accounts.ListByGroup(ctx, state.Config.GroupID)
	if err != nil {
		return errors.New("账号快照读取失败，暂停新增补号")
	}
	// ListByGroup contains only active accounts. Recheck tracked leases by ID
	// so a temporary error cannot erase a replacement or its completion receipt.
	// Deleted accounts, group removals and renewed leases are reconciled below.
	seen := make(map[int64]bool, len(accounts))
	for _, a := range accounts {
		seen[a.ID] = true
	}
	var missing []int64
	for _, demand := range state.ExpiryDemands {
		if !seen[demand.AccountID] {
			missing = append(missing, demand.AccountID)
			seen[demand.AccountID] = true
		}
	}
	if len(missing) > 0 {
		tracked, err := s.accounts.GetByIDs(ctx, missing)
		if err != nil {
			return errors.New("临期账号快照读取失败，暂停新增补号")
		}
		for _, a := range tracked {
			if a != nil {
				accounts = append(accounts, *a)
			}
		}
	}
	loadInput := make([]AccountWithConcurrency, 0, len(accounts))
	for _, a := range accounts {
		if a.IsOpenAIOAuth() && !a.IsShadow() {
			loadInput = append(loadInput, AccountWithConcurrency{ID: a.ID, MaxConcurrency: a.Concurrency})
		}
	}
	loads, err := s.loads.GetAccountsLoadBatch(ctx, loadInput)
	if err != nil {
		return errors.New("并发数据读取失败，暂停新增补号")
	}
	jobs, claims, err := s.evaluationInputs(ctx, state)
	if err != nil {
		return err
	}
	state.Evaluation = evaluateAccountRefill(state.Config, accounts, loads, jobs, now, refillPlannedPurchasePlans(state)...)
	evaluateRefillExpiryDemands(state, jobs, claims, now)
	limitAccountRefillEvaluation(state.Config, &state.Evaluation, jobs)
	applyRefillTimeoutRetry(state, jobs, now)
	return nil
}

func (s *AccountRefillService) run(ctx context.Context, preview bool) error {
	return s.store.WithLock(ctx, func(raw string, save func(string) error) (runErr error) {
		state, err := s.decodeRefillWithMuze(ctx, raw)
		if err != nil {
			return err
		}
		if raw == "" && !preview {
			return nil
		}
		now := time.Now()
		persist := func() error { return saveRefillState(&state, save) }
		var timeoutNotice *refillTimeoutNotice
		defer func() {
			if !preview && runErr == nil {
				// Check delays after advancing deliveries, including early returns
				// for paused automation or provider backoff. A completed import in
				// this tick must not first announce that it is still missing.
				if err := s.notifyRefillDelayed(ctx, &state, time.Now(), persist); err != nil {
					runErr = err
					return
				}
				runErr = s.notifyRefillTimeoutRetry(ctx, &state, timeoutNotice, now, persist)
			}
		}()
		if !preview {
			if markRefillTimeouts(&state, now) {
				if err := persist(); err != nil {
					return err
				}
			}
			if err := s.checkDisabledRefillNotification(ctx, &state, now, persist); err != nil {
				return err
			}
			// Evaluate locally so supplier backoff cannot hide a due timeout.
			if refillNotificationConfigured(&state) && refillTimeoutNotifyEnabled(state.Config) && captureRefillTimeoutNotice(&state, now) != nil {
				if err := s.evaluate(ctx, &state, now); err == nil {
					timeoutNotice = captureRefillTimeoutNotice(&state, now)
				}
			}
			// Daily purchases are independent of normal monitoring, time windows
			// and supplier review. Reserve the entire batch atomically first.
			if reserved, err := s.runRefillDailySchedule(ctx, &state, now, persist); err != nil || reserved {
				return err
			}
		}
		if err := s.refreshMuzeProducts(ctx, &state, now, preview, persist); err != nil {
			return err
		}
		if !preview && state.Muze.EncryptedToken != "" && !now.Before(state.Muze.RetryAt) {
			if err := s.advanceMuzeJobs(ctx, &state, now, persist); err != nil {
				return err
			}
		}
		var token string
		if state.EncryptedToken != "" && !now.Before(state.ProviderRetryAt) {
			token, err = s.encryptor.Decrypt(state.EncryptedToken)
			if err != nil {
				state.Message = "密钥解密失败，请重新保存 API Key"
				if state.Config.automaticProvider() == "" && !state.Config.MuzeFallback.enabled() {
					return persist()
				}
				token = ""
			}
		}
		// Delivered manual orders still need lease updates when monitoring is off.
		// Primary backoff must not postpone a Muze supplier timestamp.
		if token != "" || state.Muze.EncryptedToken != "" {
			if s.syncSourceLeases(ctx, token, &state, now, preview) {
				if err := persist(); err != nil {
					return err
				}
			}
		}
		if !preview && token != "" && !time.Now().Before(state.ProviderRetryAt) {
			// Continue accepted work even when automation or its time window is off.
			if index := nextAccountRefillJob(state.Jobs, state.Config, now); index >= 0 {
				if err := s.advance(ctx, token, &state, &state.Jobs[index], persist); err != nil {
					return err
				}
			}
		}
		// Reconcile the primary delivery first, then evaluate the backup even
		// during primary backoff or a long normal monitoring interval.
		if !preview {
			if reserved, err := s.runMuzeFallback(ctx, &state, time.Now(), persist); err != nil || reserved {
				return err
			}
		}
		primary := state.Config.automaticProvider()
		if time.Now().Before(state.providerBackoff(primary)) {
			if timeoutNotice != nil {
				state.Message = "号商限流或暂不可用，等待退避结束后重试"
			}
			return nil
		}
		if !preview && !time.Now().Before(state.ProviderRetryAt) {
			if err := s.checkRefillNotificationBalance(ctx, token, &state, now, persist); err != nil {
				return err
			}
			if time.Now().Before(state.providerBackoff(primary)) {
				return nil
			}
		}
		if !preview && ((now.Before(state.NextCheckAt) && !refillTimeoutCheckDue(&state, now)) || !state.Config.MonitorEnabled) {
			return nil
		}
		state.NextCheckAt = now.Add(time.Duration(state.Config.CheckSeconds) * time.Second)
		if err := s.evaluate(ctx, &state, now); err != nil {
			state.Message = err.Error()
			return persist()
		}
		if !preview {
			state.TimeoutCheckedAt = now
			timeoutNotice = captureRefillTimeoutNotice(&state, now)
		}
		reconcileRefillFailureReplacements(&state)
		if time.Now().Before(state.providerBackoff(primary)) {
			state.Message = "号商限流，等待重试后继续同步"
			return persist()
		}
		// A read-only refresh also supplies the manual 1208 quote when Muze is primary.
		if primary == "" || (preview && token != "") {
			if token == "" {
				state.Message = "请配置 1208 API Key"
				return persist()
			}
			catalog, err := s.provider.Catalog(ctx, token)
			if err != nil || catalog == nil {
				state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
				if primary == "" {
					state.Message = "无法读取 1208 报价"
					if err != nil {
						state.Message = err.Error()
					}
					return persist()
				}
			} else {
				state.Catalog, state.CatalogAt = catalog, &now
			}
			if err == nil && catalog != nil && (preview || state.IdentityAt == nil || now.Sub(*state.IdentityAt) >= 5*time.Minute) {
				wallet, key, identityErr := s.provider.Identity(ctx, token)
				if wallet != nil {
					state.Wallet = wallet
				}
				if identityErr == nil {
					state.Key = key
					state.IdentityAt = &now
				}
			}
		}
		state.Message = state.Evaluation.Reason
		if preview {
			state.Message = "仅检查，未下单：" + state.Message
			return persist()
		}
		if !state.Config.Enabled {
			state.Message = "自动补号已关闭；" + state.Message
			return persist()
		}
		if !state.Config.PushEnabled {
			state.Message = "推送已暂停，保留在途任务并暂停新增补号"
			return persist()
		}
		timeoutRetryID := state.Evaluation.TimeoutRetryJobID
		timeoutRetry := timeoutRetryID != ""
		if !timeoutRetry && !refillInWindow(state.Config, now) {
			state.Message = "当前不在补号时段，继续处理在途任务"
			return persist()
		}
		if err := state.Config.Validate(); err != nil {
			state.Message = err.Error()
			return persist()
		}
		if err := s.validateGroups(ctx, state.Config); err != nil {
			state.Message = err.Error()
			return persist()
		}
		if _, err := s.selectProxy(ctx, state.Config.Delivery); err != nil {
			state.Message = err.Error()
			return persist()
		}
		if state.Config.Mode == "interval" && !timeoutRetry {
			if state.NextIntervalAt.IsZero() {
				state.NextIntervalAt = now.Add(time.Duration(state.Config.IntervalMinutes) * time.Minute)
			}
			if !now.Before(state.NextIntervalAt) && state.IntervalRemaining == 0 {
				state.IntervalRemaining = state.Config.IntervalQuantity
				state.NextIntervalAt = now.Add(time.Duration(state.Config.IntervalMinutes) * time.Minute)
			}
			state.Evaluation.Needed = refillMin(state.IntervalRemaining, refillMax(0, state.Config.MaxReady-state.Evaluation.Ready-state.Evaluation.Pending), refillMax(0, state.Config.MaxInFlight-state.Evaluation.Pending))
			state.Evaluation.Reason = "定时补号"
			if state.Evaluation.waitingForReview {
				state.Evaluation.Needed = 0
			}
		}
		if state.Evaluation.Needed <= 0 {
			// A pending backup temporarily covers this gap. Keep its receipt so a
			// failed or ambiguous delivery cannot fund the same shortage twice.
			if state.Evaluation.Pending == 0 || (state.MuzeFallback.Shortage != nil && state.MuzeFallback.Shortage.BackupJobID == "") {
				state.MuzeFallback.Shortage = nil
			}
			state.DemandSince = nil
			return persist()
		}
		// Minimum-reserve gaps act immediately. Capacity-only bursts must persist.
		if state.Evaluation.Reason == "并发利用率超过目标" {
			observation := s.capacityObservation(state.Config, state.Revision, time.Now())
			if !observation.Ready {
				state.Message = fmt.Sprintf("并发观察 %d/%d 秒，持续达到目标后才补号", observation.SustainedSeconds, observation.RequiredSeconds)
				return persist()
			}
		}
		replacement := nextRefillFailureReplacement(&state)
		if timeoutRetry {
			replacement = -1
		}
		immediateReplacement := replacement >= 0 && refillImmediateFailureRetryEnabled(state.Config)
		if !timeoutRetry && !immediateReplacement && now.Sub(state.LastSubmitAt) < time.Duration(state.Config.CooldownSeconds)*time.Second {
			state.Message = "补号冷却中，等待下次检查"
			return persist()
		}
		plan := nextRefillPurchasePlan(&state)
		if timeoutRetry {
			plan = state.Evaluation.TimeoutRetryPlan
		}
		quote, connectionID, supplyFallback, err := s.automaticQuote(ctx, &state, plan, now, persist)
		if err != nil {
			state.Message = err.Error()
			return persist()
		}
		orderProvider := primary
		if supplyFallback {
			orderProvider = state.Config.backupProvider()
			plan = quote.Plan
		}
		day := now.In(refillTimezone).Format("2006-01-02")
		usage := state.Usage[day]
		if usage.Count >= state.Config.DailyMaxCount || usage.Points+quote.Price > state.Config.DailyMaxPoints+1e-8 {
			state.Message = "已达到本日数量或积分预算"
			return persist()
		}
		graceMinutes := state.Config.ImportExpiryGraceMinutes
		// Provider calls may be slow. Every trigger rechecks local demand just
		// before reserving money, so a stale quota snapshot cannot cause a refill.
		if err := s.evaluate(ctx, &state, time.Now()); err != nil {
			s.resetCapacityObservation()
			state.Message = err.Error()
			return persist()
		}
		s.invalidateCapacityObservations(state.Config, state.Evaluation)
		timeoutNotice = captureRefillTimeoutNotice(&state, time.Now())
		if state.Config.Mode == "interval" && !timeoutRetry {
			state.Evaluation.Needed = refillMin(state.IntervalRemaining, refillMax(0, state.Config.MaxReady-state.Evaluation.Ready-state.Evaluation.Pending), refillMax(0, state.Config.MaxInFlight-state.Evaluation.Pending))
			state.Evaluation.Reason = "定时补号"
			if state.Evaluation.waitingForReview {
				state.Evaluation.Needed = 0
			}
		}
		if state.Evaluation.Needed <= 0 || state.Evaluation.TimeoutRetryJobID != timeoutRetryID {
			reconcileRefillFailureReplacements(&state)
			state.Message = state.Evaluation.Reason
			return persist()
		}
		if refillNeedsCapacityObservation(state.Evaluation.Reason) && !s.refillObservationReady(state.Config, state.Revision, state.Evaluation, time.Now()) {
			state.Message = "并发观察尚未连续达标，等待下次检查"
			if state.Evaluation.Reason == refillForecastReason {
				state.Message = "退出预判尚未连续达标，等待下次检查"
			}
			return persist()
		}
		job := AccountRefillJob{ID: uuid.NewString(), Provider: orderProvider, SupplyFallback: supplyFallback, MuzeConnectionID: connectionID, MuzeProductID: quote.muzeProductID, MuzeStudio: quote.muzeStudio, Status: "submitting", Reason: state.Evaluation.Reason, Plan: plan, Price: quote.Price, PriceVersion: quote.PriceVersion,
			RotationGeneration: state.RotationGeneration,
			Mode:               state.Config.Mode,
			ReasonDetail:       accountRefillReasonDetail(state.Config, state.Evaluation),
			ExpiryGraceMinutes: &graceMinutes,
			GroupID:            state.Config.GroupID, Concurrency: state.Config.importConcurrencyForPlan(plan), DeliveryGroupIDs: slices.Clone(state.Config.DeliveryGroupIDs), DeliveryConfig: state.Config.Delivery,
			CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
		if timeoutRetry {
			job.TimeoutRetryOf = timeoutRetryID
			job.ReasonDetail = refillTimeoutReasonDetail(&state)
		}
		if supplyFallback {
			job.Reason = refillProviderName(orderProvider) + "自动备用补号"
			job.ReasonDetail = refillProviderName(primary) + " 已确认暂时无法供应套餐，按分组规则从备用号商补入 Pro20x"
			state.MuzeFallback.Shortage.BackupJobID = job.ID
			if timeoutRetry {
				for i := range state.Jobs {
					if state.Jobs[i].ID == timeoutRetryID {
						state.Jobs[i].MuzeFallbackJobID = job.ID
						job.FallbackForJobID = timeoutRetryID
						break
					}
				}
			}
			state.MuzeFallback.LastAt = &now
			state.MuzeFallback.Message = job.ReasonDetail
		}
		if replacement >= 0 {
			linkRefillFailureReplacement(&state, replacement, &job)
		}
		if !timeoutRetry {
			claimRefillExpiryDemand(&state, &job, replacement)
		}
		state.Jobs = append([]AccountRefillJob{job}, state.Jobs...)
		state.Jobs = trimAccountRefillJobs(state.Jobs, now)
		usage.Count++
		usage.Points += quote.Price
		state.Usage[day] = usage
		for d := range state.Usage {
			if d < now.AddDate(0, 0, -32).In(refillTimezone).Format("2006-01-02") {
				delete(state.Usage, d)
			}
		}
		state.LastSubmitAt = now
		if replacement < 0 && !timeoutRetry && !supplyFallback {
			advanceRefillRotation(&state)
		}
		if state.Config.Mode == "interval" && !timeoutRetry {
			state.IntervalRemaining = refillMax(0, state.IntervalRemaining-1)
		}
		state.Message = "已预留预算，正在提交补号"
		if err := persist(); err != nil {
			return err
		}
		return s.submitRefillReservation(ctx, &state, &state.Jobs[0], token, persist)
	})
}

func refillRetryDelay(err error, attempt int) time.Duration {
	delay := time.Duration(15*(1<<refillMin(attempt, 5))) * time.Second
	var remote *refillProviderError
	if errors.As(err, &remote) {
		delay = refillMax(delay, remote.retryAfter)
		if remote.code == 401 || remote.code == 403 {
			delay = refillMax(delay, 5*time.Minute)
		}
	}
	return delay
}
func (s *AccountRefillService) advance(ctx context.Context, token string, state *accountRefillState, j *AccountRefillJob, persist func() error) error {
	if err := s.notifyRefillStarted(ctx, state, j, persist); err != nil {
		return err
	}
	now := time.Now()
	j.UpdatedAt = now
	if j.SupplierStatus == "succeeded" && !j.ResultReady && refillDeliveryExpired(j.ExpiresAt, now) {
		finishRefillDeliveryExpired(j, now)
		return persist()
	}
	if j.SupplierID != "" && j.SupplierStatus == "succeeded" && j.ResultReady {
		if !state.Config.PushEnabled {
			j.Status = "awaiting_push"
			j.NextAttemptAt = now.Add(time.Minute)
			return persist()
		}
		j.Status = "importing"
		if err := persist(); err != nil {
			return err
		}
		if err := s.importDelivery(ctx, token, j); err != nil {
			j.Status = "import_failed"
			j.Error = "交付导入失败，请检查目标分组、代理或租期后重试原任务"
			j.NextAttemptAt = now.Add(time.Minute)
			var remote *refillProviderError
			var temporary *refillProviderTemporaryError
			if errors.As(err, &temporary) || (errors.As(err, &remote) && (remote.code == 409 || remote.code == 429 || remote.code >= 500)) {
				j.Status = "awaiting_delivery"
				j.ResultReady = false // Recheck the original task before fetching again.
				j.Error = "号商交付暂不可用，继续查询原任务"
				j.NextAttemptAt = nextRefillOrderPoll(err)
			}
			if remote != nil {
				if remote.code == 429 {
					state.ProviderRetryAt = j.NextAttemptAt
				}
				if remote.code == 410 {
					finishRefillDeliveryExpired(j, now)
				} else if j.Status == "import_failed" {
					j.Error = remote.Error()
				}
			}
			if errors.Is(err, errRefillDeliveryExpired) {
				finishRefillDeliveryExpired(j, now)
			}
		} else {
			j.Status = "imported"
			j.Error = ""
			j.FinishedAt = &now
			completeRefillExpiryDemand(state, *j)
			state.Message = "新账号已导入并绑定分组"
			if err := s.notifyRefillImported(ctx, state, j, persist); err != nil {
				return err
			}
		}
		return persist()
	}
	var remote *RefillProviderJob
	var err error
	if j.SupplierID == "" {
		remote, err = s.provider.Submit(ctx, token, *j)
	} else {
		remote, err = s.provider.Status(ctx, token, j.SupplierID)
	}
	if err == nil {
		if err = validateRefillProviderJob(remote, *j); err != nil {
			j.Status = "supplier_unknown"
			j.ResultReady = false
		}
	}
	j.Attempts++
	if err != nil {
		j.Error = err.Error()
		j.NextAttemptAt = now.Add(refillRetryDelay(err, j.Attempts))
		if j.SupplierID != "" {
			j.NextAttemptAt = nextRefillOrderPoll(err)
		}
		var e *refillProviderError
		if errors.As(err, &e) {
			if e.code == 429 {
				state.ProviderRetryAt = j.NextAttemptAt
			}
			if e.code >= 400 && e.code < 500 && e.code != 429 {
				j.Status = "blocked"
			}
		}
		state.Message = "原任务等待重试，不新建重复订单"
		return persist()
	}
	j.SupplierID = remote.ID
	j.SupplierStatus = remote.Status
	publicError := []rune(strings.TrimSpace(remote.Error))
	j.SupplierError = string(publicError[:refillMin(len(publicError), 1024)])
	j.Status = remote.Status
	j.ResultReady = remote.ResultReady
	if remote.ExpiresAt != nil {
		j.ExpiresAt = remote.ExpiresAt
		if *remote.ExpiresAt > 0 && *remote.ExpiresAt <= 253402214399 {
			j.SupplierExpiresAt = remote.ExpiresAt
		}
	}
	j.Error = ""
	j.NextAttemptAt = nextRefillOrderPoll(nil)
	if !refillProviderStatusKnown(remote.Status) {
		j.Status = "supplier_unknown"
		j.ResultReady = false
		j.Error = "号商返回未知状态，保留原任务继续核对；超过设定时间仍未导入时不再阻塞自动补号"
		return persist()
	}
	if remote.Status == "review" {
		j.Error = "号商要求人工核对，保留原任务；超过设定时间仍未导入时不再阻塞自动补号"
	}
	if remote.Status == "failed" {
		j.Error = "号商确认补号失败，预算预占已释放"
		j.FinishedAt = &now
		if !j.BudgetReleased {
			day := j.CreatedAt.In(refillTimezone).Format("2006-01-02")
			u := state.Usage[day]
			u.Points = refillMax(0, u.Points-j.Price)
			state.Usage[day] = u
			j.BudgetReleased = true
			markRefillFailureReplacement(state, j)
		}
		if err := s.notifyRefillFailed(ctx, state, j, persist); err != nil {
			return err
		}
	}
	if remote.Status == "succeeded" {
		j.Status = "awaiting_delivery"
		if refillDeliveryExpired(j.ExpiresAt, now) {
			finishRefillDeliveryExpired(j, now)
		}
	}
	return persist()
}

func (s *AccountRefillService) Retry(ctx context.Context, id string) error {
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		for i := range state.Jobs {
			j := &state.Jobs[i]
			if j.ID != id {
				continue
			}
			if j.Provider == retiredRefillProvider {
				return errors.New("旧号商订单仅供核对历史记录")
			}
			if !slices.Contains([]string{"blocked", "import_failed"}, j.Status) {
				return errors.New("此任务无需人工重试")
			}
			if j.Provider == refillProviderMuze && j.SupplierID == "" && j.SubmissionStartedAt != nil {
				return errors.New("muze下单结果未确认，请关联原订单，或确认未受理后释放；不能重复下单")
			}
			j.Status = "submitting"
			if j.SupplierID != "" {
				j.Status = "processing"
			}
			j.Error = ""
			j.NextAttemptAt = time.Time{}
			// Manual retries also respect the provider-wide Retry-After deadline.
			j.NextAttemptAt = state.providerBackoff(j.Provider)
			return saveRefillState(&state, save)
		}
		return errors.New("任务不存在")
	})
}

func refillTemplate(template, name, email, id, plan string, push, expires time.Time) string {
	return strings.NewReplacer("{name}", name, "{email}", email, "{id}", id, "{plan_type}", plan,
		"{push_time}", push.In(refillTimezone).Format("2006-01-02 15:04:05"), "{expire_time}", expires.In(refillTimezone).Format("2006-01-02 15:04:05")).Replace(template)
}
func (s *AccountRefillService) importDelivery(ctx context.Context, token string, j *AccountRefillJob) error {
	// Recover after a crash between CreateAccount and persisting the job result.
	existing, err := s.accounts.FindByExtraField(ctx, "refill_job_id", j.ID)
	if err != nil {
		return err
	}
	if len(existing) > 1 {
		return errors.New("发现重复导入标记，需要核对")
	}
	var account *Account
	if len(existing) == 1 {
		account = &existing[0]
	} else {
		// Recover an already-created account before checking the download lease:
		// a crash after activation must not erase the committed import result.
		if refillDeliveryExpired(j.ExpiresAt, time.Now()) {
			return errRefillDeliveryExpired
		}
		delivery, err := s.provider.Delivery(ctx, token, j.SupplierID)
		if err != nil {
			return err
		}
		if err := validateRefillDelivery(delivery); err != nil {
			return err
		}
		input, err := s.refillDeliveryInput(ctx, j, delivery)
		if err != nil {
			return err
		}
		if j.DeliveryConfig.ProxyMode == "random" || j.DeliveryConfig.ProxyMode == "ovh_ipv6" {
			creator, ok := s.admin.(interface {
				CreateAccountWithProxySelection(context.Context, *CreateAccountInput) (*Account, error)
			})
			if !ok {
				return errors.New("事务代理分配服务不可用，请稍后重试")
			}
			input.ProxyMode = j.DeliveryConfig.ProxyMode
			account, err = creator.CreateAccountWithProxySelection(ctx, input)
		} else {
			account, err = s.admin.CreateAccount(ctx, input)
		}
		if err != nil {
			return err
		}
	}
	return s.finishRefillImport(ctx, account.ID, j)
}

func (s *AccountRefillService) finishRefillImport(ctx context.Context, accountID int64, j *AccountRefillJob) error {
	if err := s.accounts.BindGroups(ctx, accountID, j.DeliveryGroupIDs); err != nil {
		return err
	}
	if err := s.accounts.SetSchedulable(ctx, accountID, j.DeliveryConfig.Schedulable); err != nil {
		return err
	}
	if _, err := s.admin.UpdateAccount(ctx, accountID, &UpdateAccountInput{Status: j.DeliveryConfig.Status}); err != nil {
		return err
	}
	j.AccountID = accountID
	return nil
}

func (s *AccountRefillService) selectProxy(ctx context.Context, d AccountRefillDeliveryConfig) (*int64, error) {
	if d.ProxyMode == "ovh_ipv6" {
		r, ok := s.proxies.(OVHIPv6Admin)
		if !ok {
			return nil, errors.New("OVH IPv6 出口服务不可用")
		}
		status, err := r.GetOVHIPv6(ctx)
		if err != nil {
			return nil, err
		}
		if !status.Configured || !status.Available {
			return nil, errors.New("OVH IPv6 出口服务未就绪，暂停新增补号")
		}
		return nil, nil
	}
	if d.ProxyMode == "direct" {
		return nil, nil
	}
	if d.ProxyMode == "random" {
		pool, ok := s.proxies.(interface {
			HasFreeAccountProxy(context.Context) (bool, error)
		})
		if !ok {
			return nil, errors.New("空闲代理查询不可用，请稍后重试")
		}
		free, err := pool.HasFreeAccountProxy(ctx)
		if err != nil {
			return nil, errors.New("无法读取空闲代理，请稍后重试")
		}
		if !free {
			return nil, errors.New("没有未达绑定上限的有效空闲代理，暂停新增补号")
		}
		return nil, nil // Preflight only; reserve atomically when creating the account.
	}
	proxies, err := s.proxies.ListActive(ctx)
	if err != nil {
		return nil, errors.New("无法读取代理，请稍后重试")
	}
	now := time.Now()
	for _, p := range proxies {
		if !p.IsExpired(now) {
			if d.ProxyMode == "specified" && d.ProxyID != nil && p.ID == *d.ProxyID {
				if p.ManagedIPv6 != "" || strings.HasPrefix(p.Username, "ovh6_") {
					return nil, errors.New("OVH IPv6 为独占地址，补号请使用 ovh_ipv6 模式为每个账号分配全新地址")
				}
				id := p.ID
				return &id, nil
			}
		}
	}
	return nil, errors.New("没有可用且未过期的代理，请检查推送设置")
}

// DismissRejected is an explicit administrator reconciliation, never an automatic retry.
// Only submissions rejected before any supplier task ID can be reconciled here.
func (s *AccountRefillService) DismissRejected(ctx context.Context, id string) error {
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		for i := range state.Jobs {
			j := &state.Jobs[i]
			if j.ID != id {
				continue
			}
			if j.Status == "blocked" && j.Provider == retiredRefillProvider {
				now := time.Now()
				j.Status = "dismissed"
				j.FinishedAt = &now
				j.UpdatedAt = now
				j.Error = "管理员已核对旧订单并归档"
				return saveRefillState(&state, save)
			}
			if j.Status != "blocked" || j.SupplierID != "" {
				return errors.New("仅未返回号商任务 ID 的拒绝请求可核对释放；在途任务必须继续查询")
			}
			now := time.Now()
			j.Status = "dismissed"
			j.FinishedAt = &now
			j.UpdatedAt = now
			j.Error = "管理员确认号商未受理，已释放本地预算预占"
			if !j.BudgetReleased {
				day := j.CreatedAt.In(refillTimezone).Format("2006-01-02")
				usage := state.Usage[day]
				usage.Points = refillMax(0, usage.Points-j.Price)
				state.Usage[day] = usage
				j.BudgetReleased = true
			}
			state.NextCheckAt = time.Time{}
			state.LastSubmitAt = now
			return saveRefillState(&state, save)
		}
		return errors.New("任务不存在")
	})
}

// Fresh deliveries take priority over timed-out reconciliation. Within each
// class, advance the oldest due transition so new accounts are not starved by
// an accumulating backlog of supplier reviews.
func nextAccountRefillJob(jobs []AccountRefillJob, c AccountRefillConfig, now time.Time) int {
	selected := -1
	for i, j := range jobs {
		if j.Provider != "" || !j.pending() || now.Before(j.NextAttemptAt) || j.Status == "blocked" || j.Status == "import_failed" || (!c.PushEnabled && j.Status == "awaiting_push") {
			continue
		}
		if selected >= 0 && refillJobTimedOut(c, j, now) != refillJobTimedOut(c, jobs[selected], now) {
			if !refillJobTimedOut(c, j, now) {
				selected = i
			}
			continue
		}
		if selected < 0 || j.NextAttemptAt.Before(jobs[selected].NextAttemptAt) || (j.NextAttemptAt.Equal(jobs[selected].NextAttemptAt) && j.CreatedAt.Before(jobs[selected].CreatedAt)) {
			selected = i
		}
	}
	return selected
}
