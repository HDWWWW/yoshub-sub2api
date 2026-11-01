package service

import (
	"context"
	"maps"
)

// Each account gets its own allocation and transaction. A failed item retains
// its original proxy and settings; successful siblings are reported explicitly.
func (s *adminServiceImpl) bulkUpdateWithAutomaticProxy(ctx context.Context, input *BulkUpdateAccountsInput, updates AccountBulkUpdate, result *BulkUpdateAccountsResult) (*BulkUpdateAccountsResult, error) {
	seen := make(map[int64]bool, len(input.AccountIDs))
	for _, id := range input.AccountIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		_, err := s.scopedAdminTransaction(ctx, id, false, func(txctx context.Context) (*Account, error) {
			account, err := s.accountRepo.GetByID(txctx, id)
			if err != nil {
				return nil, err
			}
			if account.IsCredentialShadow() {
				return nil, ErrAccountNotFound
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
			account.ProxyID = proxyID
			return account, nil
		})
		entry := BulkUpdateAccountResult{AccountID: id, Success: err == nil}
		if err != nil {
			entry.Error = err.Error()
			result.Failed++
			result.FailedIDs = append(result.FailedIDs, id)
		} else {
			result.Success++
			result.SuccessIDs = append(result.SuccessIDs, id)
		}
		result.Results = append(result.Results, entry)
	}
	return result, nil
}
