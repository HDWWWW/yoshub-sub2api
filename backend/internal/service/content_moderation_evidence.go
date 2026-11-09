package service

import (
	"context"
	"strings"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const maxModerationEvidenceRunes = 12000
const defaultModerationKeywordContextChars = 640
const maxModerationKeywordContextChars = 12000
const moderationConversationScope = "full_conversation"

// Local/API audits retain bounded user text; provider policy events retain the
// complete request conversation, independently of the text used for detection.
// Character counts refer to Text, after redaction. Empty ContentScope identifies
// legacy evidence whose retained content may include non-user text.
type ContentModerationInputEvidence struct {
	RuleFilter       *ContentModerationRuleEvidence    `json:"rule_filter,omitempty"`
	ContentScope     string                            `json:"content_scope,omitempty"`
	Source           string                            `json:"source,omitempty"`
	CombinationRules []ContentModerationCombinationHit `json:"combination_rules,omitempty"`
	Text             string                            `json:"text"`
	TotalCharacters  int                               `json:"total_characters"`
	StoredCharacters int                               `json:"stored_characters"`
	Truncated        bool                              `json:"truncated"`
	MatchContext     string                            `json:"match_context,omitempty"`
	Policy           *ContentModerationPolicyEvidence  `json:"policy,omitempty"`
}

type ContentModerationPolicyEvidence struct {
	Code           string `json:"code"`
	RequestBody    string `json:"request_body,omitempty"` // Legacy read compatibility only.
	UpstreamBody   string `json:"upstream_body,omitempty"`
	UpstreamStatus int    `json:"upstream_status"`
}

func setContentModerationPolicyEvidence(log *ContentModerationLog, in CyberPolicyRecordInput) {
	text := redactContentModerationPolicyPayload(in.RequestBody)
	characters := utf8.RuneCountInString(text)
	log.InputEvidence = &ContentModerationInputEvidence{
		ContentScope: moderationConversationScope, Source: "request",
		Text: text, TotalCharacters: characters, StoredCharacters: characters,
		Policy: &ContentModerationPolicyEvidence{
			Code: log.Action, UpstreamStatus: in.UpstreamStatus,
			UpstreamBody: redactContentModerationPolicyPayload(in.UpstreamBody),
		},
	}
	// Keep the list small. The detail endpoint alone transfers full conversations.
	log.InputExcerpt = trimRunes(text, maxModerationExcerptRunes)
}

func isModerationSecretField(key string) bool {
	switch strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", "")) {
	case "apikey", "authorization", "cookie", "setcookie", "password", "passwd", "pwd", "secret", "clientsecret", "privatekey", "token", "accesstoken", "refreshtoken", "idtoken", "sessiontoken":
		return true
	}
	return false
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
	result, err := s.repo.GetLogInput(ctx, id)
	if err != nil || result == nil || result.Evidence == nil {
		return result, err
	}
	if policy := result.Evidence.Policy; policy != nil && policy.RequestBody != "" {
		// Restore legacy complete evidence without rewriting historical rows.
		// Previously retained user-only excerpts stay explicitly user-only.
		log := &ContentModerationLog{Action: policy.Code}
		setContentModerationPolicyEvidence(log, CyberPolicyRecordInput{RequestBody: policy.RequestBody, UpstreamBody: policy.UpstreamBody, UpstreamStatus: policy.UpstreamStatus})
		return &ContentModerationLogInput{InputExcerpt: log.InputExcerpt, Evidence: log.InputEvidence}, nil
	}
	return result, nil
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
