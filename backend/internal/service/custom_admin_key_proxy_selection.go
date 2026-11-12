package service

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// NormalizeAccountProxyMode 显式模式与旧 proxy_id/random_proxy 互斥，省略时保留原绑定语义。
func NormalizeAccountProxyMode(mode string, id *int64, random bool) (string, error) {
	if id != nil && *id < 0 {
		return "", infraerrors.BadRequest("PROXY_SELECTION_INVALID", "代理 ID 无效")
	}
	if random {
		if mode != "" && mode != "random" {
			return "", infraerrors.BadRequest("PROXY_SELECTION_CONFLICT", "代理选择参数冲突")
		}
		mode = "random"
	}
	switch mode {
	case "":
		return "", nil
	case "specified":
		if id == nil || *id <= 0 {
			return "", infraerrors.BadRequest("PROXY_SELECTION_INVALID", "指定代理须提供正数 proxy_id")
		}
	case "direct":
		if id != nil && *id != 0 {
			return "", infraerrors.BadRequest("PROXY_SELECTION_CONFLICT", "直连模式不能同时指定代理")
		}
	case "random", "least_used", "ovh_ipv6":
		if id != nil && *id != 0 {
			return "", infraerrors.BadRequest("PROXY_SELECTION_CONFLICT", "自动选择不能同时指定代理")
		}
	default:
		return "", infraerrors.BadRequest("PROXY_SELECTION_INVALID", "代理模式仅支持 direct、specified、random、least_used、ovh_ipv6")
	}
	return mode, nil
}

func (s *adminServiceImpl) scopedAccountProxySelection(ctx context.Context, mode string, id *int64) (*int64, error) {
	normalized, err := NormalizeAccountProxyMode(mode, id, false)
	if err != nil {
		return nil, err
	}
	switch normalized {
	case "", "specified":
		return id, nil
	case "direct":
		zero := int64(0)
		return &zero, nil
	}
	if dbent.TxFromContext(ctx) == nil {
		return nil, fmt.Errorf("自动代理选择需要账户写事务")
	}
	repo, ok := s.proxyRepo.(interface {
		SelectAccountProxy(context.Context, string) (int64, error)
	})
	if !ok {
		return nil, fmt.Errorf("自动代理选择服务不可用")
	}
	selected, err := repo.SelectAccountProxy(ctx, normalized)
	if err != nil {
		return nil, err
	}
	if selected <= 0 {
		return nil, infraerrors.BadRequest("PROXY_POOL_EMPTY", "没有启用、未过期且未达绑定上限的代理，请添加代理、提高上限或释放已有绑定后重试")
	}
	return &selected, nil
}

// CreateAccountWithProxySelection is the custom refill entry point. Selection,
// account creation and group bindings commit together, as in native and scoped
// account saves; the allocator never exposes an uncommitted account binding.
func (s *adminServiceImpl) CreateAccountWithProxySelection(ctx context.Context, input *CreateAccountInput) (*Account, error) {
	return s.scopedAdminCreateTransaction(ctx, input)
}

func (s *adminServiceImpl) createAccountProxyMode(ctx context.Context, input *CreateAccountInput) (*Account, error) {
	if _, err := NormalizeAccountProxyMode(input.ProxyMode, input.ProxyID, false); err != nil {
		return nil, err
	}
	if dbent.TxFromContext(ctx) == nil {
		return s.scopedAdminCreateTransaction(ctx, input)
	}
	safe := *input
	id, err := s.scopedAccountProxySelection(ctx, safe.ProxyMode, safe.ProxyID)
	if err != nil {
		return nil, err
	}
	safe.ProxyID, safe.ProxyMode = id, ""
	if id != nil && *id == 0 {
		safe.ProxyID = nil
	}
	return s.CreateAccount(ctx, &safe)
}

func (s *adminServiceImpl) needsProxyWriteTransaction(ctx context.Context, proxyID *int64) bool {
	return proxyID != nil && s.entClient != nil && dbent.TxFromContext(ctx) == nil
}

func (s *adminServiceImpl) createDuplicateWithFreshIPv6(ctx context.Context, account *Account, groups []AccountGroup) error {
	if s.entClient == nil {
		return fmt.Errorf("OVH IPv6 复制需要账号写事务")
	}
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	txctx := dbent.NewTxContext(ctx, tx)
	id, err := s.scopedAccountProxySelection(txctx, "ovh_ipv6", nil)
	if err != nil {
		return err
	}
	account.ProxyID = id
	if err = s.accountDuplicateRepo.CreateWithAccountGroups(txctx, account, groups); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *adminServiceImpl) updateAccountProxyMode(ctx context.Context, id int64, input *UpdateAccountInput) (*Account, error) {
	if _, err := NormalizeAccountProxyMode(input.ProxyMode, input.ProxyID, false); err != nil {
		return nil, err
	}
	run := func(txctx context.Context) (*Account, error) {
		account, err := s.accountRepo.GetByID(txctx, id)
		if err != nil {
			return nil, err
		}
		if account.IsCredentialShadow() {
			if input.ProxyMode != "" {
				return nil, infraerrors.BadRequest("SPARK_SHADOW_PROXY_INHERITED", "影子账号的代理继承母账号，请修改母账号")
			}
			// Legacy edit/re-auth forms include the inherited proxy ID. Preserve
			// their existing behavior: ignore that field and save other changes.
			safe := *input
			safe.ProxyID = nil
			return s.UpdateAccount(txctx, id, &safe)
		}
		safe := *input
		safe.ProxyID, err = s.scopedAccountProxySelection(txctx, safe.ProxyMode, safe.ProxyID)
		if err != nil {
			return nil, err
		}
		safe.ProxyMode = ""
		return s.UpdateAccount(txctx, id, &safe)
	}
	if dbent.TxFromContext(ctx) != nil {
		return run(ctx)
	}
	return s.scopedAdminTransaction(ctx, id, false, run)
}
