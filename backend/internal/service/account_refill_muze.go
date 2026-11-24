package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

type muzeRefillState struct {
	EncryptedToken  string        `json:"encrypted_token,omitempty"`
	TokenHash       string        `json:"token_hash,omitempty"`
	ConnectionID    string        `json:"connection_id"`
	Products        []MuzeProduct `json:"products"`
	CheckedAt       *time.Time    `json:"checked_at"`
	RetryAt         time.Time     `json:"retry_at"`
	LeaseSyncCursor int           `json:"lease_sync_cursor,omitempty"`
}

type MuzeRefillView struct {
	TokenConfigured bool          `json:"token_configured"`
	ConnectionID    string        `json:"connection_id"`
	Products        []MuzeProduct `json:"products"`
	CheckedAt       *time.Time    `json:"checked_at"`
}

func (f muzeRefillState) view() MuzeRefillView {
	return MuzeRefillView{TokenConfigured: f.EncryptedToken != "", ConnectionID: f.ConnectionID, Products: f.Products, CheckedAt: f.CheckedAt}
}

func (s *AccountRefillService) muzeClient() muzeProvider {
	if s.muze != nil {
		return s.muze
	}
	return newHTTPMuzeProvider()
}

// An installation may supply a default credential for existing and new rules.
// Snapshot its ciphertext and wallet hash with the first reservation. Explicit
// group credentials win, and an in-flight order keeps its original wallet.
type muzeDefaultStore interface {
	ReadMuzeDefaultToken(context.Context) (string, error)
}

func (s *AccountRefillService) decodeRefillWithMuze(ctx context.Context, raw string) (accountRefillState, error) {
	state, err := decodeAccountRefillState(raw)
	if err != nil || state.Muze.EncryptedToken != "" {
		return state, err
	}
	store, ok := s.rootService().store.(muzeDefaultStore)
	if !ok {
		return state, nil
	}
	encrypted, err := store.ReadMuzeDefaultToken(ctx)
	if err != nil || encrypted == "" {
		return state, err
	}
	token, err := s.encryptor.Decrypt(encrypted)
	if err != nil || !muzeTokenPattern.MatchString(token) {
		return state, errors.New("无法读取 Muze 默认 API Key")
	}
	hash := sha256.Sum256([]byte(token))
	state.Muze.EncryptedToken = encrypted
	state.Muze.TokenHash = hex.EncodeToString(hash[:])
	return state, nil
}

// ConfigureMuze changes only the Muze connection. Blank tokens keep the
// encrypted secret; no API response contains the secret or its hash.
func (s *AccountRefillService) ConfigureMuze(ctx context.Context, token, connectionID string, revision int64) error {
	token, connectionID = strings.TrimSpace(token), strings.TrimSpace(connectionID)
	if token != "" && !muzeTokenPattern.MatchString(token) {
		return errors.New("muze对接码格式无效")
	}
	if !validMuzeIdentifier(connectionID, true) {
		return errors.New("muze推送连接 ID 无效")
	}
	return withAccountRefillConfigLock(ctx, s.store, 30*time.Second, func(raw string, save func(string) error) error {
		state, err := s.decodeRefillWithMuze(ctx, raw)
		if err != nil {
			return err
		}
		if revision != state.Revision {
			return errors.New("配置已被其他页面修改，请刷新后再保存")
		}
		secret := token
		if secret == "" && state.Muze.EncryptedToken != "" {
			secret, err = s.encryptor.Decrypt(state.Muze.EncryptedToken)
		}
		if err != nil || secret == "" {
			return errors.New("请填写有效的muze对接码")
		}
		digest := sha256.Sum256([]byte(secret))
		hash := hex.EncodeToString(digest[:])
		for _, j := range state.Jobs {
			if j.Provider == refillProviderMuze && j.pending() && (hash != state.Muze.TokenHash || connectionID != state.Muze.ConnectionID) {
				return errors.New("muze仍有在途订单，请先处理完成再更换对接码或连接")
			}
		}
		if time.Now().Before(state.Muze.RetryAt) {
			return errors.New("muze正在退避，请稍后再试")
		}
		if _, err := s.muzeClient().ResolveConnection(ctx, secret, connectionID); err != nil {
			return err
		}
		products, err := s.muzeClient().Products(ctx, secret)
		if err != nil {
			state.Muze.RetryAt = time.Now().Add(refillRetryDelay(err, 1))
			if saveErr := saveRefillState(&state, save); saveErr != nil {
				return saveErr
			}
			return errors.New("无法读取muze产品次数，请检查对接码及服务状态")
		}
		cipher, err := s.encryptor.Encrypt(secret)
		if err != nil {
			return errors.New("muze对接码加密失败")
		}
		now := time.Now()
		state.Muze = muzeRefillState{EncryptedToken: cipher, TokenHash: hash, ConnectionID: connectionID, Products: products, CheckedAt: &now}
		state.Revision++
		return saveRefillState(&state, save)
	})
}

