package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The public key API can look up API-created jobs only. Website orders return
// API order not found; their imported lease snapshot must be kept independently.
func (s *AccountRefillService) syncSourceLeases(ctx context.Context, token string, state *accountRefillState, now time.Time, force bool) {
	if !force && state.LeaseSyncAt != nil && now.Sub(*state.LeaseSyncAt) < 5*time.Minute {
		return
	}
	state.LeaseSyncAt = &now
	writer, ok := s.accounts.(interface {
		UpdateExtra(context.Context, int64, map[string]any) error
	})
	if !ok {
		return
	}
	var eligible []int
	for i, j := range state.Jobs {
		if j.Status == "imported" && j.AccountID > 0 && j.SupplierID != "" {
			eligible = append(eligible, i)
		}
	}
	if len(eligible) == 0 {
		state.LeaseSyncMessage = "暂无可由 Key 同步的 API 订单；网站推送账号使用导入时记录的租期"
		return
	}
	synced, failed := 0, 0
	for n := 0; n < len(eligible) && n < 5; n++ {
		index := (state.LeaseSyncCursor + n) % len(eligible)
		j := &state.Jobs[eligible[index]]
		matches, matchErr := s.accounts.FindByExtraField(ctx, "refill_job_id", j.ID)
		if matchErr != nil || len(matches) != 1 || matches[0].ID != j.AccountID {
			failed++
			continue
		}
		remote, err := s.provider.Status(ctx, token, j.SupplierID)
		var providerErr *refillProviderError
		if errors.As(err, &providerErr) && providerErr.code == 429 {
			state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
			failed++
			break
		}
		if err != nil || remote == nil || remote.ID != j.SupplierID || remote.ExpiresAt == nil || *remote.ExpiresAt <= 0 || *remote.ExpiresAt > 253402214399 {
			failed++
			if ctx.Err() != nil {
				break
			}
			continue
		}
		updates := map[string]any{AccountSourceExpiryExtraKey: *remote.ExpiresAt, AccountSourceExpiryOriginExtraKey: "supplier_api", AccountSourceExpiryCheckedExtraKey: now.UTC().Format(time.RFC3339)}
		if err := writer.UpdateExtra(ctx, j.AccountID, updates); err != nil {
			failed++
			continue
		}
		j.ExpiresAt = remote.ExpiresAt
		synced++
	}
	state.LeaseSyncCursor = (state.LeaseSyncCursor + 5) % len(eligible)
	state.LeaseSyncMessage = fmt.Sprintf("API 订单租期同步：成功 %d，未更新 %d；网站推送账号使用导入租期", synced, failed)
}
