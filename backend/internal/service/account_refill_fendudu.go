package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

type fenduduRefillState struct {
	EncryptedToken string           `json:"encrypted_token,omitempty"`
	TokenHash      string           `json:"token_hash,omitempty"`
	ConnectionID   string           `json:"connection_id"`
	Products       []FenduduProduct `json:"products"`
	CheckedAt      *time.Time       `json:"checked_at"`
	RetryAt        time.Time        `json:"retry_at"`
}

type FenduduRefillView struct {
	TokenConfigured bool             `json:"token_configured"`
	ConnectionID    string           `json:"connection_id"`
	Products        []FenduduProduct `json:"products"`
	CheckedAt       *time.Time       `json:"checked_at"`
}

func (f fenduduRefillState) view() FenduduRefillView {
	return FenduduRefillView{TokenConfigured: f.EncryptedToken != "", ConnectionID: f.ConnectionID, Products: f.Products, CheckedAt: f.CheckedAt}
}

func (s *AccountRefillService) fenduduClient() fenduduProvider {
	if s.fendudu != nil {
		return s.fendudu
	}
	return newHTTPFenduduProvider()
}

// ConfigureFendudu changes only the backup connection. Blank tokens keep the
// encrypted secret; no API response contains the secret or its hash.
func (s *AccountRefillService) ConfigureFendudu(ctx context.Context, token, connectionID string, revision int64) error {
	token, connectionID = strings.TrimSpace(token), strings.TrimSpace(connectionID)
	if token != "" && (!strings.HasPrefix(token, "rvak_") || len(token) > 256 || strings.ContainsAny(token, "\r\n\t ")) {
		return errors.New("粉嘟嘟对接码格式无效")
	}
	if !validFenduduIdentifier(connectionID, true) {
		return errors.New("粉嘟嘟推送连接 ID 无效")
	}
	return withAccountRefillConfigLock(ctx, s.store, 30*time.Second, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		if revision != state.Revision {
			return errors.New("配置已被其他页面修改，请刷新后再保存")
		}
		secret := token
		if secret == "" && state.Fendudu.EncryptedToken != "" {
			secret, err = s.encryptor.Decrypt(state.Fendudu.EncryptedToken)
		}
		if err != nil || secret == "" {
			return errors.New("请填写有效的粉嘟嘟对接码")
		}
		digest := sha256.Sum256([]byte(secret))
		hash := hex.EncodeToString(digest[:])
		for _, j := range state.Jobs {
			if j.Provider == refillProviderFendudu && j.pending() && (hash != state.Fendudu.TokenHash || connectionID != state.Fendudu.ConnectionID) {
				return errors.New("粉嘟嘟仍有在途订单，请先处理完成再更换对接码或连接")
			}
		}
		if time.Now().Before(state.Fendudu.RetryAt) {
			return errors.New("粉嘟嘟正在退避，请稍后再试")
		}
		products, err := s.fenduduClient().Products(ctx, secret)
		if err != nil {
			state.Fendudu.RetryAt = time.Now().Add(refillRetryDelay(err, 1))
			if saveErr := saveRefillState(&state, save); saveErr != nil {
				return saveErr
			}
			return errors.New("无法读取粉嘟嘟产品次数，请检查对接码及服务状态")
		}
		cipher, err := s.encryptor.Encrypt(secret)
		if err != nil {
			return errors.New("粉嘟嘟对接码加密失败")
		}
		now := time.Now()
		state.Fendudu = fenduduRefillState{EncryptedToken: cipher, TokenHash: hash, ConnectionID: connectionID, Products: products, CheckedAt: &now}
		state.Revision++
		return saveRefillState(&state, save)
	})
}

