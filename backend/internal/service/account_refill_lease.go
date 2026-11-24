package service

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The public key API can look up API-created jobs only. Website orders return
// API order not found; their imported lease snapshot must be kept independently.
func (s *AccountRefillService) syncSourceLeases(ctx context.Context, token string, state *accountRefillState, now time.Time, force bool) bool {
	if !force && state.LeaseSyncAt != nil && now.Sub(*state.LeaseSyncAt) < 5*time.Minute {
		return false
	}
	state.LeaseSyncAt = &now
	writer, ok := s.accounts.(interface {
		UpdateExtra(context.Context, int64, map[string]any) error
	})
	if !ok {
		return true
	}
	var eligible, muze []int
	for i, j := range state.Jobs {
		if (j.Status == "imported" || (j.Provider == refillProviderMuze && j.Status == "delivery_expired")) && j.AccountID > 0 && j.SupplierID != "" {
			if j.Provider == refillProviderMuze {
				muze = append(muze, i)
			} else if j.Provider == "" {
				eligible = append(eligible, i)
			}
		}
	}
	if len(eligible)+len(muze) == 0 {
		state.LeaseSyncMessage = "暂无可由 Key 同步的 API 订单；网站推送账号使用导入时记录的租期"
		return true
	}
	// Give the backup supplier its own bounded rotation. Hundreds of old primary
	// orders must not postpone a freshly delivered Muze lease for hours.
	selected := make([]int, 0, 5)
	for n := 0; n < min(len(muze), 2); n++ {
		selected = append(selected, muze[(state.Muze.LeaseSyncCursor+n)%len(muze)])
	}
	primaryCount := min(len(eligible), 5-len(selected))
	for n := 0; n < primaryCount; n++ {
		selected = append(selected, eligible[(state.LeaseSyncCursor+n)%len(eligible)])
	}
	synced, failed := 0, 0
	for _, index := range selected {
		j := &state.Jobs[index]
		var remote *RefillProviderJob
		var err error
		if j.Provider == refillProviderMuze {
			if now.Before(state.Muze.RetryAt) {
				continue
			}
			var secret string
			secret, err = s.encryptor.Decrypt(state.Muze.EncryptedToken)
			if err == nil {
				var cycle *muzeCycle
				cycle, err = s.muzeClient().Cycle(ctx, secret, j.SupplierID)
				if err == nil {
					err = validateMuzeCycle(cycle, *j)
				}
				if err == nil {
					j.SupplierStatus, j.SupplierRefundPhase = cycle.Status, cycle.RefundPhase
					if at := cycle.expiresAt(); at != nil {
						j.SupplierExpiresAt = at
					}
					if syncer, ok := s.admin.(interface {
						SyncMuzeAccountLease(context.Context, AccountRefillJob, *muzeCycle, time.Time) (*int64, error)
					}); ok {
						var expires *int64
						expires, err = syncer.SyncMuzeAccountLease(ctx, *j, cycle, now)
						if err == nil {
							remote = &RefillProviderJob{ID: cycle.ID, ExpiresAt: expires}
							if native := cycle.expiresAt(); native != nil {
								j.SupplierExpiresAt = native
							}
						}
					} else {
						err = errors.New("muze租期核对服务不可用")
					}
				}
			}
		} else if token != "" {
			matches, matchErr := s.accounts.FindByExtraField(ctx, "refill_job_id", j.ID)
			if matchErr != nil || len(matches) != 1 || matches[0].ID != j.AccountID {
				failed++
				continue
			}
			remote, err = s.provider.Status(ctx, token, j.SupplierID)
		}
		var providerErr *refillProviderError
		if errors.As(err, &providerErr) && providerErr.code == 429 {
			if j.Provider == refillProviderMuze {
				state.Muze.RetryAt = now.Add(refillRetryDelay(err, 1))
			} else {
				state.ProviderRetryAt = now.Add(refillRetryDelay(err, 1))
			}
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
		if j.Provider != refillProviderMuze {
			j.SupplierExpiresAt = remote.ExpiresAt
			updates := map[string]any{AccountSourceExpiryExtraKey: *remote.ExpiresAt, AccountSourceExpiryOriginExtraKey: "supplier_api", AccountSourceExpiryCheckedExtraKey: now.UTC().Format(time.RFC3339)}
			if err := writer.UpdateExtra(ctx, j.AccountID, updates); err != nil {
				failed++
				continue
			}
		}
		j.ExpiresAt = remote.ExpiresAt
		synced++
	}
	if len(eligible) > 0 {
		state.LeaseSyncCursor = (state.LeaseSyncCursor + primaryCount) % len(eligible)
	}
	if len(muze) > 0 {
		state.Muze.LeaseSyncCursor = (state.Muze.LeaseSyncCursor + 2) % len(muze)
	}
	state.LeaseSyncMessage = fmt.Sprintf("API 订单租期同步：成功 %d，未更新 %d；网站推送账号使用导入租期", synced, failed)
	return true
}
