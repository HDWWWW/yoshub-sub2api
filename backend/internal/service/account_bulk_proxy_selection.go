package service

import (
	"context"
	"maps"
	"sync"
	"sync/atomic"
)

const automaticProxyBulkConcurrency = 8

// Each account gets its own allocation and transaction. A failed item retains
// its original proxy and settings; successful siblings are reported explicitly.
func (s *adminServiceImpl) bulkUpdateWithAutomaticProxy(ctx context.Context, input *BulkUpdateAccountsInput, updates AccountBulkUpdate, result *BulkUpdateAccountsResult) (*BulkUpdateAccountsResult, error) {
	seen := make(map[int64]bool, len(input.AccountIDs))
	ids := make([]int64, 0, len(input.AccountIDs))
	for _, id := range input.AccountIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	entries := make([]BulkUpdateAccountResult, len(ids))
	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(automaticProxyBulkConcurrency, len(ids)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(ids) {
					return
				}
				entries[i] = s.bulkUpdateOneWithAutomaticProxy(ctx, ids[i], input, updates)
			}
		}()
	}
	workers.Wait()
	// Stable input order makes partial success and retry targets unambiguous.
	for _, entry := range entries {
		if !entry.Success {
			result.Failed++
			result.FailedIDs = append(result.FailedIDs, entry.AccountID)
		} else {
			result.Success++
			result.SuccessIDs = append(result.SuccessIDs, entry.AccountID)
		}
		result.Results = append(result.Results, entry)
	}
	return result, nil
}

func (s *adminServiceImpl) bulkUpdateOneWithAutomaticProxy(ctx context.Context, id int64, input *BulkUpdateAccountsInput, updates AccountBulkUpdate) BulkUpdateAccountResult {
	entry := BulkUpdateAccountResult{AccountID: id}
	if err := ctx.Err(); err != nil {
		entry.Error = err.Error()
		return entry
	}
	var binding *Proxy
	_, err := s.scopedAdminTransaction(ctx, id, false, func(txctx context.Context) (*Account, error) {
		account, err := s.accountRepo.GetByID(txctx, id)
		if err != nil {
			return nil, err
		}
		if account.IsCredentialShadow() {
			return nil, ErrAccountNotFound
		}
		if err = txctx.Err(); err != nil {
			return nil, err
		}
		proxyID, err := s.scopedAccountProxySelection(txctx, input.ProxyMode, nil)
		if err != nil {
			return nil, err
		}
		item := updates
		item.Extra, item.Credentials, item.ProxyID = maps.Clone(updates.Extra), maps.Clone(updates.Credentials), proxyID
		if _, err = s.accountRepo.BulkUpdate(txctx, []int64{id}, item); err != nil {
			return nil, err
		}
		if input.GroupIDs != nil {
			if err = s.accountRepo.BindGroups(txctx, id, *input.GroupIDs); err != nil {
				return nil, err
			}
		}
		if err = s.propagateProxyToShadows(txctx, id, proxyID); err != nil {
			return nil, err
		}
		binding = &Proxy{ID: *proxyID}
		if err = s.attachManagedIPv6(txctx, binding); err != nil {
			return nil, err
		}
		account.ProxyID = proxyID
		return account, nil
	})
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	entry.Success = true
	entry.ProxyID, entry.ManagedIPv6 = binding.ID, binding.ManagedIPv6
	return entry
}
