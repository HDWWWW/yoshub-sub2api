package service

import (
	"context"
	"math"
	"time"
)

func (s *AccountRefillService) manualRefillQuote(ctx context.Context, state *accountRefillState, plan string, persist func() error) (*RefillQuote, error) {
	now := time.Now()
	c := state.Config
	if state.EncryptedToken == "" {
		return nil, invalidManualRefill("请先配置号商 API Key")
	}
	token, err := s.encryptor.Decrypt(state.EncryptedToken)
	if err != nil {
		return nil, invalidManualRefill("密钥解密失败，请重新保存 API Key")
	}
	providerFailure := func(err error) error {
		state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
		if err := persist(); err != nil {
			return err
		}
		return invalidManualRefill("无法读取号商报价或余额，请稍后重试")
	}
	catalog, err := s.provider.Catalog(ctx, token)
	if err != nil || catalog == nil {
		return nil, providerFailure(err)
	}
	wallet, key, err := s.provider.Identity(ctx, token)
	if err != nil || wallet == nil || key == nil {
		return nil, providerFailure(err)
	}
	state.Wallet, state.Key, state.IdentityAt = wallet, key, &now
	if err := s.notifyRefillBalance(ctx, state, wallet.Available, 0, persist); err != nil {
		return nil, err
	}
	if key.Revoked || (!key.ExpiresAt.IsZero() && !key.ExpiresAt.After(time.Now())) {
		return nil, invalidManualRefill("号商 API Key 已失效，请替换")
	}
	var quote *RefillQuote
	for i := range catalog.Items {
		q := &catalog.Items[i]
		if q.Service == "refill" && q.Plan == plan && q.Enabled {
			quote = q
			break
		}
	}
	if quote == nil {
		return nil, invalidManualRefill("所选补号套餐暂不可用")
	}
	if math.IsNaN(quote.Price) || math.IsInf(quote.Price, 0) || quote.Price < 0 || quote.Price > c.MaxUnitPrice {
		return nil, invalidManualRefill("当前单价超过允许上限或报价无效，请检查补号规则")
	}
	unsubmitted, reserveErr := s.unsubmittedPoints(ctx, state)
	if reserveErr != nil {
		return nil, invalidManualRefill(reserveErr.Error())
	}
	if math.IsNaN(catalog.Available) || math.IsNaN(wallet.Available) || math.Min(catalog.Available, wallet.Available)+1e-8 < quote.Price+unsubmitted {
		if err := s.notifyRefillBalance(ctx, state, math.Min(catalog.Available, wallet.Available), quote.Price+unsubmitted, persist); err != nil {
			return nil, err
		}
		return nil, invalidManualRefill("号商可用积分不足（已扣除本地在途预占）")
	}
	state.Catalog, state.Wallet, state.Key = catalog, wallet, key
	state.CatalogAt, state.IdentityAt = &now, &now
	return quote, nil

}