func validMuzeIdentifier(value string, empty bool) bool {
	if value == "" {
		return empty
	}
	if len(value) > 128 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func (s *AccountRefillService) refreshMuzeProducts(ctx context.Context, state *accountRefillState, now time.Time, force bool, persist func() error) error {
	f := &state.Muze
	if f.EncryptedToken == "" || now.Before(f.RetryAt) || (!force && f.CheckedAt != nil && now.Sub(*f.CheckedAt) < 5*time.Minute) {
		return nil
	}
	secret, err := s.encryptor.Decrypt(f.EncryptedToken)
	if err != nil {
		f.RetryAt = now.Add(5 * time.Minute)
		return persist()
	}
	products, err := s.muzeClient().Products(ctx, secret)
	if err != nil {
		f.RetryAt = now.Add(refillRetryDelay(err, 1))
		return persist()
	}
	f.Products, f.CheckedAt, f.RetryAt = products, &now, time.Time{}
	return persist()
}

// Quote all requested plans against one inventory snapshot, subtracting shared
// unsubmitted reservations. Resolve the push destination before reserving.
func (s *AccountRefillService) muzePurchaseQuote(ctx context.Context, state *accountRefillState, plans []string, persist func() error) (string, error) {
	if state.Muze.EncryptedToken == "" {
		return "", invalidManualRefill("请先配置 Muze API Key")
	}
	if time.Now().Before(state.Muze.RetryAt) {
		return "", invalidManualRefill("Muze 正在退避，请稍后重试")
	}
	token, err := s.encryptor.Decrypt(state.Muze.EncryptedToken)
	if err != nil {
		return "", invalidManualRefill("Muze API Key 解密失败")
	}
	client := s.muzeClient()
	products, err := client.Products(ctx, token)
	if err != nil {
		state.Muze.RetryAt = time.Now().Add(refillRetryDelay(err, 1))
		if err := persist(); err != nil {
			return "", err
		}
		return "", invalidManualRefill("无法读取 Muze 产品次数")
	}
	now := time.Now()
	state.Muze.Products, state.Muze.CheckedAt = products, &now
	if err := persist(); err != nil {
		return "", err
	}
	counts := map[string]int{}
	for _, plan := range plans {
		counts[plan]++
	}
	for plan, count := range counts {
		reserved, err := s.muzeReserved(ctx, state, plan)
		if err != nil {
			return "", err
		}
		available := 0
		for _, item := range products {
			if item.Product == plan && !item.Expired && item.BlockedReason == "" {
				available = item.AvailableNow
				break
			}
		}
		if available < count {
			return "", supplyUnavailable("Muze 所选套餐库存或次数不足")
		}
		if available-reserved < count {
			return "", invalidManualRefill("Muze 所选套餐库存或次数不足（含本地预占）")
		}
	}
	id, err := client.ResolveConnection(ctx, token, state.Muze.ConnectionID)
	if err != nil {
		var remote *refillProviderError
		if errors.As(err, &remote) && remote.code == 429 {
			state.Muze.RetryAt = now.Add(refillRetryDelay(err, 1))
			if err := persist(); err != nil {
				return "", err
			}
		}
		return "", invalidManualRefill("无法确认 Muze 推送连接，请检查已启用的 Sub2API 连接")
	}
	return id, nil
}

func (s *AccountRefillService) muzeReserved(ctx context.Context, state *accountRefillState, plan string) (int, error) {
	count := 0
	add := func(other accountRefillState) {
		if other.Muze.TokenHash != state.Muze.TokenHash {
			return
		}
		for _, j := range other.Jobs {
			if j.Provider == refillProviderMuze && j.Plan == plan && j.pending() && j.SupplierID == "" {
				count++
			}
		}
	}
	add(*state)
	if stores, ok := s.rootService().store.(AccountRefillGroupStore); ok {
		rows, err := stores.ListRules(ctx)
		if err != nil {
			return 0, invalidManualRefill("无法核对其他分组的muze次数预占")
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

// ReconcileMuze binds a supplier-confirmed order after a lost POST response.
// It makes read requests only and rejects orders already assigned to any rule.
func (s *AccountRefillService) ReconcileMuze(ctx context.Context, jobID, supplierID string) error {
	if !validMuzeIdentifier(supplierID, false) {
		return errors.New("muze订单 ID 无效")
	}
	return s.store.WithLock(ctx, func(raw string, save func(string) error) error {
		state, err := s.decodeRefillWithMuze(ctx, raw)
		if err != nil {
			return err
		}
		for i := range state.Jobs {
			j := &state.Jobs[i]
			if j.ID != jobID || j.Provider != refillProviderMuze {
				continue
			}
			if j.SupplierID == supplierID {
				return nil
			}
			if j.SupplierID != "" || !j.pending() || j.SubmissionStartedAt == nil {
				return errors.New("此任务不能关联新订单")
			}
			if time.Now().Before(state.Muze.RetryAt) {
				return errors.New("muze正在退避，请稍后再试")
			}
			if err := s.muzeOrderUnclaimed(ctx, state.Jobs, j.ID, supplierID); err != nil {
				return err
			}
			token, err := s.encryptor.Decrypt(state.Muze.EncryptedToken)
			if err != nil {
				return errors.New("muze对接码解密失败")
			}
			cycle, err := s.muzeClient().Cycle(ctx, token, supplierID)
			if err != nil {
				return errors.New("无法核对muze原订单")
			}
			if err := validateMuzeCycle(cycle, *j); err != nil {
				return err
			}
			j.SupplierID, j.Status, j.Error = supplierID, "processing", ""
			j.SupplierStatus, j.SupplierRefundPhase = cycle.Status, cycle.RefundPhase
			if expires := cycle.expiresAt(); expires != nil {
				j.SupplierExpiresAt = expires
			}
			j.NextAttemptAt = time.Now()
			return saveRefillState(&state, save)
		}
		return errors.New("muze任务不存在")
	})
}

func (s *AccountRefillService) muzeOrderUnclaimed(ctx context.Context, jobs []AccountRefillJob, localID, supplierID string) error {
	check := func(jobs []AccountRefillJob) error {
		for _, j := range jobs {
			if j.Provider == refillProviderMuze && j.ID != localID && j.SupplierID == supplierID {
				return errors.New("muze订单已关联其他任务")
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
			state, err := s.decodeRefillWithMuze(ctx, raw)
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
