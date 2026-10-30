package service

import "maps"

// ApplyNewAccountQuotaPauseDefaults gives every newly persisted OpenAI account
// a 99% account scheduling override. Explicit imported values (including 100
// to disable it) are preserved. Legacy per-window settings remain independent.
func ApplyNewAccountQuotaPauseDefaults(account *Account) {
	if account == nil || account.Platform != PlatformOpenAI {
		return
	}
	credentials := maps.Clone(account.Credentials)
	if credentials == nil {
		credentials = make(map[string]any, 1)
	}
	if value, exists := credentials[accountSchedulingThresholdCredentialKey]; !exists || value == nil {
		credentials[accountSchedulingThresholdCredentialKey] = 99
	}
	account.Credentials = credentials
}
