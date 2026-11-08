package promptfilter

// SignalMatchContext exposes the detector's reconstructed evidence, including
// decoded views and cache hits, without rerunning or changing classification.
// Callers must bound and redact this text before persisting it.
func SignalMatchContext(signal Signal) string {
	if signal.reviewText != "" {
		return signal.reviewText
	}
	if signal.legacyVerdict == nil {
		return ""
	}
	return signal.legacyVerdict.MatchContext
}
