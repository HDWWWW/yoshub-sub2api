package service

// OpenAIForceFastExtraKey enables priority forwarding with Standard customer
// billing for this account. Missing values keep the existing request policy.
const OpenAIForceFastExtraKey = "openai_force_fast"

func (a *Account) IsOpenAIForceFastEnabled() bool {
	if a == nil || !a.IsOpenAI() {
		return false
	}
	enabled, _ := a.Extra[OpenAIForceFastExtraKey].(bool)
	return enabled
}
