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
	notifier   refillNotifier
	capacityMu sync.Mutex
	capacity   refillCapacityTracker
	store      AccountRefillStore
	encryptor  SecretEncryptor
	accounts   accountRefillAccounts
	admin      accountRefillAdmin
	groups     accountRefillGroups
	loads      accountRefillLoads
	proxies    accountRefillProxies
	provider   RefillProvider
	stop       chan struct{}
	cancel     context.CancelFunc
	once       sync.Once
	wg         sync.WaitGroup
}

func NewAccountRefillService(store AccountRefillStore, encryptor SecretEncryptor, accounts AccountRepository, admin AdminService, groups GroupRepository, loads *ConcurrencyService, proxies ProxyRepository) *AccountRefillService {
	return &AccountRefillService{store: store, encryptor: encryptor, accounts: accounts, admin: admin, groups: groups, loads: loads, proxies: proxies, provider: newHTTPRefillProvider(), stop: make(chan struct{})}
}
func (s *AccountRefillService) Start() {
	ctx, stop := context.WithCancel(context.Background())
	s.cancel = stop
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
				s.sampleCapacity(sampleCtx)
				cancel()
			}
		}
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
				err := s.run(ctx, false)
				cancel()
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
	WebhookConfigured         bool                             `json:"webhook_configured"`
	NotificationStatus        AccountRefillNotificationStatus  `json:"notification_status"`
	LeaseSyncAt               *time.Time                       `json:"lease_sync_at"`
	LeaseSyncMessage          string                           `json:"lease_sync_message"`
	CapacityObservation       AccountRefillCapacityObservation `json:"capacity_observation"`
	Config                    AccountRefillConfig              `json:"config"`
	Revision                  int64                            `json:"revision"`
	TokenConfigured           bool                             `json:"token_configured"`
	Key                       *RefillKeyInfo                   `json:"key"`
	Wallet                    *RefillWallet                    `json:"wallet"`
	Catalog                   *RefillCatalog                   `json:"catalog"`
	CatalogAt                 *time.Time                       `json:"catalog_at"`
	IdentityAt                *time.Time                       `json:"identity_at"`
	Jobs                      []AccountRefillJob               `json:"jobs"`
	Evaluation                AccountRefillEvaluation          `json:"evaluation"`
	Daily                     AccountRefillDailyUsage          `json:"daily"`
	NextCheckAt               time.Time                        `json:"next_check_at"`
	NextIntervalAt            time.Time                        `json:"next_interval_at"`
	Message                   string                           `json:"message"`
	ImportExpiryOffsetMinutes int                              `json:"import_expiry_offset_minutes"`
}

