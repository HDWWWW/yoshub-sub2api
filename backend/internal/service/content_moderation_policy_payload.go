package service

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Unlike excerpt redaction, full policy evidence must retain URLs, identifiers,
// opaque model content, every message and tool result. Only credentials are masked.
var moderationPolicyCredential = regexp.MustCompile(`(?i)(["']?\b(?:api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|session[_-]?token|cookie|set[_-]?cookie|authorization|password|passwd|pwd|secret|client[_-]?secret|private[_-]?key)["']?\s*[:=]\s*["']?)[^"'\s,;&{}]+`)

func redactContentModerationPolicyPayload(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	if !json.Valid([]byte(raw)) {
		return redactModerationPolicyText(raw)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return redactModerationPolicyText(raw)
	}
	var scrub func(any) any
	scrub = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if isModerationSecretField(key) {
					v[key] = "[已脱敏]"
				} else {
					v[key] = scrub(child)
				}
			}
		case []any:
			for i, child := range v {
				v[i] = scrub(child)
			}
		case string:
			return redactModerationPolicyText(v)
		}
		return value
	}
	encoded, err := json.MarshalIndent(scrub(value), "", "  ")
	if err != nil {
		return redactModerationPolicyText(raw)
	}
	return string(encoded)
}

func redactModerationPolicyText(text string) string {

	// Bearer credentials, JWTs and recognizable API-key prefixes.
	for _, pattern := range contentModerationSecretPatterns[2:5] {
		text = pattern.ReplaceAllString(text, "[已脱敏]")
	}
	return moderationPolicyCredential.ReplaceAllString(text, `${1}[已脱敏]`)
}
