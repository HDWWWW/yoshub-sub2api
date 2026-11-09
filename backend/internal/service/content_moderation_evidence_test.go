package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestContentModerationEvidence_LongScriptKeywordBeyondRetention(t *testing.T) {
	text := strings.Repeat("场景：雨夜的街道。🎬\n", 1400) + "角色穿上仿生装甲走入画面。" + strings.Repeat("镜头缓慢后退。", 60)
	log := (&ContentModerationService{}).buildLog(ContentModerationCheckInput{}, defaultContentModerationConfig(),
		ContentModerationActionKeywordBlock, true, contentModerationKeywordCategory, 1, nil, text, nil, nil, "", "仿生")

	require.Contains(t, log.InputExcerpt, "角色穿上仿生装甲走入画面。")
	require.LessOrEqual(t, utf8.RuneCountInString(log.InputExcerpt), maxModerationExcerptRunes)
	require.True(t, utf8.ValidString(log.InputExcerpt))
	evidence := log.InputEvidence
	require.NotNil(t, evidence)
	require.True(t, evidence.Truncated)
	require.Equal(t, maxModerationEvidenceRunes, evidence.StoredCharacters)
	require.Equal(t, utf8.RuneCountInString(text), evidence.TotalCharacters)
	require.Equal(t, string([]rune(text)[:maxModerationEvidenceRunes]), evidence.Text)
	require.NotContains(t, evidence.Text, "仿生")
	require.Contains(t, evidence.MatchContext, "角色穿上仿生装甲走入画面。")
	require.LessOrEqual(t, utf8.RuneCountInString(evidence.MatchContext), defaultModerationKeywordContextChars)
	require.True(t, strings.HasPrefix(evidence.MatchContext, "…"))
	require.True(t, strings.HasSuffix(evidence.MatchContext, "…"))

	// List responses must not accidentally expose or transfer the large payload.
	encoded, err := json.Marshal(log)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "input_evidence")
	require.NotContains(t, string(encoded), "stored_characters")
}

func TestContentModerationEvidence_RedactsBeforeAnyWindow(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("abcdefghijklmnop", 35)
	text := strings.Repeat("序幕。", 130) + " before " + secret + " after 仿生装甲" + strings.Repeat("收尾。", 130)
	log := &ContentModerationLog{MatchedKeyword: "仿生"}
	setContentModerationEvidence(log, text, defaultModerationKeywordContextChars)
	for _, value := range []string{log.InputExcerpt, log.InputEvidence.Text, log.InputEvidence.MatchContext} {
		require.NotContains(t, value, "abcdefghijklmnop")
		require.NotContains(t, value, "sk-proj-")
		require.Contains(t, value, "[已脱敏]")
		require.Contains(t, value, "仿生")
	}

	// Never reconstruct a hidden credential merely to show a keyword hit.
	log.MatchedKeyword = "abcdefghijklmnop"
	setContentModerationEvidence(log, text, defaultModerationKeywordContextChars)
	require.Empty(t, log.InputEvidence.MatchContext)
	require.NotContains(t, log.InputExcerpt, log.MatchedKeyword)

	text = strings.Repeat("字", maxModerationEvidenceRunes-8) + " " + secret
	setContentModerationEvidence(log, text, defaultModerationKeywordContextChars)
	require.NotContains(t, log.InputEvidence.Text, "sk-proj-")
	require.NotContains(t, log.InputEvidence.Text, "abcdefghijklmnop")
}