func (s *AccountRefillService) View(ctx context.Context) (*AccountRefillView, error) {
	raw, err := s.store.Read(ctx)
	if err != nil {
		return nil, err
	}
	state, err := decodeAccountRefillState(raw)
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
	return &AccountRefillView{WebhookConfigured: state.EncryptedWebhook != "", NotificationStatus: state.NotificationStatus, Config: state.Config, Revision: state.Revision, TokenConfigured: state.EncryptedToken != "", Key: state.Key, Wallet: state.Wallet, LeaseSyncAt: state.LeaseSyncAt, LeaseSyncMessage: state.LeaseSyncMessage,
		Catalog: state.Catalog, CatalogAt: state.CatalogAt, IdentityAt: state.IdentityAt, Jobs: state.Jobs, Evaluation: state.Evaluation,
		Daily: daily, NextCheckAt: state.NextCheckAt, NextIntervalAt: state.NextIntervalAt, Message: state.Message, ImportExpiryOffsetMinutes: state.Config.ImportExpiryGraceMinutes,
		CapacityObservation: s.capacityObservation(state.Config, state.Revision, time.Now())}, nil
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
	if err := c.Validate(); err != nil {
		return err
	}
	webhook := ""
	if len(webhooks) > 0 {
		webhook = strings.TrimSpace(webhooks[0])
	}
	if webhook != "" {
		if err := validateRefillWebhook(webhook); err != nil {
			return err
		}
	}
	if c.Enabled {
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
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		if revision != state.Revision {
			return errors.New("配置已被其他页面修改，请刷新后再保存")
		}
		if webhook != "" {
			cipher, err := s.encryptor.Encrypt(webhook)
			if err != nil {
				return errors.New("机器人地址加密失败")
			}
			state.EncryptedWebhook = cipher
		}
		if (c.Notifications.RefillStarted || c.Notifications.LowBalance) && state.EncryptedWebhook == "" {
			return errors.New("启用通知前请配置企业微信群机器人 Webhook")
		}
		if webhook != "" || c.Notifications != state.Config.Notifications {
			state.LowBalanceAlerted = false
			state.LowBalanceRequired = 0
			state.NextBalanceCheckAt = time.Time{}
		}
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
				if j.pending() {
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
		if c.Enabled && state.EncryptedToken == "" {
			return errors.New("启用前请配置号商 API Key")
		}
		if c.Mode != state.Config.Mode || c.IntervalMinutes != state.Config.IntervalMinutes || c.IntervalQuantity != state.Config.IntervalQuantity {
			state.NextIntervalAt = time.Now().Add(time.Duration(c.IntervalMinutes) * time.Minute)
			state.IntervalRemaining = 0
		}
		state.Config = c
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
	state.Evaluation = evaluateAccountRefill(state.Config, accounts, loads, state.Jobs, now)
	return nil
}

func (s *AccountRefillService) run(ctx context.Context, preview bool) error {
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		if raw == "" && !preview {
			return nil
		}
		now := time.Now()
		persist := func() error { return saveRefillState(&state, save) }
		if now.Before(state.ProviderRetryAt) {
			return nil
		}
		var token string
		if state.EncryptedToken != "" {
			token, err = s.encryptor.Decrypt(state.EncryptedToken)
			if err != nil {
				state.Message = "密钥解密失败，请重新保存 API Key"
				return persist()
			}
		}
		if !preview && token != "" {
			// Continue accepted work even when automation or its time window is off.
			if index := nextAccountRefillJob(state.Jobs, state.Config.PushEnabled, now); index >= 0 {
				if err := s.advance(ctx, token, &state, &state.Jobs[index], persist); err != nil {
					return err
				}
			}
		}
		if time.Now().Before(state.ProviderRetryAt) {
			return nil
		}
		if !preview {
			if err := s.checkRefillNotificationBalance(ctx, token, &state, now, persist); err != nil {
				return err
			}
			if time.Now().Before(state.ProviderRetryAt) {
				return nil
			}
		}
		if !preview && (now.Before(state.NextCheckAt) || !state.Config.MonitorEnabled) {
			return nil
		}
		state.NextCheckAt = now.Add(time.Duration(state.Config.CheckSeconds) * time.Second)
		if token != "" {
			s.syncSourceLeases(ctx, token, &state, now, preview)
		}
		if err := s.evaluate(ctx, &state, now); err != nil {
			state.Message = err.Error()
			return persist()
		}
		if time.Now().Before(state.ProviderRetryAt) {
			state.Message = "号商限流，等待重试后继续同步"
			return persist()
		}
		if token == "" {
			state.Message = "请配置号商 API Key"
			return persist()
		}
		catalog, err := s.provider.Catalog(ctx, token)
		if err != nil {
			state.Message = err.Error()
			state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
			return persist()
		}
		state.Catalog = catalog
		state.CatalogAt = &now
		if preview || state.IdentityAt == nil || now.Sub(*state.IdentityAt) >= 5*time.Minute {
			wallet, key, identityErr := s.provider.Identity(ctx, token)
			if wallet != nil {
				state.Wallet = wallet
			}
			if identityErr == nil {
				state.Key = key
				state.IdentityAt = &now
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
		if !refillInWindow(state.Config, now) {
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
		if state.Config.Mode == "interval" {
			if state.NextIntervalAt.IsZero() {
				state.NextIntervalAt = now.Add(time.Duration(state.Config.IntervalMinutes) * time.Minute)
			}
			if !now.Before(state.NextIntervalAt) && state.IntervalRemaining == 0 {
				state.IntervalRemaining = state.Config.IntervalQuantity
				state.NextIntervalAt = now.Add(time.Duration(state.Config.IntervalMinutes) * time.Minute)
			}
			state.Evaluation.Needed = refillMin(state.IntervalRemaining, refillMax(0, state.Config.MaxReady-state.Evaluation.Ready-state.Evaluation.Pending), refillMax(0, state.Config.MaxInFlight-state.Evaluation.Pending))
			state.Evaluation.Reason = "定时补号"
			for _, j := range state.Jobs {
				if slices.Contains([]string{"blocked", "review", "import_failed"}, j.Status) {
					state.Evaluation.Needed = 0
				}
			}
		}
		if state.Evaluation.Needed <= 0 {
			state.DemandSince = nil
			return persist()
		}
		// Minimum/expiry gaps act immediately. Capacity-only bursts must persist.
		if state.Evaluation.Reason == "并发利用率超过目标" {
			observation := s.capacityObservation(state.Config, state.Revision, time.Now())
			if !observation.Ready {
				state.Message = fmt.Sprintf("并发观察 %d/%d 秒，持续达到目标后才补号", observation.SustainedSeconds, observation.RequiredSeconds)
				return persist()
			}
		}
		if now.Sub(state.LastSubmitAt) < time.Duration(state.Config.CooldownSeconds)*time.Second {
			state.Message = "补号冷却中，等待下次检查"
			return persist()
		}
		var quote *RefillQuote
		for i := range catalog.Items {
			q := &catalog.Items[i]
			if q.Service == "refill" && q.Plan == state.Config.Plan && q.Enabled {
				quote = q
				break
			}
		}
		if quote == nil {
			state.Message = "所选补号套餐暂不可用"
			return persist()
		}
		if state.Key != nil && (!state.Key.ExpiresAt.IsZero() && !state.Key.ExpiresAt.After(now) || state.Key.Revoked) {
			state.Message = "号商 API Key 已失效，请替换"
			return persist()
		}
		if quote.Price > state.Config.MaxUnitPrice {
			state.Message = "当前单价超过允许上限，请确认报价"
			return persist()
		}
		day := now.In(refillTimezone).Format("2006-01-02")
		usage := state.Usage[day]
		if usage.Count >= state.Config.DailyMaxCount || usage.Points+quote.Price > state.Config.DailyMaxPoints+1e-8 {
			state.Message = "已达到本日数量或积分预算"
			return persist()
		}
		if catalog.Available+1e-8 < quote.Price {
			state.Message = "号商可用积分不足"
			if err := s.notifyRefillBalance(ctx, &state, catalog.Available, quote.Price, persist); err != nil {
				return err
			}
			return persist()
		}
		if state.Evaluation.Reason == "并发利用率超过目标" {
			// Re-read immediately before reserving a paid order. Slow provider calls
			// must not turn an already-ended burst into a purchase.
			if err := s.evaluate(ctx, &state, time.Now()); err != nil {
				s.resetCapacityObservation()
				state.Message = err.Error()
				return persist()
			}
			if !refillCapacityHigh(state.Config, state.Evaluation) {
				s.resetCapacityObservation()
				state.Message = "并发已回落，重新观察"
				return persist()
			}
			if state.Evaluation.Needed <= 0 || !s.capacityObservation(state.Config, state.Revision, time.Now()).Ready {
				state.Message = "并发观察尚未连续达标，等待下次检查"
				return persist()
			}
		}
		graceMinutes := state.Config.ImportExpiryGraceMinutes
		// Provider calls may be slow. Every trigger rechecks local demand just
		// before reserving money, so a stale quota snapshot cannot cause a refill.
		if err := s.evaluate(ctx, &state, time.Now()); err != nil {
			state.Message = err.Error()
			return persist()
		}
		if state.Config.Mode == "interval" {
			state.Evaluation.Needed = refillMin(state.IntervalRemaining, refillMax(0, state.Config.MaxReady-state.Evaluation.Ready-state.Evaluation.Pending), refillMax(0, state.Config.MaxInFlight-state.Evaluation.Pending))
			state.Evaluation.Reason = "定时补号"
		}
		if state.Evaluation.Needed <= 0 {
			state.Message = state.Evaluation.Reason
			return persist()
		}
		if state.Evaluation.Reason == "并发利用率超过目标" && !s.capacityObservation(state.Config, state.Revision, time.Now()).Ready {
			state.Message = "并发观察尚未连续达标，等待下次检查"
			return persist()
		}
		job := AccountRefillJob{ID: uuid.NewString(), Status: "submitting", Reason: state.Evaluation.Reason, Plan: state.Config.Plan, Price: quote.Price, PriceVersion: quote.PriceVersion,
			ExpiryGraceMinutes: &graceMinutes,
			GroupID:            state.Config.GroupID, Concurrency: state.Config.ImportConcurrency, DeliveryGroupIDs: slices.Clone(state.Config.DeliveryGroupIDs), DeliveryConfig: state.Config.Delivery,
			CreatedAt: now, UpdatedAt: now, NextAttemptAt: now}
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
		if state.Config.Mode == "interval" {
			state.IntervalRemaining = refillMax(0, state.IntervalRemaining-1)
		}
		state.Message = "已预留预算，正在提交补号"
		if err := persist(); err != nil {
			return err
		}
		return s.advance(ctx, token, &state, &state.Jobs[0], persist)
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
			if errors.As(err, &remote) && remote.code == 429 {
				state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
			}
			if errors.As(err, &remote) && remote.code == 410 {
				j.Status = "delivery_expired"
				j.Error = "号商交付已过租期"
				j.FinishedAt = &now
			}
		} else {
			j.Status = "imported"
			j.Error = ""
			j.FinishedAt = &now
			state.Message = "新账号已导入并绑定分组"
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
	if err == nil && (remote == nil || remote.ID == "" || len(remote.ID) > 128 || !slices.Contains([]string{"queued", "processing", "succeeded", "failed", "review"}, remote.Status) || (j.SupplierID != "" && remote.ID != j.SupplierID)) {
		err = errors.New("号商任务响应无效，保留原幂等键核对")
	}
	j.Attempts++
	if err != nil {
		j.Error = err.Error()
		j.NextAttemptAt = now.Add(refillRetryDelay(err, j.Attempts))
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
	j.Status = remote.Status
	j.ResultReady = remote.ResultReady
	j.ExpiresAt = remote.ExpiresAt
	j.Error = ""
	j.NextAttemptAt = now.Add(15 * time.Second)
	if remote.Status == "review" {
		j.Error = "号商要求人工核对，保留原任务并暂停新增补号"
		j.NextAttemptAt = now.Add(time.Minute)
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
		}
	}
	if remote.Status == "succeeded" {
		j.Status = "awaiting_delivery"
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
			if !slices.Contains([]string{"blocked", "import_failed"}, j.Status) {
				return errors.New("此任务无需人工重试")
			}
			j.Status = "submitting"
			if j.SupplierID != "" {
				j.Status = "processing"
			}
			j.Error = ""
			j.NextAttemptAt = time.Time{}
			// Manual retries also respect the provider-wide Retry-After deadline.
			j.NextAttemptAt = state.ProviderRetryAt
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
		delivery, err := s.provider.Delivery(ctx, token, j.SupplierID)
		if err != nil {
			return err
		}
		if len(delivery.Accounts) != 1 {
			return errors.New("号商交付必须包含一个账号")
		}
		a := delivery.Accounts[0]
		if a.Platform != PlatformOpenAI || a.Type != AccountTypeOAuth {
			return errors.New("交付账号类型不匹配")
		}
		credentials := map[string]any{}
		for _, key := range []string{"access_token", "refresh_token", "id_token", "client_id", "expires_at", "email", "chatgpt_account_id", "chatgpt_user_id", "organization_id", "plan_type"} {
			if value, ok := a.Credentials[key].(string); ok && value != "" {
				credentials[key] = value
			}
		}
		if value, ok := a.Credentials["expires_at"].(float64); ok {
			credentials["expires_at"] = value
		}
		if credentials["access_token"] == nil {
			return errors.New("交付缺少接入凭据")
		}
		deadline := a.ExpiresAt
		if j.ExpiresAt != nil && (deadline == nil || *j.ExpiresAt < *deadline) {
			deadline = j.ExpiresAt
		}
		if deadline == nil || *deadline <= time.Now().Unix() {
			return errors.New("交付缺少有效租期")
		}
		if j.DeliveryConfig.ExpiryMode == "fixed" && j.DeliveryConfig.FixedExpiresAt != nil && *j.DeliveryConfig.FixedExpiresAt < *deadline {
			deadline = j.DeliveryConfig.FixedExpiresAt
		}
		if *deadline <= time.Now().Unix() {
			return errors.New("指定截止时间已过期")
		}
		extra := map[string]any{"refill_job_id": j.ID, "refill_supplier_id": j.SupplierID, "import_source": "llmrefund_api",
			"codex_fingerprint_mode": j.DeliveryConfig.FingerprintMode, "openai_oauth_responses_websockets_v2_enabled": j.DeliveryConfig.WSMode != "off", "openai_oauth_responses_websockets_v2_mode": j.DeliveryConfig.WSMode}
		graceMinutes := AccountImportExpiryOffsetMinutes
		if j.ExpiryGraceMinutes != nil {
			graceMinutes = *j.ExpiryGraceMinutes
		}
		deadline, extra = ImportedAccountExpiryWithGrace(deadline, extra, graceMinutes)
		// The supplier lease is independent of a locally shortened deadline.
		if a.ExpiresAt != nil {
			extra[AccountSourceExpiryExtraKey] = *a.ExpiresAt
		}
		if j.ExpiresAt != nil {
			extra[AccountSourceExpiryExtraKey] = *j.ExpiresAt
		}
		extra[AccountSourceExpiryOriginExtraKey] = "supplier_api"
		extra[AccountSourceExpiryCheckedExtraKey] = time.Now().UTC().Format(time.RFC3339)
		if len(j.DeliveryConfig.ModelMapping) > 0 {
			credentials["model_mapping"] = j.DeliveryConfig.ModelMapping
		}
		email, _ := credentials["email"].(string)
		name := a.Name
		if name == "" {
			name = email
		}
		if name == "" {
			name = "补号 " + j.SupplierID
		}
		expires := time.Unix(*deadline, 0)
		pushTime := time.Now()
		if j.DeliveryConfig.NameTemplate != "" {
			name = refillTemplate(j.DeliveryConfig.NameTemplate, name, email, j.SupplierID, j.Plan, pushTime, expires)
		}
		var notes *string
		if j.DeliveryConfig.NotesTemplate != "" {
			n := refillTemplate(j.DeliveryConfig.NotesTemplate, a.Name, email, j.SupplierID, j.Plan, pushTime, expires)
			notes = &n
		}
		autoPause := j.DeliveryConfig.AutoPauseOnExpired
		var proxyID *int64
		if j.DeliveryConfig.ProxyMode != "random" {
			proxyID, err = s.selectProxy(ctx, j.DeliveryConfig)
			if err != nil {
				return err
			}
		}
		var load *int
		if j.DeliveryConfig.LoadFactor > 0 {
			v := j.DeliveryConfig.LoadFactor
			load = &v
		}
		// Start disabled until all delivery settings and bindings have committed.
		input := &CreateAccountInput{Name: name, Notes: notes, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: credentials, Extra: extra,
			GroupIDs: j.DeliveryGroupIDs, Concurrency: j.Concurrency, Priority: j.DeliveryConfig.Priority, RateMultiplier: &j.DeliveryConfig.RateMultiplier, LoadFactor: load,
			ExpiresAt: deadline, AutoPauseOnExpired: &autoPause, ProxyID: proxyID, InitiallyInactive: true, SkipDefaultGroupBind: true}
		if j.DeliveryConfig.ProxyMode == "random" {
			creator, ok := s.admin.(interface {
				CreateAccountWithProxySelection(context.Context, *CreateAccountInput) (*Account, error)
			})
			if !ok {
				return errors.New("事务代理分配服务不可用，请稍后重试")
			}
			input.ProxyMode = "random"
			account, err = creator.CreateAccountWithProxySelection(ctx, input)
		} else {
			account, err = s.admin.CreateAccount(ctx, input)
		}
		if err != nil {
			return err
		}
	}
	if err := s.accounts.BindGroups(ctx, account.ID, j.DeliveryGroupIDs); err != nil {
		return err
	}
	if err := s.accounts.SetSchedulable(ctx, account.ID, j.DeliveryConfig.Schedulable); err != nil {
		return err
	}
	if _, err := s.admin.UpdateAccount(ctx, account.ID, &UpdateAccountInput{Status: j.DeliveryConfig.Status}); err != nil {
		return err
	}
	j.AccountID = account.ID
	return nil
}

func (s *AccountRefillService) selectProxy(ctx context.Context, d AccountRefillDeliveryConfig) (*int64, error) {
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

// Advance the oldest due transition so a large queue cannot starve earlier orders.
func nextAccountRefillJob(jobs []AccountRefillJob, pushEnabled bool, now time.Time) int {
	selected := -1
	for i, j := range jobs {
		if !j.pending() || now.Before(j.NextAttemptAt) || j.Status == "blocked" || j.Status == "import_failed" || (!pushEnabled && j.Status == "awaiting_push") {
			continue
		}
		if selected < 0 || j.NextAttemptAt.Before(jobs[selected].NextAttemptAt) || (j.NextAttemptAt.Equal(jobs[selected].NextAttemptAt) && j.CreatedAt.Before(jobs[selected].CreatedAt)) {
			selected = i
		}
	}
	return selected
}
