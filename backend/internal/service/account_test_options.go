package service

import "fmt"

const MaxAccountTestPromptBytes = 32 << 10

// ValidateAccountTestParameters is shared by manual tests and persisted plans.
// Empty values retain the original prompt and the upstream reasoning default.
func ValidateAccountTestParameters(prompt, effort string) error {
	if len(prompt) > MaxAccountTestPromptBytes {
		return fmt.Errorf("test prompt exceeds %d bytes", MaxAccountTestPromptBytes)
	}
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return nil
	default:
		return fmt.Errorf("unsupported reasoning_effort")
	}
}
