package service

import (
	"context"
	"errors"
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ExcelBPSSettings affects only accounts explicitly using an Excel OAuth grant.
type ExcelBPSSettings struct {
	Enabled bool                       `json:"enabled"`
	Images  ExcelBPSImageRelaySettings `json:"images"`
}

func (s *SettingService) GetExcelBPSSettings(ctx context.Context) (*ExcelBPSSettings, error) {
	images, err := s.GetExcelBPSImageRelaySettings(ctx)
	if err != nil {
		return nil, err
	}
	enabled := true
	if s != nil && s.settingRepo != nil {
		value, readErr := s.settingRepo.GetValue(ctx, SettingKeyExcelBPSEnabled)
		if readErr != nil && !errors.Is(readErr, ErrSettingNotFound) {
			return nil, readErr
		}
		enabled = value != "false"
		// Expose the saved image preference even while the global gate is off.
		imageValue, readErr := s.settingRepo.GetValue(ctx, SettingKeyExcelBPSImageRelayEnabled)
		if readErr != nil && !errors.Is(readErr, ErrSettingNotFound) {
			return nil, readErr
		}
		images.Enabled = imageValue == "" || imageValue == "true"
	}
	return &ExcelBPSSettings{Enabled: enabled, Images: images}, nil
}

func (s *SettingService) SaveExcelBPSSettings(ctx context.Context, input ExcelBPSSettings) error {
	image := input.Images
	normalized, err := normalizeExcelBPSImageRelaySettings(image.Enabled, image.BaseURL, image.Mode)
	if err != nil {
		return err
	}
	if err := validateExcelBPSImageCapacity(image.BodyLimitMiB, image.BudgetMiB, image.MaxRequests); err != nil {
		return err
	}
	if err := image.Limits.Validate(); err != nil {
		return infraerrors.BadRequest("INVALID_EXCEL_BPS_IMAGE_LIMITS", err.Error())
	}
	if err := validateExcelBPSImagePolicy(image.Policy, image.WarningRemaining, image.CompactReserve, image.Limits.MaxImages); err != nil {
		return infraerrors.BadRequest("INVALID_EXCEL_BPS_IMAGE_POLICY", err.Error())
	}
	values := map[string]string{
		SettingKeyExcelBPSEnabled:               strconv.FormatBool(input.Enabled),
		SettingKeyExcelBPSImageRelayEnabled:     strconv.FormatBool(image.Enabled),
		SettingKeyExcelBPSImageMode:             normalized.Mode,
		SettingKeyExcelBPSImageBaseURL:          normalized.BaseURL,
		SettingKeyExcelBPSImageBodyLimitMiB:     strconv.Itoa(image.BodyLimitMiB),
		SettingKeyExcelBPSImageBudgetMiB:        strconv.Itoa(image.BudgetMiB),
		SettingKeyExcelBPSImageMaxRequests:      strconv.Itoa(image.MaxRequests),
		SettingKeyExcelBPSImageLimitPolicy:      image.Policy,
		SettingKeyExcelBPSImageWarningRemaining: strconv.Itoa(image.WarningRemaining),
		SettingKeyExcelBPSImageCompactReserve:   strconv.Itoa(image.CompactReserve),
		SettingKeyExcelBPSImageMaxImageMiB:      strconv.Itoa(image.Limits.MaxImageMiB),
		SettingKeyExcelBPSImageMaxImages:        strconv.Itoa(image.Limits.MaxImages),
		SettingKeyExcelBPSImageMaxTotalMiB:      strconv.Itoa(image.Limits.MaxTotalMiB),
		SettingKeyExcelBPSImageStorageMiB:       strconv.Itoa(image.Limits.StorageMiB),
		SettingKeyExcelBPSImageStorageEntries:   strconv.Itoa(image.Limits.StorageEntries),
		SettingKeyExcelBPSImageTTLMinutes:       strconv.Itoa(image.Limits.TTLMinutes),
	}
	return s.settingRepo.SetMultiple(ctx, values)
}
