package service

import (
	"context"
	"fmt"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type ProxyBindingLimit struct {
	ProxyID      int64 `json:"proxy_id"`
	MaxAccounts  int   `json:"max_accounts"`
	AccountCount int64 `json:"account_count"`
}

type ProxyBindingLimitsAdmin interface {
	GetProxyBindingLimits(context.Context, []int64) ([]ProxyBindingLimit, error)
	SetProxyBindingLimits(context.Context, []int64, int) error
}

func ValidateExtensionIDs(ids []int64) error {
	if len(ids) == 0 || len(ids) > 1000 {
		return infraerrors.BadRequest("INVALID_EXTENSION_IDS", "每次须选择 1–1000 条记录")
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return infraerrors.BadRequest("INVALID_EXTENSION_IDS", "ID 必须为不重复的正整数")
		}
		seen[id] = true
	}
	return nil
}

func (s *adminServiceImpl) GetProxyBindingLimits(ctx context.Context, ids []int64) ([]ProxyBindingLimit, error) {
	if err := ValidateExtensionIDs(ids); err != nil {
		return nil, err
	}
	repo, ok := s.proxyRepo.(ProxyBindingLimitsAdmin)
	if !ok {
		return nil, fmt.Errorf("proxy binding limits unavailable")
	}
	return repo.GetProxyBindingLimits(ctx, ids)
}

func (s *adminServiceImpl) SetProxyBindingLimits(ctx context.Context, ids []int64, maxAccounts int) error {
	if err := ValidateExtensionIDs(ids); err != nil {
		return err
	}
	if maxAccounts < 1 || maxAccounts > 100000 {
		return infraerrors.BadRequest("INVALID_PROXY_BINDING_LIMIT", "绑定上限须为 1–100000 的整数")
	}
	repo, ok := s.proxyRepo.(ProxyBindingLimitsAdmin)
	if !ok {
		return fmt.Errorf("proxy binding limits unavailable")
	}
	return repo.SetProxyBindingLimits(ctx, ids, maxAccounts)
}
