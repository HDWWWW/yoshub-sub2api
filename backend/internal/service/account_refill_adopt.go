package service

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"strings"
	"time"
)

// SyncFenduduAccountLease also repairs identity lost by an older edit form.
// Verify the delivered account under the same lock as the metadata write;
// never reapply import settings, allocate proxies, or reactivate the account.
func (s *adminServiceImpl) SyncFenduduAccountLease(ctx context.Context, j AccountRefillJob, cycle *fenduduCycle, now time.Time) (*int64, error) {
	if j.Provider != refillProviderFendudu || j.ID == "" || j.AccountID <= 0 || (j.Status != "imported" && j.Status != "delivery_expired") {
		return nil, errors.New("粉嘟嘟账号尚未完成交付核对")
	}
	if err := validateFenduduCycle(cycle, j); err != nil {
		return nil, err
	}
	if !cycle.Pushed || cycle.AccountID != strconv.FormatInt(j.AccountID, 10) {
		return nil, errors.New("粉嘟嘟订单与已交付账号不匹配")
	}
	var expires *int64
	_, err := s.scopedAdminTransaction(ctx, j.AccountID, false, func(txctx context.Context) (*Account, error) {
		a, err := s.accountRepo.GetByID(txctx, j.AccountID)
		if err != nil {
			return nil, err
		}
		if err := validateFenduduPushedAccount(a, &j, cycle, now); err != nil {
			return nil, err
		}
		a.Extra = maps.Clone(a.Extra)
		if a.Extra == nil {
			a.Extra = map[string]any{}
		}
		a.Extra["import_source"] = "fendudu_api"
		expires = fenduduSourceExpiry(cycle, a)
		updates := fenduduLeaseUpdates(&j, a, *expires, now)
		updates["refill_job_id"], updates["refill_supplier_id"] = j.ID, j.SupplierID
		updates["refill_plan"], updates["import_source"] = j.Plan, "fendudu_api"
		if err := s.accountRepo.UpdateExtra(txctx, a.ID, updates); err != nil {
			return nil, err
		}
		return s.accountRepo.GetByID(txctx, a.ID)
	})
	return expires, err
}

func validateRefillAdoption(account *Account, input *CreateAccountInput) error {
	email, _ := account.Credentials["email"].(string)
	expected, _ := input.Credentials["email"].(string)
	marker, _ := input.Extra["refill_job_id"].(string)
	if !account.IsOpenAIOAuth() || account.IsShadow() || marker == "" || expected == "" || !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(expected)) {
		return errors.New("推送账号身份不匹配")
	}
	if old, _ := account.Extra["refill_job_id"].(string); old != "" && old != marker {
		return errors.New("账号已关联其他补号任务")
	}
	return nil
}

// AdoptRefillAccount applies the common import input to a supplier-pushed
// account. The existing account, exclusive proxy allocation, expiry metadata
// and recovery marker commit together; the caller activates it afterwards.
func (s *adminServiceImpl) AdoptRefillAccount(ctx context.Context, id int64, input *CreateAccountInput) (*Account, error) {
	return s.scopedAdminTransaction(ctx, id, false, func(txctx context.Context) (*Account, error) {
		account, err := s.accountRepo.GetByID(txctx, id)
		if err != nil {
			return nil, err
		}
		if err := validateRefillAdoption(account, input); err != nil {
			return nil, err
		}
		if old, _ := account.Extra["refill_job_id"].(string); old != "" {
			return account, nil
		}
		extra := maps.Clone(account.Extra)
		if extra == nil {
			extra = map[string]any{}
		}
		maps.Copy(extra, input.Extra)
		// False must clear a supplier default as well as enable true.
		extra["openai_responses_flatten_namespaces"] = input.Extra["openai_responses_flatten_namespaces"] == true
		credentials := maps.Clone(account.Credentials)
		for _, key := range SensitiveCredentialKeys {
			delete(credentials, key) // UpdateAccount preserves fresh tokens in the locked row.
		}
		delete(credentials, "model_mapping")
		if mapping, ok := input.Credentials["model_mapping"]; ok {
			credentials["model_mapping"] = mapping
		}
		load := 0
		if input.LoadFactor != nil {
			load = *input.LoadFactor
		}
		privacy := true
		if input.PrivacyEnabled != nil {
			privacy = *input.PrivacyEnabled
		}
		notes := ""
		if input.Notes != nil {
			notes = *input.Notes
		}
		if _, err = s.UpdateAccount(txctx, id, &UpdateAccountInput{
			Name: input.Name, Notes: &notes, Credentials: credentials, Extra: extra,
			Concurrency: &input.Concurrency, Priority: &input.Priority, RateMultiplier: input.RateMultiplier,
			LoadFactor: &load, PrivacyEnabled: &privacy, AutoPauseOnExpired: input.AutoPauseOnExpired,
			ProxyMode: input.ProxyMode, ProxyID: input.ProxyID, GroupIDs: &input.GroupIDs, Status: "inactive",
		}); err != nil {
			return nil, err
		}
		policy := map[string]any{}
		for key, value := range input.Extra {
			if IsAccountExpiryExtra(key) {
				policy[key] = value
			}
		}
		if err = s.accountRepo.UpdateExtra(txctx, id, policy); err != nil {
			return nil, err
		}
		if err = s.accountRepo.SetSchedulable(txctx, id, false); err != nil {
			return nil, err
		}
		return s.accountRepo.GetByID(txctx, id)
	})
}

// A lost receipt can be recovered after the supplier has already closed the
// lease. Record its identity and original deadline atomically without allocating
// another proxy, overwriting credentials, or reviving the expired account.
func (s *adminServiceImpl) RecordExpiredRefillAccount(ctx context.Context, id int64, input *CreateAccountInput) error {
	_, err := s.scopedAdminTransaction(ctx, id, false, func(txctx context.Context) (*Account, error) {
		account, err := s.accountRepo.GetByID(txctx, id)
		if err != nil {
			return nil, err
		}
		if err := validateRefillAdoption(account, input); err != nil {
			return nil, err
		}
		var earlierDeadline *int64
		if account.ExpiresAt != nil && input.ExpiresAt != nil && account.ExpiresAt.Unix() < *input.ExpiresAt {
			v := account.ExpiresAt.Unix()
			earlierDeadline = &v
		}
		updates := map[string]any{}
		for key, value := range input.Extra {
			if IsAccountExpiryExtra(key) || key == "import_source" || key == "refill_job_id" || key == "refill_supplier_id" || key == "refill_plan" {
				updates[key] = value
			}
		}
		if err := s.accountRepo.UpdateExtra(txctx, id, updates); err != nil {
			return nil, err
		}
		if _, err := s.UpdateAccount(txctx, id, &UpdateAccountInput{Status: "inactive", ExpiresAt: earlierDeadline}); err != nil {
			return nil, err
		}
		if err := s.accountRepo.SetSchedulable(txctx, id, false); err != nil {
			return nil, err
		}
		return s.accountRepo.GetByID(txctx, id)
	})
	return err
}
