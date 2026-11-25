package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"time"
)

type Refill558669Limits struct {
	MaxUnitPrice   float64 `json:"max_unit_price"`
	DailyMaxPoints float64 `json:"daily_max_points"`
}

func (c AccountRefillConfig) limits558669() Refill558669Limits {
	if c.Supplier558669 != nil {
		return *c.Supplier558669
	}
	return Refill558669Limits{MaxUnitPrice: 60, DailyMaxPoints: 3000}
}

func (c AccountRefillConfig) validate558669() error {
	l := c.limits558669()
	for _, value := range []float64{l.MaxUnitPrice, l.DailyMaxPoints} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1e7 {
			return errors.New("558669 积分预算无效")
		}
	}
	if l.DailyMaxPoints <= 0 {
		return errors.New("558669 每日预算须大于零")
	}
	if c.MuzeFallback.enabled() && c.MuzeFallback.Provider != "" && c.MuzeFallback.Provider == refillPublicProvider(c.automaticProvider()) {
		return errors.New("主号商与备用号商不能相同")
	}
	return nil
}

type refill558669State struct {
	EncryptedToken string                             `json:"encrypted_token,omitempty"`
	TokenHash      string                             `json:"token_hash,omitempty"`
	Catalog        *RefillCatalog                     `json:"catalog,omitempty"`
	CheckedAt      *time.Time                         `json:"checked_at,omitempty"`
	RetryAt        time.Time                          `json:"retry_at"`
	Usage          map[string]AccountRefillDailyUsage `json:"usage,omitempty"`
}

type Refill558669View struct {
	TokenConfigured bool                    `json:"token_configured"`
	Catalog         *RefillCatalog          `json:"catalog"`
	CheckedAt       *time.Time              `json:"checked_at"`
	RetryAt         time.Time               `json:"retry_at"`
	Daily           AccountRefillDailyUsage `json:"daily"`
}

func (state *accountRefillState) view558669() Refill558669View {
	day := time.Now().In(refillTimezone).Format("2006-01-02")
	daily := state.Supplier558669.Usage[day]
	for _, j := range state.Jobs {
		if j.Provider == refillProvider558669 && j.CreatedAt.In(refillTimezone).Format("2006-01-02") == day && j.pending() && j.SupplierStatus != "succeeded" {
			daily.ReservedPoints += j.Price
		}
	}
	daily.SpentPoints = math.Max(0, daily.Points-daily.ReservedPoints)
	f := state.Supplier558669
	return Refill558669View{f.EncryptedToken != "", f.Catalog, f.CheckedAt, f.RetryAt, daily}
}

func (s *AccountRefillService) Configure558669(ctx context.Context, token string, revision int64) error {
	token = strings.TrimSpace(token)
	if token != "" && !valid558669Token(token) {
		return invalidManualRefill("558669 API Key 格式无效")
	}
	return withAccountRefillConfigLock(ctx, s.store, 30*time.Second, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		if revision != state.Revision {
			return invalidManualRefill("配置已被其他页面修改，请刷新后保存")
		}
		if time.Now().Before(state.Supplier558669.RetryAt) {
			return invalidManualRefill("558669 正在退避，请稍后重试")
		}
		if token == "" {
			token, err = s.encryptor.Decrypt(state.Supplier558669.EncryptedToken)
		}
		if err != nil || !valid558669Token(token) {
			return invalidManualRefill("请先填写 558669 API Key")
		}
		digest := sha256.Sum256([]byte(token))
		hash := hex.EncodeToString(digest[:])
		for _, j := range state.Jobs {
			if j.Provider == refillProvider558669 && (j.pending() || j.needs558669Sync(time.Now())) && hash != state.Supplier558669.TokenHash {
				return invalidManualRefill("558669 仍有原任务或凭据更新待核对，不能更换 API Key")
			}
		}
		catalog, err := s.providerClient(refillProvider558669).Catalog(ctx, token)
		if err != nil || catalog == nil {
			state.Supplier558669.RetryAt = time.Now().Add(refillRetryDelay(err, 1))
			if e := saveRefillState(&state, save); e != nil {
				return e
			}
			return invalidManualRefill("无法读取 558669 报价，请检查密钥和读取权限")
		}
		cipher, err := s.encryptor.Encrypt(token)
		if err != nil {
			return errors.New("558669 API Key 加密失败")
		}
		now := time.Now()
		state.Supplier558669.EncryptedToken = cipher
		state.Supplier558669.TokenHash = hash
		state.Supplier558669.Catalog = catalog
		state.Supplier558669.CheckedAt = &now
		state.Supplier558669.RetryAt = time.Time{}
		state.Revision++
		return saveRefillState(&state, save)
	})
}

func (s *AccountRefillService) Catalog558669(ctx context.Context) (Refill558669View, error) {
	var view Refill558669View
	err := withAccountRefillConfigLock(ctx, s.store, 8*time.Second, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		if err = s.refresh558669(ctx, &state, func() error { return saveRefillState(&state, save) }); err != nil {
			return err
		}
		view = state.view558669()
		return nil
	})
	return view, err
}

