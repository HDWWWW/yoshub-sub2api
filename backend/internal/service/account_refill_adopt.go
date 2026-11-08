package service

import (
	"context"
	"errors"
	"maps"
	"strings"
)

// AdoptRefillAccount applies the common import input to a supplier-pushed
// account. The existing account, exclusive proxy allocation, expiry metadata
// and recovery marker commit together; the caller activates it afterwards.
func (s *adminServiceImpl) AdoptRefillAccount(ctx context.Context, id int64, input *CreateAccountInput) (*Account, error) {
	return s.scopedAdminTransaction(ctx, id, false, func(txctx context.Context) (*Account, error) {
		account, err := s.accountRepo.GetByID(txctx, id)
		if err != nil {
			return nil, err
		}
		email, _ := account.Credentials["email"].(string)
		expected, _ := input.Credentials["email"].(string)
		marker, _ := input.Extra["refill_job_id"].(string)
		if !account.IsOpenAIOAuth() || account.IsShadow() || marker == "" || expected == "" || !strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(expected)) {
			return nil, errors.New("推送账号身份不匹配")
		}
		if old, _ := account.Extra["refill_job_id"].(string); old != "" {
			if old != marker {
				return nil, errors.New("账号已关联其他补号任务")
			}
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
