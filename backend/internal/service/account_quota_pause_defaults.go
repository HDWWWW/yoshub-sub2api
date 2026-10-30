package service

import "maps"

// ApplyNewAccountQuotaPauseDefaults gives every newly persisted OpenAI account
// a 99% pause threshold for both quota windows. Existing accounts are not changed;
// imported explicit thresholds and per-window opt-outs remain configurable.
func ApplyNewAccountQuotaPauseDefaults(account *Account) {
	if account == nil || account.Platform != PlatformOpenAI {
		return
	}
	extra := maps.Clone(account.Extra)
	if extra == nil {
		extra = make(map[string]any, 2)
	}
	for _, key := range []string{"auto_pause_5h_threshold", "auto_pause_7d_threshold"} {
		if value, exists := extra[key]; !exists || value == nil {
			extra[key] = 0.99
		}
	}
	account.Extra = extra
}