func TestContentModerationEvidence_CompleteTextAndUnicodeContext(t *testing.T) {
	for _, text := range []string{"", "中文🎬\n最后一句。", strings.Repeat("字", maxModerationEvidenceRunes)} {
		log := &ContentModerationLog{}
		setContentModerationEvidence(log, text, defaultModerationKeywordContextChars)
		require.Equal(t, text, log.InputEvidence.Text)
		require.False(t, log.InputEvidence.Truncated)
		require.Equal(t, utf8.RuneCountInString(text), log.InputEvidence.TotalCharacters)
		require.Equal(t, log.InputEvidence.TotalCharacters, log.InputEvidence.StoredCharacters)
	}
	for _, sample := range []struct{ text, keyword, match string }{
		{"仿生" + strings.Repeat("后", 500), "仿生", "仿生"},
		{strings.Repeat("前", 500) + "仿生", "仿生", "仿生"},
		{strings.Repeat("🎬İ", 500) + "BadWord" + strings.Repeat("终", 500), "badword", "BadWord"},
		{strings.Repeat("前", 500) + "KELVIN", "kelvin", "KELVIN"},
	} {
		context := moderationKeywordContext(sample.text, sample.keyword, maxModerationExcerptRunes)
		require.Contains(t, context, sample.match)
		require.LessOrEqual(t, utf8.RuneCountInString(context), maxModerationExcerptRunes)
		require.True(t, utf8.ValidString(context))
	}
}

func TestContentModerationEvidence_CurrentUserInputOnly(t *testing.T) {
	text := strings.Repeat("分镜说明。", 100) + "角色穿上仿生装甲。"
	body, err := json.Marshal(map[string]any{"messages": []map[string]string{
		{"role": "system", "content": "system-only-private-text"},
		{"role": "user", "content": "historical-private-text"},
		{"role": "assistant", "content": "assistant-private-text"},
		{"role": "user", "content": text},
	}})
	require.NoError(t, err)
	cfg := defaultContentModerationConfig()
	cfg.Enabled, cfg.Mode, cfg.BlockedKeywords = true, ContentModerationModePreBlock, []string{"仿生"}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	repo := &contentModerationTestRepo{}
	svc := NewContentModerationService(&contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw),
	}}, repo, &contentModerationTestHashCache{}, nil, nil, nil, nil, nil)
	decision, err := svc.Check(context.Background(), ContentModerationCheckInput{
		Protocol: ContentModerationProtocolOpenAIChat, Body: body,
	})
	require.NoError(t, err)
	require.True(t, decision.Blocked)
	logs := requireContentModerationLogCount(t, repo, 1)
	require.Contains(t, logs[0].InputExcerpt, "角色穿上仿生装甲。")
	require.Equal(t, text, logs[0].InputEvidence.Text)
	require.NotContains(t, logs[0].InputEvidence.Text, "private-text")
}

func TestContentModerationEvidence_ConfiguredKeywordContextAppliesWithoutRestart(t *testing.T) {
	text := strings.Repeat("分镜🎬。", 3500) + "仿生" + strings.Repeat("后续🎬。", 3500)
	body, err := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": text}}})
	require.NoError(t, err)
	cfg := defaultContentModerationConfig()
	cfg.Enabled, cfg.Mode, cfg.BlockedKeywords = true, ContentModerationModePreBlock, []string{"仿生"}
	cfg.EmailOnHit, cfg.AutoBanEnabled = false, false
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	settings := &contentModerationTestSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw),
	}}
	repo := &contentModerationTestRepo{}
	svc := NewContentModerationService(settings, repo, &contentModerationTestHashCache{}, nil, nil, nil, nil, nil)
	for index, chars := range []int{1, 80, 2000, 12000} {
		view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{KeywordContextChars: &chars})
		require.NoError(t, err)
		require.Equal(t, chars, view.KeywordContextChars)
		decision, err := svc.Check(context.Background(), ContentModerationCheckInput{Protocol: ContentModerationProtocolOpenAIChat, Body: body})
		require.NoError(t, err)
		require.True(t, decision.Blocked)
		logs := requireContentModerationLogCount(t, repo, index+1)
		log := logs[index]
		require.NotContains(t, log.InputEvidence.Text, "仿生")
		require.Contains(t, log.InputEvidence.MatchContext, "仿生")
		require.True(t, utf8.ValidString(log.InputEvidence.MatchContext))
		require.Equal(t, max(chars, 2), utf8.RuneCountInString(log.InputEvidence.MatchContext))
		require.LessOrEqual(t, utf8.RuneCountInString(log.InputExcerpt), maxModerationExcerptRunes)
	}
	// Persisted config survives service recreation and is shared by both engines.
	fresh := NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	view, err := fresh.GetConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, 12000, view.KeywordContextChars)
	for _, engine := range view.EngineConfigs {
		require.Equal(t, 12000, engine.KeywordContextChars)
	}
}

