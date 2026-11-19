package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
)

const (
	initialOpenAIImportThreshold    = 99
	settingKeyOpenAIImportThreshold = "openai_import_scheduling_threshold"
)

func readOpenAIImportThreshold(ctx context.Context, settings SettingRepository) (int, error) {
	raw, err := settings.GetValue(ctx, settingKeyOpenAIImportThreshold)
	if errors.Is(err, ErrSettingNotFound) {
		return initialOpenAIImportThreshold, nil
	}
	if err != nil {
		return 0, err
	}
	threshold, err := strconv.Atoi(raw)
	if err != nil || threshold < 1 || threshold > 100 {
		return 0, fmt.Errorf("invalid stored OpenAI import scheduling threshold")
	}
	return threshold, nil
}

func (s *SettingService) GetOpenAIImportThreshold(ctx context.Context) (int, error) {
	return readOpenAIImportThreshold(ctx, s.settingRepo)
}

func (s *SettingService) SetOpenAIImportThreshold(ctx context.Context, threshold int) error {
	if threshold < 1 || threshold > 100 {
		return fmt.Errorf("新导入账号停调阈值须为 1–100 的整数")
	}
	return s.settingRepo.Set(ctx, settingKeyOpenAIImportThreshold, strconv.Itoa(threshold))
}

// ApplyNewAccountQuotaPauseDefaults snapshots the site default at creation only.
// Explicit values (including 100 to disable) and legacy per-window rules are preserved.
func ApplyNewAccountQuotaPauseDefaults(ctx context.Context, account *Account, settings SettingRepository) error {
	if account == nil || account.Platform != PlatformOpenAI || account.Credentials[accountSchedulingThresholdCredentialKey] != nil {
		return nil
	}
	threshold, err := readOpenAIImportThreshold(ctx, settings)
	if err != nil {
		return err
	}
	credentials := maps.Clone(account.Credentials)
	if credentials == nil {
		credentials = make(map[string]any, 1)
	}
	credentials[accountSchedulingThresholdCredentialKey] = threshold
	account.Credentials = credentials
	return nil
}
