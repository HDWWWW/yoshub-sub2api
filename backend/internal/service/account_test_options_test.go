//go:build unit

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func parameterTestService(kind string, status int) (*AccountTestService, *queuedHTTPUpstream) {
	a := &Account{ID: 71, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://api.example.com"},
		Extra:       map[string]any{"openai_responses_mode": "force_responses"}}
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"21\"}\n\ndata: {\"type\":\"response.completed\"}\n\n"
	if kind == "oauth" {
		a.Type = AccountTypeOAuth
		a.Credentials = map[string]any{"access_token": "test-token"}
	}
	if kind == "chat" {
		a.Extra["openai_responses_mode"] = "force_chat_completions"
		body = "data: {\"choices\":[{\"delta\":{\"content\":\"21\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	}
	if status != http.StatusOK {
		body = `{"error":{"message":"unsupported reasoning effort","type":"invalid_request_error"}}`
	}
	up := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(status, body)}}
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{a.ID: a}}}
	return &AccountTestService{accountRepo: repo, httpUpstream: up, cfg: rawChatCompletionsTestConfig()}, up
}

func TestAccountTestCustomParametersWire(t *testing.T) {
	for _, kind := range []string{"oauth", "responses", "chat"} {
		for _, background := range []bool{false, true} {
			for _, effort := range []string{"", "none", "xhigh", "max"} {
				t.Run(kind+"/"+effort+map[bool]string{false: "/manual", true: "/background"}[background], func(t *testing.T) {
					svc, up := parameterTestService(kind, http.StatusOK)
					prompt := "  请只回复 21\n保留换行与空格  "
					opts := AccountTestOptions{Prompt: prompt, ReasoningEffort: effort}
					if background {
						result, err := svc.RunTestBackground(context.Background(), 71, "gpt-5.4", opts)
						require.NoError(t, err)
						require.Equal(t, "success", result.Status)
						require.Equal(t, "21", result.ResponseText)
					} else {
						gc, _ := newTestContext()
						require.NoError(t, svc.TestAccountConnection(gc, 71, "gpt-5.4", prompt, "default", opts))
					}
					require.Len(t, up.requests, 1)
					req := up.requests[0]
					raw, err := io.ReadAll(req.Body)
					require.NoError(t, err)
					require.Equal(t, "gpt-5.4", gjson.GetBytes(raw, "model").String())
					promptPath, effortPath := "input.0.content.0.text", "reasoning.effort"
					if kind == "chat" {
						promptPath, effortPath = "messages.0.content", "reasoning_effort"
						require.Equal(t, "/v1/chat/completions", req.URL.Path)
					}
					require.Equal(t, prompt, gjson.GetBytes(raw, promptPath).String())
					require.Equal(t, effort, gjson.GetBytes(raw, effortPath).String())
					require.Equal(t, effort != "", gjson.GetBytes(raw, effortPath).Exists())
					if kind == "oauth" {
						require.False(t, gjson.GetBytes(raw, "store").Bool())
						require.NotEmpty(t, gjson.GetBytes(raw, "instructions").String())
					}
				})
			}
		}
	}
}

func TestAccountTestCustomParametersDefaultsAndFailures(t *testing.T) {
	for _, kind := range []string{"oauth", "responses", "chat"} {
		t.Run(kind, func(t *testing.T) {
			svc, up := parameterTestService(kind, 200)
			gc, _ := newTestContext()
			require.NoError(t, svc.TestAccountConnection(gc, 71, "gpt-5.4", "", ""))
			body, _ := io.ReadAll(up.requests[0].Body)
			require.Contains(t, string(body), `"hi"`)
			require.False(t, gjson.GetBytes(body, "reasoning").Exists())
			require.False(t, gjson.GetBytes(body, "reasoning_effort").Exists())
			svc, up = parameterTestService(kind, 400)
			gc, w := newTestContext()
			require.Error(t, svc.TestAccountConnection(gc, 71, "gpt-5.4", "custom", "", AccountTestOptions{ReasoningEffort: "high"}))
			require.Len(t, up.requests, 1)
			require.Contains(t, w.Body.String(), "unsupported reasoning effort")
			require.NotContains(t, w.Body.String(), `"success":true`)
		})
	}
	for _, opts := range []AccountTestOptions{{Prompt: strings.Repeat("界", MaxAccountTestPromptBytes/3+1)}, {ReasoningEffort: "made-up"}} {
		svc, up := parameterTestService("responses", 200)
		gc, _ := newTestContext()
		require.Error(t, svc.TestAccountConnection(gc, 71, "gpt-5.4", opts.Prompt, "", opts))
		require.Empty(t, up.requests)
	}
	for _, modelMode := range [][2]string{{"gpt-image-2", "default"}, {"gpt-5.4", "compact"}} {
		svc, up := parameterTestService("responses", 200)
		gc, _ := newTestContext()
		require.Error(t, svc.TestAccountConnection(gc, 71, modelMode[0], "message", modelMode[1], AccountTestOptions{ReasoningEffort: "high"}))
		require.Empty(t, up.requests)
	}
}

type parameterPlanRepo struct {
	ScheduledTestPlanRepository
	updated bool
}

func (r *parameterPlanRepo) UpdateAfterRun(context.Context, int64, time.Time, time.Time) error {
	r.updated = true
	return nil
}

type parameterResultRepo struct {
	ScheduledTestResultRepository
	saved *ScheduledTestResult
}

func (r *parameterResultRepo) Create(_ context.Context, v *ScheduledTestResult) (*ScheduledTestResult, error) {
	r.saved = v
	return v, nil
}
func (r *parameterResultRepo) PruneOldResults(context.Context, int64, int) error { return nil }

func TestScheduledTestRunnerCustomParameters(t *testing.T) {
	svc, up := parameterTestService("responses", 200)
	pr, rr := &parameterPlanRepo{}, &parameterResultRepo{}
	runner := NewScheduledTestRunnerService(pr, NewScheduledTestService(pr, rr), svc, nil, nil)
	runner.runOnePlan(context.Background(), &ScheduledTestPlan{ID: 8, AccountID: 71, ModelID: "gpt-5.4", Prompt: "定时内容 21", ReasoningEffort: "high", CronExpression: "*/5 * * * *", MaxResults: 10})
	require.Len(t, up.requests, 1)
	raw, _ := io.ReadAll(up.requests[0].Body)
	require.Equal(t, "定时内容 21", gjson.GetBytes(raw, "input.0.content.0.text").String())
	require.Equal(t, "high", gjson.GetBytes(raw, "reasoning.effort").String())
	require.True(t, pr.updated)
	require.Equal(t, int64(8), rr.saved.PlanID)
	require.Equal(t, "success", rr.saved.Status)
	// Zero values from pre-upgrade plans remain compatible with the new fields.
	var old ScheduledTestPlan
	require.NoError(t, json.Unmarshal([]byte(`{"model_id":"gpt-5.4"}`), &old))
	require.Empty(t, old.Prompt)
	require.Empty(t, old.ReasoningEffort)
}
