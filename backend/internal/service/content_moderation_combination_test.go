package service

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func combinationTestRule() ContentModerationCombinationRule {
	return ContentModerationCombinationRule{ID: "research", Name: "Research", Enabled: true, Action: "block", Scope: "conversation", WindowCharacters: 40,
		Groups: []ContentModerationCombinationGroup{
			{Name: "object", Keywords: []string{"蛋白", "protein"}},
			{Name: "feature", Keywords: []string{"结构", "structure"}},
			{Name: "work", Keywords: []string{"预测", "prediction"}},
		}}
}

func TestCombinationMatcherBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		window     int
		want       bool
	}{
		{"all groups", "蛋白的结构预测", 7, true},
		{"one group missing", "蛋白的结构", 40, false},
		{"inclusive distance", "蛋白🎬结构预测", 7, true},
		{"outside distance", "蛋白🎬结构预测", 6, false},
		{"last match counts", "蛋白" + strings.Repeat("字", 80) + "结构预测蛋白", 6, true},
		{"reverse order", "预测结构蛋白", 6, true},
		{"case independent", "PROTEIN Structure prediction", 40, true},
		{"word boundary", "proteinase structure prediction", 40, false},
		{"late match beyond retention", strings.Repeat("字", 13000) + "蛋白结构预测", 40, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule := combinationTestRule()
			rule.WindowCharacters = tc.window
			hits, err := TestContentModerationCombinationRules([]ContentModerationCombinationRule{rule}, tc.text)
			require.NoError(t, err)
			require.Equal(t, tc.want, len(hits) > 0)
			if tc.want {
				require.Len(t, hits[0].Matches, 3)
			}
		})
	}
	// Failure-link outputs and overlapping terms must report every group.
	rule := combinationTestRule()
	rule.Groups[0].Keywords, rule.Groups[1].Keywords, rule.Groups[2].Keywords = []string{"复合物"}, []string{"合物"}, []string{"物"}
	hits, err := TestContentModerationCombinationRules([]ContentModerationCombinationRule{rule}, "复合物")
	require.NoError(t, err)
	require.Len(t, hits, 1)
}

func TestCombinationInputMessageBoundariesAndToolContinuation(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, body string
		want                 bool
	}{
		{"responses history then tool", ContentModerationProtocolOpenAIResponses, `{"input":[{"role":"user","content":[{"type":"input_text","text":"蛋白结构预测"}]},{"type":"function_call_output","output":"done"}]}`, true},
		{"custom tool text", ContentModerationProtocolOpenAIResponses, `{"input":[{"type":"custom_tool_call_output","output":"蛋白结构预测"}]}`, true},
		{"tool structured text", ContentModerationProtocolOpenAIResponses, `{"input":[{"type":"function_call_output","output":[{"type":"input_text","text":"蛋白结构预测"}]}]}`, true},
		{"cross message", ContentModerationProtocolOpenAIResponses, `{"input":[{"role":"user","content":"蛋白"},{"role":"user","content":"结构预测"}]}`, false},
		{"system ignored", ContentModerationProtocolOpenAIResponses, `{"input":[{"role":"system","content":"蛋白结构预测"},{"role":"user","content":"hi"}]}`, false},
		{"encrypted reasoning ignored", ContentModerationProtocolOpenAIResponses, `{"input":[{"type":"reasoning","encrypted_content":"蛋白结构预测"}]}`, false},
		{"image ignored", ContentModerationProtocolOpenAIResponses, `{"input":[{"type":"input_image","image_url":"蛋白结构预测"}]}`, false},
		{"assistant history", ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"assistant","content":"蛋白结构预测"},{"role":"tool","content":"done"}]}`, true},
		{"anthropic tool", ContentModerationProtocolAnthropicMessages, `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"蛋白结构预测"}]}]}`, true},
		{"gemini tool", ContentModerationProtocolGemini, `{"contents":[{"role":"user","parts":[{"functionResponse":{"response":{"text":"蛋白结构预测"}}}]}]}`, true},
		{"string input", ContentModerationProtocolOpenAIResponses, `{"input":"蛋白结构预测"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := newCombinationMatcher([]ContentModerationCombinationRule{combinationTestRule()}).match(extractCombinationMessages(tc.protocol, []byte(tc.body)))
			require.Equal(t, tc.want, len(hits) > 0)
		})
	}
	rule := combinationTestRule()
	rule.Scope = "current_user"
	matcher := newCombinationMatcher([]ContentModerationCombinationRule{rule})
	require.Empty(t, matcher.match(extractCombinationMessages(ContentModerationProtocolOpenAIResponses, []byte(`{"input":[{"role":"user","content":"蛋白结构预测"},{"type":"function_call_output","output":"done"}]}`))))
	require.Len(t, matcher.match(extractCombinationMessages(ContentModerationProtocolOpenAIResponses, []byte(`{"input":"蛋白结构预测"}`))), 1)
}