func validFenduduIdentifier(value string, empty bool) bool {
	if value == "" {
		return empty
	}
	if len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (s *AccountRefillService) refreshFenduduProducts(ctx context.Context, state *accountRefillState, now time.Time, force bool, persist func() error) error {
	f := &state.Fendudu
	if f.EncryptedToken == "" || now.Before(f.RetryAt) || (!force && f.CheckedAt != nil && now.Sub(*f.CheckedAt) < 5*time.Minute) {
		return nil
	}
	secret, err := s.encryptor.Decrypt(f.EncryptedToken)
	if err != nil {
		f.RetryAt = now.Add(5 * time.Minute)
		return persist()
	}
	products, err := s.fenduduClient().Products(ctx, secret)
	if err != nil {
		f.RetryAt = now.Add(refillRetryDelay(err, 1))
		return persist()
	}
	f.Products, f.CheckedAt, f.RetryAt = products, &now, time.Time{}
	return persist()
}

func (s *AccountRefillService) fenduduManualQuote(ctx context.Context, state *accountRefillState, plan string, persist func() error) (*RefillQuote, error) {
	if state.Fendudu.EncryptedToken == "" {
		return nil, invalidManualRefill("请先配置粉嘟嘟对接码")
	}
	token, err := s.encryptor.Decrypt(state.Fendudu.EncryptedToken)
	if err != nil {
		return nil, invalidManualRefill("粉嘟嘟对接码解密失败，请重新保存")
	}
	products, err := s.fenduduClient().Products(ctx, token)
	if err != nil {
		state.Fendudu.RetryAt = time.Now().Add(refillRetryDelay(err, 1))
		if err := persist(); err != nil {
			return nil, err
		}
		return nil, invalidManualRefill("无法读取粉嘟嘟产品次数，请稍后再试")
	}
	now := time.Now()
	state.Fendudu.Products, state.Fendudu.CheckedAt = products, &now
	if err := persist(); err != nil {
		return nil, err
	}
	reserved, err := s.fenduduReserved(ctx, state, plan)
	if err != nil {
		return nil, err
	}
	for _, p := range products {
		if p.Product == plan && !p.Expired && p.BlockedReason == "" && p.AvailableNow > reserved {
			// Prepaid quotas have no documented points quote. They share the
			// daily order limit, but never fabricate or charge llmrefund points.
			return &RefillQuote{Service: "refill", Plan: plan, Enabled: true}, nil
		}
	}
	return nil, invalidManualRefill("粉嘟嘟所选套餐不可用或次数不足（已扣除本地待提交预占）")
}

func (s *AccountRefillService) fenduduReserved(ctx context.Context, state *accountRefillState, plan string) (int, error) {
	count := 0
	add := func(other accountRefillState) {
		if other.Fendudu.TokenHash != state.Fendudu.TokenHash {
			return
		}
		for _, j := range other.Jobs {
			if j.Provider == refillProviderFendudu && j.Plan == plan && j.pending() && j.SupplierID == "" {
				count++
			}
		}
	}
	add(*state)
	if stores, ok := s.rootService().store.(AccountRefillGroupStore); ok {
		rows, err := stores.ListRules(ctx)
		if err != nil {
			return 0, invalidManualRefill("无法核对其他分组的粉嘟嘟次数预占")
		}
		for id, raw := range rows {
			if id == s.ruleGroupID {
				continue
			}
			other, err := decodeAccountRefillState(raw)
			if err != nil {
				return 0, err
			}
			add(other)
		}
	}
	return count, nil
}

// ReconcileFendudu binds a supplier-confirmed order after a lost POST response.
// It makes read requests only and rejects orders already assigned to any rule.
func (s *AccountRefillService) ReconcileFendudu(ctx context.Context, jobID, supplierID string) error {
	if !validFenduduIdentifier(supplierID, false) {
		return errors.New("粉嘟嘟订单 ID 无效")
	}
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		state, err := decodeAccountRefillState(raw)
		if err != nil {
			return err
		}
		for i := range state.Jobs {
			j := &state.Jobs[i]
			if j.ID != jobID || j.Provider != refillProviderFendudu {
				continue
			}
			if j.SupplierID == supplierID {
				return nil
			}
			if j.SupplierID != "" || !j.pending() || j.SubmissionStartedAt == nil {
				return errors.New("此任务不能关联新订单")
			}
			if time.Now().Before(state.Fendudu.RetryAt) {
				return errors.New("粉嘟嘟正在退避，请稍后再试")
			}
			if err := s.fenduduOrderUnclaimed(ctx, state.Jobs, j.ID, supplierID); err != nil {
				return err
			}
			token, err := s.encryptor.Decrypt(state.Fendudu.EncryptedToken)
			if err != nil {
				return errors.New("粉嘟嘟对接码解密失败")
			}
			cycle, err := s.fenduduClient().Cycle(ctx, token, supplierID)
			if err != nil {
				return errors.New("无法核对粉嘟嘟原订单")
			}
			if err := validateFenduduCycle(cycle, *j); err != nil {
				return err
			}
			j.SupplierID, j.Status, j.Error = supplierID, "processing", ""
			j.NextAttemptAt = time.Now()
			return saveRefillState(&state, save)
		}
		return errors.New("粉嘟嘟任务不存在")
	})
}

func (s *AccountRefillService) fenduduOrderUnclaimed(ctx context.Context, jobs []AccountRefillJob, localID, supplierID string) error {
	check := func(jobs []AccountRefillJob) error {
		for _, j := range jobs {
			if j.Provider == refillProviderFendudu && j.ID != localID && j.SupplierID == supplierID {
				return errors.New("粉嘟嘟订单已关联其他任务")
			}
		}
		return nil
	}
	if err := check(jobs); err != nil {
		return err
	}
	if stores, ok := s.rootService().store.(AccountRefillGroupStore); ok {
		rows, err := stores.ListRules(ctx)
		if err != nil {
			return err
		}
		for id, raw := range rows {
			if id == s.ruleGroupID {
				continue
			}
			state, err := decodeAccountRefillState(raw)
			if err != nil {
				return err
			}
			if err := check(state.Jobs); err != nil {
				return err
			}
		}
	}
	return nil
}