func TestContentModerationKeywordContextConfigDefaults(t *testing.T) {
	for _, value := range []int{0, -1, 12001} {
		cfg := defaultContentModerationConfig()
		cfg.KeywordContextChars = value
		cfg.normalize()
		require.Equal(t, 640, cfg.KeywordContextChars)
	}
	for _, sample := range []struct {
		text, keyword string
		limit         int
	}{
		{"前仿生后", "仿生", 1}, {"前仿生后", "仿生", 2}, {"前仿生后", "仿生", 3},
		{"仿生后后后", "仿生", 4}, {"前前前仿生", "仿生", 4}, {"前🎬后", "🎬", 1},
	} {
		value := moderationKeywordContext(sample.text, sample.keyword, sample.limit)
		require.Contains(t, value, sample.keyword)
		require.True(t, utf8.ValidString(value))
		require.LessOrEqual(t, utf8.RuneCountInString(value), max(sample.limit, utf8.RuneCountInString(sample.keyword)))
	}
}

func TestContentModerationEvidence_OfficialPoliciesRetainFullConversation(t *testing.T) {
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		t.Run(code, func(t *testing.T) {
			prompt := strings.Repeat("完整多轮剧本输入。", 2000) + "请求末尾证据"
			message := strings.Repeat("官方分析。", 1500) + "响应末尾证据"
			request, err := json.Marshal(map[string]any{
				"instructions": "完整系统提示", "input": []map[string]string{
					{"role": "developer", "content": "开发者提示"}, {"role": "user", "content": prompt},
					{"role": "assistant", "content": "助手消息"},
					{"type": "function_call", "call_id": "call-1", "arguments": "工具参数"},
					{"type": "function_call_output", "call_id": "call-1", "output": "完整工具结果"},
					{"role": "user", "content": "<codex_internal_context source=\"goal\">继续</codex_internal_context>"},
				},
				"tools":   []map[string]string{{"name": "test-tool", "description": "工具说明"}},
				"api_key": "short-secret", "metadata": map[string]any{"authorization": "another-secret", "large_id": json.Number("12345678901234567890")},
			})
			require.NoError(t, err)
			response, err := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{
				"error": map[string]string{"code": code, "message": message}, "provider_extra": "保留额外字段",
			}})
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			require.True(t, markOpenAICyberPolicyEvent(c, response, 200, nil))
			mark := GetOpsCyberPolicy(c)
			require.Equal(t, string(response), mark.Body, "do not truncate the provider event at 4096 bytes")
			repo := &contentModerationTestRepo{}
			svc := NewContentModerationService(&contentModerationTestSettingRepo{values: map[string]string{SettingKeyRiskControlEnabled: "true"}}, repo, nil, nil, nil, nil, nil, nil)
			svc.RecordCyberPolicyEvent(context.Background(), CyberPolicyRecordInput{PolicyCode: code, AccountID: 363, AccountName: "pool-account", RequestBody: string(request), UpstreamBody: mark.Body, UpstreamStatus: 200})
			logs := repo.snapshotLogs()
			require.Len(t, logs, 1)
			evidence := logs[0].InputEvidence
			require.NotNil(t, evidence.Policy)
			require.False(t, evidence.Truncated)
			require.Equal(t, moderationConversationScope, evidence.ContentScope)
			require.Equal(t, "request", evidence.Source)
			require.Greater(t, evidence.StoredCharacters, maxModerationEvidenceRunes)
			require.Equal(t, utf8.RuneCountInString(evidence.Text), evidence.StoredCharacters)
			require.Equal(t, evidence.StoredCharacters, evidence.TotalCharacters)
			require.Equal(t, int64(363), *logs[0].AccountID)
			require.Equal(t, "pool-account", logs[0].AccountName)
			require.Equal(t, 200, evidence.Policy.UpstreamStatus)
			require.Equal(t, code, evidence.Policy.Code)
			require.JSONEq(t, string(response), evidence.Policy.UpstreamBody)
			for _, marker := range []string{"完整系统提示", "开发者提示", "请求末尾证据", "助手消息", "工具参数", "完整工具结果", "codex_internal_context", "12345678901234567890"} {
				require.Contains(t, evidence.Text, marker)
			}
			require.NotContains(t, evidence.Text, "short-secret")
			require.NotContains(t, evidence.Text, "another-secret")
			var retained map[string]any
			require.NoError(t, json.Unmarshal([]byte(evidence.Text), &retained))
			require.Len(t, retained["input"], 6)
			require.LessOrEqual(t, utf8.RuneCountInString(logs[0].InputExcerpt), maxModerationExcerptRunes)

			require.NotContains(t, logs[0].Error, "官方分析")
		})
	}
}