func TestCombinationConfigValidationAndIsolation(t *testing.T) {
	rule := combinationTestRule()
	for _, mutate := range []func(*ContentModerationCombinationRule){
		func(r *ContentModerationCombinationRule) { r.ID = "" }, func(r *ContentModerationCombinationRule) { r.Name = "" },
		func(r *ContentModerationCombinationRule) { r.Action = "unknown" }, func(r *ContentModerationCombinationRule) { r.Scope = "unknown" },
		func(r *ContentModerationCombinationRule) { r.WindowCharacters = 0 }, func(r *ContentModerationCombinationRule) { r.WindowCharacters = 12001 },
		func(r *ContentModerationCombinationRule) { r.Groups = r.Groups[:1] }, func(r *ContentModerationCombinationRule) { r.Groups[0].Keywords = []string{" "} },
	} {
		copy := cloneCombinationRules([]ContentModerationCombinationRule{rule})
		mutate(&copy[0])
		_, err := normalizeCombinationRules(copy)
		require.Error(t, err)
	}
	_, err := normalizeCombinationRules([]ContentModerationCombinationRule{rule, rule})
	require.Error(t, err)
	copy := cloneCombinationRules([]ContentModerationCombinationRule{rule})
	copy[0].Groups[0].Keywords = []string{" Protein ", "protein", ""}
	normalized, err := normalizeCombinationRules(copy)
	require.NoError(t, err)
	require.Equal(t, []string{"Protein"}, normalized[0].Groups[0].Keywords)
	require.Equal(t, []string{" Protein ", "protein", ""}, copy[0].Groups[0].Keywords)
}

func TestCombinationServiceControlsAndAudit(t *testing.T) {
	for _, tc := range []struct {
		name, mode, action, allowlist string
		enabled, blocked, logged      bool
	}{
		{"block", "pre_block", "block", "", true, true, true},
		{"rule observe", "pre_block", "observe", "", true, false, true},
		{"global observe", "observe", "block", "", true, false, true},
		{"off", "off", "block", "", true, false, false},
		{"disabled", "pre_block", "block", "", false, false, false},
		{"allowlisted", "pre_block", "block", "7", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultContentModerationConfig()
			cfg.Enabled, cfg.Mode, cfg.KeywordBlockingMode = true, tc.mode, ContentModerationKeywordModeKeywordOnly
			rule := combinationTestRule()
			rule.Enabled, rule.Action = tc.enabled, tc.action
			cfg.CombinationRules = []ContentModerationCombinationRule{rule}
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			settings := &contentModerationTestSettingRepo{values: map[string]string{SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw), SettingKeyCyberPolicyUserAllowlist: tc.allowlist}}
			repo, cache := &contentModerationTestRepo{}, &contentModerationTestHashCache{}
			svc := NewContentModerationService(settings, repo, cache, nil, nil, nil, nil, nil)
			decision, err := svc.Check(context.Background(), ContentModerationCheckInput{UserID: 7, Protocol: ContentModerationProtocolOpenAIResponses, Body: []byte(`{"input":[{"role":"user","content":"蛋白结构预测"},{"type":"function_call_output","output":"done"}]}`)})
			require.NoError(t, err)
			require.Equal(t, tc.blocked, decision.Blocked)
			if tc.logged {
				logs := requireContentModerationLogCount(t, repo, 1)
				require.Len(t, logs[0].InputEvidence.CombinationRules, 1)
				require.Contains(t, logs[0].InputExcerpt, "蛋白结构预测")
				require.Equal(t, "input[0]", logs[0].InputEvidence.CombinationRules[0].Source)
				require.Zero(t, logs[0].ViolationCount)
				require.False(t, logs[0].EmailSent)
				require.False(t, logs[0].AutoBanned)
			}
		})
	}
}

