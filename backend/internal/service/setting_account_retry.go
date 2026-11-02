package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
)

const SettingKeyDefaultAccountRetryCount = "default_account_retry_count"

func (s *SettingService) GetDefaultAccountRetryCount(ctx context.Context) (int, error) {
	if s == nil || s.settingRepo == nil {
		return defaultPoolModeRetryCount, nil
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyDefaultAccountRetryCount)
	if errors.Is(err, ErrSettingNotFound) || (err == nil && raw == "") {
		return defaultPoolModeRetryCount, nil
	}
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(raw)
	if err != nil || count < 0 || count > maxPoolModeRetryCount {
		return 0, fmt.Errorf("invalid default account retry count")
	}
	return count, nil
}

func (s *SettingService) SetDefaultAccountRetryCount(ctx context.Context, count int) error {
	if count < 0 || count > maxPoolModeRetryCount {
		return fmt.Errorf("同账号重试次数须为 0–10")
	}
	if s == nil || s.settingRepo == nil {
		return fmt.Errorf("account retry settings unavailable")
	}
	return s.settingRepo.Set(ctx, SettingKeyDefaultAccountRetryCount, strconv.Itoa(count))
}

// Snapshot the saved default for new/imported accounts only. Explicit zero and
// legacy pool-mode overrides must survive imports and subsequent default edits.
func (s *SettingService) applyDefaultAccountRetryCount(ctx context.Context, input *CreateAccountInput) error {
	if input.Extra["same_account_retry_count"] != nil {
		return nil
	}
	account := &Account{Type: input.Type, Credentials: input.Credentials}
	if account.IsPoolMode() && input.Credentials["pool_mode_retry_count"] != nil {
		return nil
	}
	count, err := s.GetDefaultAccountRetryCount(ctx)
	if err != nil {
		return err
	}
	input.Extra = maps.Clone(input.Extra)
	if input.Extra == nil {
		input.Extra = map[string]any{}
	}
	input.Extra["same_account_retry_count"] = count
	return nil
}