func (s *AccountRefillService) refresh558669(ctx context.Context, state *accountRefillState, persist func() error) error {
	if time.Now().Before(state.Supplier558669.RetryAt) {
		return invalidManualRefill("558669 正在退避，请稍后重试")
	}
	token, err := s.encryptor.Decrypt(state.Supplier558669.EncryptedToken)
	if err != nil || token == "" {
		return invalidManualRefill("请先配置 558669 API Key")
	}
	catalog, err := s.providerClient(refillProvider558669).Catalog(ctx, token)
	if err != nil || catalog == nil {
		state.Supplier558669.RetryAt = time.Now().Add(refillRetryDelay(err, 1))
		if e := persist(); e != nil {
			return e
		}
		return invalidManualRefill("558669 报价读取失败，保留原任务")
	}
	now := time.Now()
	state.Supplier558669.Catalog = catalog
	state.Supplier558669.CheckedAt = &now
	state.Supplier558669.RetryAt = time.Time{}
	return persist()
}

func (s *AccountRefillService) quote558669(ctx context.Context, state *accountRefillState, plan string, persist func() error) (*RefillQuote, error) {
	if err := s.refresh558669(ctx, state, persist); err != nil {
		return nil, err
	}
	quote, err := quote558669FromCatalog(state, plan)
	if err != nil {
		return nil, err
	}
	held, err := s.reserved558669(ctx, state)
	if err != nil {
		return nil, err
	}
	if state.Supplier558669.Catalog.Available+1e-8 < quote.Price {
		return nil, supplyUnavailable("558669 可用积分不足")
	}
	if state.Supplier558669.Catalog.Available+1e-8 < quote.Price+held {
		return nil, invalidManualRefill("558669 可用积分不足（含本地预占）")
	}
	return quote, nil
}

func quote558669FromCatalog(state *accountRefillState, plan string) (*RefillQuote, error) {
	catalog := state.Supplier558669.Catalog
	if catalog == nil {
		return nil, invalidManualRefill("缺少 558669 报价")
	}
	for _, q := range catalog.Items {
		if q.Service != "refill" || q.Plan != plan {
			continue
		}
		if !q.Enabled {
			return nil, supplyUnavailable("558669 所选套餐暂不可用")
		}
		if q.Price > state.Config.limits558669().MaxUnitPrice {
			return nil, invalidManualRefill("558669 单价超过配置上限")
		}
		return &q, nil
	}
	return nil, supplyUnavailable("558669 未开放所选套餐")
}

func (s *AccountRefillService) reserved558669(ctx context.Context, state *accountRefillState) (float64, error) {
	var held float64
	add := func(jobs []AccountRefillJob) {
		for _, j := range jobs {
			if j.Provider == refillProvider558669 && j.pending() && j.SupplierID == "" {
				held += j.Price
			}
		}
	}
	add(state.Jobs)
	if stores, ok := s.rootService().store.(AccountRefillGroupStore); ok {
		rows, err := stores.ListRules(ctx)
		if err != nil {
			return 0, err
		}
		for id, raw := range rows {
			if id == s.ruleGroupID {
				continue
			}
			other, err := decodeAccountRefillState(raw)
			if err != nil {
				return 0, err
			}
			// No account ID is supplied by /catalog: conservatively reserve across
			// all 558669 rules, including different keys that may share one wallet.
			add(other.Jobs)
		}
	}
	return held, nil
}

func (state *accountRefillState) canReserveRefill(provider string, price float64, now time.Time) bool {
	day := now.In(refillTimezone).Format("2006-01-02")
	if state.Usage[day].Count >= state.Config.DailyMaxCount {
		return false
	}
	if provider == refillProvider558669 {
		return state.Supplier558669.Usage[day].Points+price <= state.Config.limits558669().DailyMaxPoints+1e-8
	}
	return state.Usage[day].Points+price <= state.Config.DailyMaxPoints+1e-8
}

func (state *accountRefillState) addRefillBudget(j AccountRefillJob) {
	day := j.CreatedAt.In(refillTimezone).Format("2006-01-02")
	u := state.Usage[day]
	u.Count++
	if j.Provider == refillProvider558669 {
		if state.Supplier558669.Usage == nil {
			state.Supplier558669.Usage = map[string]AccountRefillDailyUsage{}
		}
		v := state.Supplier558669.Usage[day]
		v.Count++
		v.Points += j.Price
		state.Supplier558669.Usage[day] = v
	} else {
		u.Points += j.Price
	}
	state.Usage[day] = u
}

func (state *accountRefillState) release558669Budget(j *AccountRefillJob) {
	if j.BudgetReleased {
		return
	}
	day := j.CreatedAt.In(refillTimezone).Format("2006-01-02")
	u := state.Supplier558669.Usage[day]
	u.Points = math.Max(0, u.Points-j.Price)
	if state.Supplier558669.Usage == nil {
		state.Supplier558669.Usage = map[string]AccountRefillDailyUsage{}
	}
	state.Supplier558669.Usage[day] = u
	j.BudgetReleased = true
}

func (j AccountRefillJob) needs558669Sync(now time.Time) bool {
	return j.Provider == refillProvider558669 && j.Status == "imported" && j.AccountID > 0 && !j.CredentialsSyncStopped &&
		!refillDeliveryExpired(j.SupplierExpiresAt, now)
}
