package promptfilter

import "regexp"

// SignalMatchContext exposes the detector's reconstructed evidence, including
// decoded views and cache hits, without rerunning or changing classification.
// Callers must bound and redact this text before persisting it.
func SignalMatchContext(signal Signal) string {
	if signal.legacyVerdict != nil && signal.legacyVerdict.MatchContext != "" {
		return signal.legacyVerdict.MatchContext
	}
	return signal.reviewText
}

// SignalContextKeywords restores visual highlights when upstream redaction
// deliberately removes its marker tokens. Only already-matched rules are used,
// and only substrings present in the caller's sanitized, bounded context are
// returned. This helper never reevaluates a decision or exposes original input.
func SignalContextKeywords(signal Signal, cfg Config, sanitizedContext string) []string {
	words := []string{}
	if sanitizedContext == "" {
		return words
	}
	engine, err := engineForConfig(legacyRegexDetectionConfig(cfg))
	if err != nil {
		return words
	}
	names, seen := map[string]bool{}, map[string]bool{}
	for _, match := range signal.Matches {
		names[match.Name] = true
	}
	for _, pattern := range engine.patterns {
		if !names[pattern.cfg.Name] {
			continue
		}
		expressions := []*regexp.Regexp{pattern.re}
		expressions = append(expressions, pattern.all...)
		expressions = append(expressions, pattern.any...)
		for _, expression := range expressions {
			if expression == nil {
				continue
			}
			for _, word := range expression.FindAllString(sanitizedContext, 32) {
				if word == "" || seen[word] {
					continue
				}
				seen[word] = true
				runes := []rune(word)
				if len(runes) > 200 {
					word = string(runes[:200])
				}
				words = append(words, word)
				if len(words) >= 32 {
					return words
				}
			}
		}
	}
	return words
}
