package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPolicyPayloadPreservesContextAndMasksCredentials(t *testing.T) {
	raw := `{"input":[{"role":"system","content":"https://example.test/docs 12345678-1234-1234-1234-123456789abc"},{"type":"function_call","arguments":"{\"password\":\"small\",\"query\":\"keep\"}"},{"type":"function_call_output","output":"Authorization: Bearer abcdefghijklmnopqrstuvwxyz\nTail evidence"},{"type":"reasoning","encrypted_content":"opaque_payload_` + strings.Repeat("x", 80) + `"}],"api_key":"short","metadata":{"authorization":"tiny","large_id":12345678901234567890}}`
	actual := redactContentModerationPolicyPayload(raw)
	require.True(t, json.Valid([]byte(actual)))
	for _, text := range []string{"https://example.test/docs", "12345678-1234-1234-1234-123456789abc", "12345678901234567890", "Tail evidence", "query", "keep", "opaque_payload_" + strings.Repeat("x", 80)} {
		require.Contains(t, actual, text)
	}
	for _, secret := range []string{"small", "short", "tiny", "abcdefghijklmnopqrstuvwxyz"} {
		require.NotContains(t, actual, secret)
	}
}

func TestPolicyPayloadKeepsEveryMessageBeyondOldLimit(t *testing.T) {
	for _, field := range []string{"input", "messages", "contents"} {
		t.Run(field, func(t *testing.T) {
			messages := make([]map[string]string, 120)
			for i := range messages {
				messages[i] = map[string]string{"role": "user", "content": strings.Repeat("完整会话内容。", 1000)}
			}
			messages[119]["content"] = "FINAL-CONVERSATION-MARKER"
			body, err := json.Marshal(map[string]any{field: messages})
			require.NoError(t, err)
			log := &ContentModerationLog{Action: OpenAIPolicyCyber}
			setContentModerationPolicyEvidence(log, CyberPolicyRecordInput{RequestBody: string(body)})
			var decoded map[string][]map[string]string
			require.NoError(t, json.Unmarshal([]byte(log.InputEvidence.Text), &decoded))
			require.Equal(t, messages, decoded[field])
			require.False(t, log.InputEvidence.Truncated)
			require.Greater(t, len(log.InputEvidence.Text), 1_000_000)
		})
	}
}

func TestPolicyLegacyExcerptIsNotClaimedAsFullConversation(t *testing.T) {
	old := &ContentModerationInputEvidence{ContentScope: moderationUserBodyScope, Text: "continue", Policy: &ContentModerationPolicyEvidence{Code: OpenAIPolicyCyber}}
	repo := &contentModerationTestRepo{logs: []ContentModerationLog{{ID: 8, InputEvidence: old}}}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	actual, err := svc.GetLogInput(context.Background(), 8)
	require.NoError(t, err)
	require.Equal(t, moderationUserBodyScope, actual.Evidence.ContentScope)
	require.Equal(t, "continue", actual.Evidence.Text)
}
