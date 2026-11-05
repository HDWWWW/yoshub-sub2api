package service

import "strings"

// IsExcelBPSEnabled selects the independently authorized Excel BPS transport.
func (a *Account) IsExcelBPSEnabled() bool {
	if a == nil || a.Platform != PlatformOpenAI || a.Type != AccountTypeOAuth || a.IsShadow() || a.IsOpenAIAgentIdentity() || a.IsOpenAIPersonalAccessToken() {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(a.GetCredential("plan_type")), "free") {
		return false
	}
	enabled, _ := a.Extra["openai_excel_bps"].(bool)
	return enabled && a.Extra[ExcelBPSAuthorizationPendingKey] != true
}

// Imported pending grants must remain ineligible until separately authorized.
const ExcelBPSAuthorizationPendingKey = "openai_excel_bps_authorization_pending"

const ExcelBPSIgnoreEncryptedContentKey = "openai_excel_bps_ignore_encrypted_content"

// IsExcelBPSIgnoreEncryptedContentEnabled opts into replacing ciphertext that
// BPS cannot forward, such as sub-agent messages in an old Codex conversation,
// with an omission notice instead of rejecting the whole request.
func (a *Account) IsExcelBPSIgnoreEncryptedContentEnabled() bool {
	if !a.IsExcelBPSEnabled() {
		return false
	}
	enabled, _ := a.Extra[ExcelBPSIgnoreEncryptedContentKey].(bool)
	return enabled
}

func (a *Account) IsExcelBPSCacheCreationAsInputEnabled() bool {
	if !a.IsExcelBPSEnabled() {
		return false
	}
	enabled, _ := a.Extra["openai_excel_bps_cache_creation_as_input"].(bool)
	return enabled
}

func (a *Account) isExcelBPSAllModelsEnabled() bool {
	if !a.IsExcelBPSEnabled() {
		return false
	}
	_, scoped := a.Extra["openai_excel_bps_models"]
	return !scoped
}

// IsExcelBPSEnabledForModel selects the protocol after account model mapping.
// Excel OAuth accounts cannot fall back to native Codex outside this scope.
func (a *Account) IsExcelBPSEnabledForModel(requestedModel string) bool {
	if !a.IsExcelBPSEnabled() {
		return false
	}
	return a.isExcelBPSUpstreamModelEnabled(a.GetMappedModel(requestedModel))
}

// IsExcelBPSImagesEnabledForModel routes image generation through BPS only
// when the image model is listed explicitly or an Excel OAuth account enables
// all models. Legacy Codex account-wide flags do not change the image channel.
func (a *Account) IsExcelBPSImagesEnabledForModel(requestedModel string) bool {
	if !a.IsExcelBPSEnabled() || (a.isExcelBPSAllModelsEnabled() && !a.IsExcelOAuth()) {
		return false
	}
	model := a.GetMappedModel(requestedModel)
	return usesCodexDirectImages(model) && excelBPSImagesSupportedModel(model) && a.isExcelBPSUpstreamModelEnabled(model)
}

func (a *Account) isExcelBPSUpstreamModelEnabled(model string) bool {
	if !a.IsExcelBPSEnabled() {
		return false
	}
	raw, scoped := a.Extra["openai_excel_bps_models"]
	if !scoped {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	switch models := raw.(type) {
	case []string:
		for _, selected := range models {
			if strings.TrimSpace(selected) == model {
				return true
			}
		}
	case []any:
		for _, selected := range models {
			if name, ok := selected.(string); ok && strings.TrimSpace(name) == model {
				return true
			}
		}
	}
	return false
}
