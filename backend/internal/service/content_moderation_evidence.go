package service

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const maxModerationEvidenceRunes = 12000
const defaultModerationKeywordContextChars = 640
const maxModerationKeywordContextChars = 12000

// Ordinary audits retain bounded extracted text. Official safety-policy events
// retain the complete redacted client request and rejecting provider event.
// Character counts refer to Text, after redaction.
type ContentModerationInputEvidence struct {
	Text             string                           `json:"text"`
	TotalCharacters  int                              `json:"total_characters"`
	StoredCharacters int                              `json:"stored_characters"`
	Truncated        bool                             `json:"truncated"`
	MatchContext     string                           `json:"match_context,omitempty"`
	Policy           *ContentModerationPolicyEvidence `json:"policy,omitempty"`
}

type ContentModerationPolicyEvidence struct {
	Code           string `json:"code"`
	RequestBody    string `json:"request_body"`
	UpstreamBody   string `json:"upstream_body"`
	UpstreamStatus int    `json:"upstream_status"`
}

func setContentModerationPolicyEvidence(log *ContentModerationLog, in CyberPolicyRecordInput) {
	request := redactModerationEvidenceJSON(in.RequestBody)
	log.InputExcerpt = trimRunes(request, maxModerationExcerptRunes)
	log.InputEvidence = &ContentModerationInputEvidence{
		Policy: &ContentModerationPolicyEvidence{
			Code: log.Action, RequestBody: request,
			UpstreamBody: redactModerationEvidenceJSON(in.UpstreamBody), UpstreamStatus: in.UpstreamStatus,
		},
	}
}

// Decode string values before redacting so escaped credentials cannot bypass
// the existing text redactor. Preserve JSON structure and all non-secret fields.
func redactModerationEvidenceJSON(raw string) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || !json.Valid([]byte(raw)) {
		return redactModerationEvidenceText(raw)
	}
	var redact func(any) any
	redact = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", "")) {
				case "apikey", "authorization", "cookie", "setcookie", "password", "passwd", "pwd", "secret", "clientsecret", "privatekey", "token", "accesstoken", "refreshtoken", "idtoken", "sessiontoken":
					v[key] = "[已脱敏]"
				default:
					v[key] = redact(child)
				}
			}
		case []any:
			for i := range v {
				v[i] = redact(v[i])
			}
		case string:
			return redactModerationEvidenceText(v)
		}
		return value
	}
	encoded, err := json.MarshalIndent(redact(value), "", "  ")
	if err != nil {
		return redactModerationEvidenceText(raw)
	}
	return string(encoded)
}

// Complete policy evidence preserves URLs, hashes, encoded images and long
// source strings. Only recognizable credentials are removed here; JSON secret
// fields are also removed structurally above, including short secret values.
func redactModerationEvidenceText(text string) string {
	for index := 1; index <= 4; index++ {
		pattern := contentModerationSecretPatterns[index]
		switch index {
		case 1:
			text = pattern.ReplaceAllString(text, `${1}${2}[已脱敏]`)
		case 2:
			text = pattern.ReplaceAllString(text, `${1}[已脱敏]`)
		default:
			text = pattern.ReplaceAllString(text, `[已脱敏]`)
		}
	}
	return text
}

type ContentModerationLogInput struct {
	InputExcerpt   string                          `json:"input_excerpt"`
	MatchedKeyword string                          `json:"matched_keyword"`
	Evidence       *ContentModerationInputEvidence `json:"evidence"`
}

func (s *ContentModerationService) GetLogInput(ctx context.Context, id int64) (*ContentModerationLogInput, error) {
	if id <= 0 {
		return nil, infraerrors.BadRequest("INVALID_LOG_ID", "审计记录 ID 无效")
	}
	return s.repo.GetLogInput(ctx, id)
}

func setContentModerationEvidence(log *ContentModerationLog, text string, keywordContextChars int) {
	// Redact the whole input BEFORE selecting windows; cutting a token first
	// could prevent the secret matcher from recognizing it.
	redacted := redactContentModerationSecrets(text)
	stored := trimRunes(redacted, maxModerationEvidenceRunes)
	log.InputEvidence = &ContentModerationInputEvidence{
		Text: stored, TotalCharacters: utf8.RuneCountInString(redacted),
		StoredCharacters: utf8.RuneCountInString(stored),
	}
	log.InputEvidence.Truncated = log.InputEvidence.StoredCharacters < log.InputEvidence.TotalCharacters
	log.InputExcerpt = trimRunes(redacted, maxModerationExcerptRunes)
	if log.MatchedKeyword != "" {
		// Search the entire sanitized text, including beyond the detail cap.
		log.InputEvidence.MatchContext = moderationKeywordContext(redacted, log.MatchedKeyword, normalizeModerationKeywordContextChars(keywordContextChars))
		if excerpt := moderationKeywordContext(redacted, log.MatchedKeyword, maxModerationExcerptRunes); excerpt != "" {
			log.InputExcerpt = excerpt
		}
	}
}

func normalizeModerationKeywordContextChars(value int) int {
	if value < 1 || value > maxModerationKeywordContextChars {
		return defaultModerationKeywordContextChars
	}
	return value
}

func moderationKeywordContext(text, keyword string, limit int) string {
	if keyword == "" {
		return ""
	}
	lower := strings.ToLower(text)
	index := strings.Index(lower, strings.ToLower(keyword))
	if index < 0 {
		return "" // A match inside a redacted credential must never be restored.
	}
	runes := []rune(text)
	start := utf8.RuneCountInString(lower[:index])
	end := start + utf8.RuneCountInString(keyword)
	// Leave room for omission markers and always retain the complete keyword.
	width := max(limit-2, end-start)
	left := max(0, start-(width-(end-start))/2)
	right := min(len(runes), left+width)
	left = max(0, right-width)
	result := string(runes[left:right])
	// Very small limits still preserve the full keyword. Add omission markers
	// only when both fit within the requested character budget.
	markers := 0
	if left > 0 {
		markers++
	}
	if right < len(runes) {
		markers++
	}
	showMarkers := right-left+markers <= max(limit, end-start)
	if left > 0 && showMarkers {
		result = "…" + result
	}
	if right < len(runes) && showMarkers {
		result += "…"
	}
	return result
}
