package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// No workers: API calls are synchronous or remain inspectable in the queue.
func moderationChecksService(t *testing.T, cfg *ContentModerationConfig) *ContentModerationService {
	t.Helper()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	settings := &contentModerationRuntimeSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw),
	}}
	svc := NewContentModerationService(settings, nil, &contentModerationTestHashCache{}, nil, nil, nil, nil, nil)
	svc.repo = &contentModerationTestRepo{}
	return svc
}

func TestContentModerationIndependentChecks(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		api, keywords bool
	}{
		{ContentModerationKeywordModeKeywordAndAPI, true, true},
		{ContentModerationKeywordModeAPIOnly, true, false},
		{ContentModerationKeywordModeKeywordOnly, false, true},
		{ContentModerationKeywordModeDisabled, false, false},
	} {
		for _, rules := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/rules=%t", tc.mode, rules), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_ = json.NewEncoder(w).Encode(moderationAPIResponse{Results: []moderationAPIResult{{CategoryScores: map[string]float64{"sexual": 0.99}}}})
				}))
				defer server.Close()
				cfg := defaultContentModerationConfig()
				cfg.Enabled, cfg.BaseURL, cfg.APIKeys = true, server.URL, []string{"sk-local-check"}
				cfg.BlockedKeywords, cfg.KeywordBlockingMode = []string{"keywordcheckmarker"}, tc.mode
				cfg.RuleFilter = ruleProbeConfig()
				cfg.RuleFilter.Enabled = rules
				svc := moderationChecksService(t, cfg)
				for _, prompt := range []struct {
					text          string
					keyword, rule bool
				}{
					{"hello", false, false},
					{"keywordcheckmarker", true, false},
					{ruleProbeText, false, true},
					{"keywordcheckmarker " + ruleProbeText, true, true},
				} {
					before := calls.Load()
					d, err := svc.Check(context.Background(), runtimeCacheTestInput(prompt.text))
					require.NoError(t, err)
					expected := ContentModerationActionAllow
					apiCalls := int32(0)
					switch {
					case rules && prompt.rule:
						expected = ContentModerationActionRuleBlock
					case tc.keywords && prompt.keyword:
						expected = ContentModerationActionKeywordBlock
					case tc.api:
						expected, apiCalls = ContentModerationActionBlock, 1
					}
					require.Equal(t, expected, d.Action, prompt.text)
					require.Equal(t, expected == ContentModerationActionAllow, d.Allowed)
					require.Equal(t, apiCalls, calls.Load()-before, prompt.text)
				}
			})
		}
	}
}

func TestContentModerationObserveRespectsIndependentChecks(t *testing.T) {
	for _, mode := range []string{ContentModerationKeywordModeKeywordAndAPI, ContentModerationKeywordModeAPIOnly, ContentModerationKeywordModeKeywordOnly, ContentModerationKeywordModeDisabled} {
		for _, rules := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/rules=%t", mode, rules), func(t *testing.T) {
				cfg := defaultContentModerationConfig()
				cfg.Enabled, cfg.Mode, cfg.APIKeys = true, ContentModerationModeObserve, []string{"sk-local-check"}
				cfg.KeywordBlockingMode = mode
				cfg.BlockedKeywords = []string{ruleProbeText}
				cfg.RuleFilter = ruleProbeConfig()
				cfg.RuleFilter.Enabled = rules
				svc := moderationChecksService(t, cfg)
				d, err := svc.Check(context.Background(), runtimeCacheTestInput(ruleProbeText))
				require.NoError(t, err)
				require.True(t, d.Allowed)
				apiTasks, ruleLogs := 0, 0
				for len(svc.asyncQueue) > 0 {
					task := <-svc.asyncQueue
					if task.ruleAudit == nil && task.log == nil {
						apiTasks++
					}
					if task.log != nil {
						ruleLogs++
						require.Equal(t, ContentModerationActionRuleObserve, task.log.Action)
					}
				}
				require.Equal(t, mode == ContentModerationKeywordModeKeywordAndAPI || mode == ContentModerationKeywordModeAPIOnly, apiTasks == 1)
				require.Equal(t, rules, ruleLogs > 0)
			})
		}
	}
}

func TestContentModerationRuleSwitchDoesNotReactivateLegacyRules(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled, cfg.KeywordBlockingMode = true, ContentModerationKeywordModeDisabled
	cfg.CombinationRules = []ContentModerationCombinationRule{combinationTestRule()}
	svc := moderationChecksService(t, cfg)
	input := runtimeCacheTestInput("蛋白结构预测")
	d, err := svc.Check(context.Background(), input)
	require.NoError(t, err)
	require.True(t, d.Blocked, "legacy-only configuration remains compatible")
	_, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{RuleFilter: DefaultContentModerationRuleFilter()})
	require.NoError(t, err)
	d, err = svc.Check(context.Background(), input)
	require.NoError(t, err)
	require.True(t, d.Allowed, "explicitly disabled combination rules must stay off")
}

func TestContentModerationDisableAPIStopsQueuedCalls(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(moderationAPIResponse{Results: []moderationAPIResult{{}}})
	}))
	defer server.Close()
	cfg := defaultContentModerationConfig()
	cfg.Enabled, cfg.Mode, cfg.BaseURL, cfg.APIKeys = true, ContentModerationModeObserve, server.URL, []string{"sk-local-check"}
	svc := moderationChecksService(t, cfg)
	_, err := svc.Check(context.Background(), runtimeCacheTestInput("hello"))
	require.NoError(t, err)
	require.Len(t, svc.asyncQueue, 1)
	mode := ContentModerationKeywordModeDisabled
	_, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{KeywordBlockingMode: &mode})
	require.NoError(t, err)
	// A following record confirms the queue was processed, without timing a
	// negative assertion. Existing evidence must still be persisted.
	svc.enqueueRecord(ContentModerationCheckInput{}, cfg, &ContentModerationLog{Action: ContentModerationActionAllow}, "", false, false)
	go svc.worker(0)
	require.Eventually(t, func() bool { return len(svc.repo.(*contentModerationTestRepo).snapshotLogs()) == 1 }, time.Second, time.Millisecond)
	require.Zero(t, calls.Load())
}
