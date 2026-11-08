package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationUserBodyProtocols(t *testing.T) {
	for _, tc := range []struct{ name, protocol, body, want, source string }{
		{"responses continuation", ContentModerationProtocolOpenAIResponses, `{"instructions":"noise","tools":[{"description":"noise"}],"input":[{"role":"system","content":"noise"},{"role":"developer","content":"noise"},{"role":"user","content":[{"type":"input_text","text":"用户正文\n第二行"},{"type":"input_image","image_url":"data:noise"},{"type":"input_file","file_data":"noise"}]},{"role":"assistant","content":"noise"},{"type":"function_call","arguments":"noise"},{"type":"function_call_output","output":"noise"},{"type":"reasoning","summary":[{"text":"noise"}]}]}`, "用户正文\n第二行", "input[2]"},
		{"responses string", ContentModerationProtocolOpenAIResponses, `{"input":"正文"}`, "正文", "input"},
		{"responses implicit user", ContentModerationProtocolOpenAIResponses, `{"input":[{"content":[{"type":"input_text","text":"正文"}]}]}`, "正文", "input[0]"},
		{"responses object", ContentModerationProtocolOpenAIResponses, `{"input":{"role":"user","content":"正文"}}`, "正文", "input"},
		{"latest chat user", ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"user","content":"旧消息"},{"role":"assistant","content":"noise"},{"role":"user","content":"新消息"},{"role":"tool","content":"noise"}]}`, "新消息", "messages[2]"},
		{"anthropic tool turn", ContentModerationProtocolAnthropicMessages, `{"system":"noise","messages":[{"role":"user","content":[{"type":"text","text":"正文"},{"type":"image","source":{"data":"noise"}}]},{"role":"assistant","content":[{"type":"tool_use","input":{"text":"noise"}}]},{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"noise"}]}]}]}`, "正文", "messages[0]"},
		{"mixed anthropic user", ContentModerationProtocolAnthropicMessages, `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"noise"},{"type":"text","text":"正文"}]}]}`, "正文", "messages[0]"},
		{"mixed client context", ContentModerationProtocolOpenAIResponses, `{"input":[{"role":"user","content":[{"type":"input_text","text":"Another language model started to solve this problem and produced a summary of its thinking process. noise"},{"type":"input_text","text":"正文"},{"type":"input_text","text":"# AGENTS.md instructions\n<INSTRUCTIONS>noise</INSTRUCTIONS>"}]}]}`, "正文", "input[0]"},
		{"gemini tool turn", ContentModerationProtocolGemini, `{"contents":[{"role":"user","parts":[{"text":"正文"},{"inlineData":{"data":"noise"}}]},{"role":"model","parts":[{"text":"noise"}]},{"role":"user","parts":[{"functionResponse":{"response":{"text":"noise"}}}]}]}`, "正文", "contents[0]"},
		{"images", ContentModerationProtocolOpenAIImages, `{"prompt":"画一只猫","images":[{"text":"noise"}]}`, "画一只猫", "prompt"},
		{"systemone state", ContentModerationProtocolTypeSafeSystemOne, `{"questions":{"q":{"instructions":"noise"}},"state":{"text":"正文","token":"short-secret"}}`, "正文", "state"},
		{"policy auto detect", "", `{"messages":[{"role":"user","content":"正文"}]}`, "正文", "messages[0]"},
		{"no supplied history", ContentModerationProtocolOpenAIResponses, `{"previous_response_id":"private-id","input":[{"type":"function_call_output","output":"noise"}]}`, "", ""},
		{"no user role", ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"assistant","content":"noise"},{"content":"noise"}]}`, "", ""},
		{"malformed", ContentModerationProtocolOpenAIResponses, `{"input":"noise"`, "", ""},
		{"only image", ContentModerationProtocolOpenAIResponses, `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,noise"}]}]}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &ContentModerationLog{}
			setContentModerationUserEvidence(log, tc.protocol, []byte(tc.body), "must not fall back to raw detection text", 640)
			require.Equal(t, tc.want, log.InputEvidence.Text)
			require.Equal(t, tc.source, log.InputEvidence.Source)
			require.Equal(t, moderationUserBodyScope, log.InputEvidence.ContentScope)
			require.Equal(t, tc.want, log.InputExcerpt)
		})
	}
}

func TestContentModerationUserBodyClientEnvelopes(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"<system-reminder>noise</system-reminder>正文<environment_details>noise</environment_details>", "正文"},
		{"# AGENTS.md instructions for /project\n\n<INSTRUCTIONS>noise</INSTRUCTIONS>\n<environment_context>noise</environment_context>\n正文", "正文"},
		{"# Context from my IDE setup:\nfile: noise\n## My request:\n正文", "正文"},
		{"# Files mentioned by the user:\nnoise\n## My request:\n正文", "正文"},
		{"<user_query>正文</user_query>", "正文"},
		{"# 我的任务\n## My request:\n解释 <tag>内容</tag>\n{\"messages\":\"示例代码\"}", "# 我的任务\n## My request:\n解释 <tag>内容</tag>\n{\"messages\":\"示例代码\"}"},
	} {
		t.Run(tc.input, func(t *testing.T) { require.Equal(t, tc.want, cleanModerationUserBody(tc.input)) })
	}
	body, err := json.Marshal(map[string]any{"input": []map[string]string{
		{"role": "user", "content": "真实用户正文"},
		{"role": "user", "content": "# AGENTS.md instructions\n<INSTRUCTIONS>noise</INSTRUCTIONS>\n<environment_context>noise</environment_context>"},
		{"role": "user", "content": "Another language model started to solve this problem and produced a summary of its thinking process. noise"},
	}})
	require.NoError(t, err)
	log := &ContentModerationLog{}
	setContentModerationUserEvidence(log, ContentModerationProtocolOpenAIResponses, body, "", 640)
	require.Equal(t, "真实用户正文", log.InputEvidence.Text)
	require.Equal(t, "input[0]", log.InputEvidence.Source)
}

func TestCombinationUserBodyDoesNotChangeDetection(t *testing.T) {
	for _, tc := range []struct{ name, matchMessage, wantContext string }{
		{"tool", `{"type":"function_call_output","output":"工具噪音 猫画画"}`, ""},
		{"assistant", `{"role":"assistant","content":"助手噪音 猫画画"}`, ""},
		{"reminder", `{"role":"user","content":"<system-reminder>客户端噪音 猫画画</system-reminder>"}`, ""},
		{"historical user", `{"role":"user","content":"用户要求猫画画"}`, "用户要求猫画画"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultContentModerationConfig()
			cfg.Enabled, cfg.Mode = true, ContentModerationModePreBlock
			cfg.CombinationRules = []ContentModerationCombinationRule{{ID: "test", Name: "Test", Enabled: true, Action: "block", Scope: "conversation", WindowCharacters: 100, Groups: []ContentModerationCombinationGroup{{Name: "A", Keywords: []string{"猫"}}, {Name: "B", Keywords: []string{"画"}}}}}
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			repo := &contentModerationTestRepo{}
			svc := NewContentModerationService(&contentModerationTestSettingRepo{values: map[string]string{SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw)}}, repo, nil, nil, nil, nil, nil, nil)
			body := []byte(`{"input":[` + tc.matchMessage + `,{"role":"user","content":"最新正文"},{"type":"function_call_output","output":"尾部噪音"}]}`)
			decision, err := svc.Check(context.Background(), ContentModerationCheckInput{Protocol: ContentModerationProtocolOpenAIResponses, Body: body})
			require.NoError(t, err)
			require.True(t, decision.Blocked)
			log := requireContentModerationLogCount(t, repo, 1)[0]
			require.Equal(t, "最新正文", log.InputExcerpt)
			require.Equal(t, "最新正文", log.InputEvidence.Text)
			require.Equal(t, "input[1]", log.InputEvidence.Source)
			require.Empty(t, log.InputEvidence.MatchContext)
			hit := log.InputEvidence.CombinationRules[0]
			require.Equal(t, "input[0]", hit.Source)
			require.Equal(t, tc.wantContext, hit.Context)
			require.Len(t, hit.Matches, 2)
			stored, err := json.Marshal(log.InputEvidence)
			require.NoError(t, err)
			require.NotContains(t, string(stored), "噪音")
		})
	}
}

func TestContentModerationUserBodyLegacyPolicyProjection(t *testing.T) {
	old := &ContentModerationInputEvidence{Policy: &ContentModerationPolicyEvidence{
		Code: "bio_policy", RequestBody: `{"instructions":"noise","input":[{"role":"user","content":"正文"},{"type":"function_call_output","output":"noise"}]}`, UpstreamBody: "noise", UpstreamStatus: 400,
	}}
	repo := &contentModerationTestRepo{logs: []ContentModerationLog{{ID: 7, InputEvidence: old}}}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	result, err := svc.GetLogInput(context.Background(), 7)
	require.NoError(t, err)
	require.Equal(t, "正文", result.Evidence.Text)
	require.Equal(t, "正文", result.InputExcerpt)
	require.Equal(t, moderationUserBodyScope, result.Evidence.ContentScope)
	require.Equal(t, 400, result.Evidence.Policy.UpstreamStatus)
	require.Empty(t, result.Evidence.Policy.RequestBody)
	require.Empty(t, result.Evidence.Policy.UpstreamBody)
	require.Contains(t, old.Policy.RequestBody, "noise", "read projection must not mutate historical storage")
}