func TestContentModerationEvidence_RedactsEscapedJSONSecrets(t *testing.T) {
	log := &ContentModerationLog{Action: OpenAIPolicyBio}
	setContentModerationPolicyEvidence(log, CyberPolicyRecordInput{RequestBody: `{"input":"hello sk-proj-1234567890abc\u0064ef","Authorization":"tiny","image":"data:image/png;base64,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`})
	require.Contains(t, log.InputEvidence.Text, "hello [已脱敏]")
	require.NotContains(t, log.InputEvidence.Text, "tiny")
	require.NotContains(t, log.InputEvidence.Text, "1234567890abc")
	require.True(t, json.Valid([]byte(log.InputEvidence.Text)))
	require.Empty(t, log.InputEvidence.Policy.RequestBody)
}

func TestContentModerationStorageLimitConfig(t *testing.T) {
	cfg := defaultContentModerationConfig()
	require.Equal(t, 200, cfg.LogStorageLimitGB)
	cfg.LogStorageLimitGB = 900
	cfg.normalize()
	require.Equal(t, 200, cfg.LogStorageLimitGB)
	settings := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	for _, value := range []int{0, -1, 201} {
		_, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{LogStorageLimitGB: &value})
		require.ErrorContains(t, err, "INVALID_MODERATION_LOG_CAPACITY")
	}
	value := 25
	view, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{LogStorageLimitGB: &value})
	require.NoError(t, err)
	require.Equal(t, value, view.LogStorageLimitGB)
}

func TestContentModerationEvidence_PolicyErrorExceedsPreviewLimit(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, code := range []string{OpenAIPolicyCyber, OpenAIPolicyBio} {
		body := `{"error":{"code":"` + code + `","message":"blocked"},"evidence":"` + strings.Repeat("完整内容", int(openAIUpstreamErrorBodyReadLimit)/4) + `末尾证据"}`
		response := func() *http.Response { return &http.Response{Body: io.NopCloser(strings.NewReader(body))} }
		require.Equal(t, body, string(svc.readUpstreamErrorBody(response())))
		resp := response()
		captured, _, complete := svc.readOpenAIUpstreamErrorWithCompleteness(resp)
		require.True(t, complete)
		require.Equal(t, body, string(captured))
		replayed, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(replayed))
	}
}

func TestContentModerationEvidence_BufferedSuccessDoesNotSetPolicyMark(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	// An inconsistent error field on a successful terminal response must not
	// introduce a policy side effect merely because evidence capture was added.
	wire := `data: {"type":"response.completed","response":{"status":"completed","error":{"code":"cyber_policy"}}}` + "\n\n"
	resp := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(wire))}
	_, _, _, _, err := (&OpenAIGatewayService{}).readOpenAICompatBufferedTerminal(resp, c, nil, "test", "req-test")
	require.NoError(t, err)
	require.Nil(t, GetOpsCyberPolicy(c))
}