func TestCombinationUpdatesApplyWithoutRestart(t *testing.T) {
	ctx := context.Background()
	settings := &contentModerationTestSettingRepo{values: map[string]string{SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: `{"enabled":true,"mode":"pre_block","keyword_blocking_mode":"keyword_only"}`}}
	svc := NewContentModerationService(settings, &contentModerationTestRepo{}, &contentModerationTestHashCache{}, nil, nil, nil, nil, nil)
	input := ContentModerationCheckInput{Protocol: ContentModerationProtocolOpenAIResponses, Body: []byte(`{"input":"蛋白结构预测"}`)}
	decision, err := svc.Check(ctx, input)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	rules := []ContentModerationCombinationRule{combinationTestRule()}
	view, err := svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{CombinationRules: &rules})
	require.NoError(t, err)
	require.Len(t, view.CombinationRules, 1)
	decision, err = svc.Check(ctx, input)
	require.NoError(t, err)
	require.True(t, decision.Blocked)
	fresh := NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	view, err = fresh.GetConfig(ctx)
	require.NoError(t, err)
	require.Len(t, view.CombinationRules, 1)
	rules[0].Action = "invalid"
	_, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{CombinationRules: &rules})
	require.Error(t, err)
	decision, err = svc.Check(ctx, input)
	require.NoError(t, err)
	require.True(t, decision.Blocked)
	rules = []ContentModerationCombinationRule{}
	_, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{CombinationRules: &rules})
	require.NoError(t, err)
	decision, err = svc.Check(ctx, input)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
}

func TestCombinationRequestedSeed(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/risk-control/biological-research-rules.json")
	require.NoError(t, err)
	var cfg struct {
		Rules []ContentModerationCombinationRule `json:"combination_rules"`
	}
	require.NoError(t, json.Unmarshal(raw, &cfg))
	hits, err := TestContentModerationCombinationRules(cfg.Rules, "请核验抗体与抗原复合物的结构预测数据集。")
	require.NoError(t, err)
	require.Len(t, hits, 1)
	hits, err = TestContentModerationCombinationRules(cfg.Rules, "什么是抗体？")
	require.NoError(t, err)
	require.Empty(t, hits)
	// Optional offline replay of incident evidence. Nothing is sent upstream;
	// private request fixtures are never checked into the repository.
	for _, path := range filepath.SplitList(os.Getenv("SUB2_COMBINATION_REPLAY_FILES")) {
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		hits := newCombinationMatcher(cfg.Rules).match(extractCombinationMessages(ContentModerationProtocolOpenAIResponses, body))
		require.NotEmpty(t, hits, "saved request did not match")
		t.Logf("offline replay: %d bytes, %d matching rule(s), source %s", len(body), len(hits), hits[0].Source)
	}
}

func TestCombinationMatcherRandomWindowParity(t *testing.T) {
	rng := rand.New(rand.NewSource(88))
	alphabet := []rune("甲乙丙丁")
	for iteration := 0; iteration < 600; iteration++ {
		rule := combinationTestRule()
		rule.WindowCharacters = 1 + rng.Intn(12)
		for i := range rule.Groups {
			rule.Groups[i].Keywords = nil
			for j := 0; j < 3; j++ {
				word := make([]rune, 1+rng.Intn(3))
				for k := range word {
					word[k] = alphabet[rng.Intn(len(alphabet))]
				}
				rule.Groups[i].Keywords = append(rule.Groups[i].Keywords, string(word))
			}
		}
		text := make([]rune, 40)
		for i := range text {
			text[i] = alphabet[rng.Intn(len(alphabet))]
		}
		want := false
		for start := range text {
			window := string(text[start:min(len(text), start+rule.WindowCharacters)])
			all := true
			for _, group := range rule.Groups {
				any := false
				for _, word := range group.Keywords {
					any = any || strings.Contains(window, word)
				}
				all = all && any
			}
			want = want || all
		}
		hits, err := TestContentModerationCombinationRules([]ContentModerationCombinationRule{rule}, string(text))
		require.NoError(t, err)
		require.Equal(t, want, len(hits) > 0, "iteration %d", iteration)
	}
}
