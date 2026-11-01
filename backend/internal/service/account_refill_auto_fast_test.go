//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func autoFastFixture(now time.Time) (AccountRefillConfig, Account) {
	c := refillTestConfig()
	c.Enabled, c.MonitorEnabled, c.Mode = false, false, "fixed"
	c.AutoFast = &AccountRefillAutoFastConfig{Enabled: true, StopMode: "follow", StartRemainingPercent: 60, StopUsed5hPercent: 85, StopUsed7dPercent: 85}
	a := refillTestAccount(now.Add(time.Hour))
	a.Extra = map[string]any{AccountSourceExpiryExtraKey: now.Add(10 * time.Minute).Unix(), "codex_5h_used_percent": 20.0, "codex_7d_used_percent": 20.0,
		"codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339), "codex_7d_reset_at": now.Add(time.Hour).Format(time.RFC3339)}
	return c, a
}

func TestRefillAutoFastBoundariesAndQuotaModes(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		change func(*AccountRefillConfig, *Account)
		active bool
		want   bool
	}{
		{"starts in supplier window", func(*AccountRefillConfig, *Account) {}, false, true},
		{"legacy disabled", func(c *AccountRefillConfig, a *Account) { c.AutoFast = nil }, false, false},
		{"switch disabled", func(c *AccountRefillConfig, a *Account) { c.AutoFast.Enabled = false }, true, false},
		{"custom time before window", func(c *AccountRefillConfig, a *Account) { n := 9; c.AutoFast.LeadMinutes = &n }, false, false},
		{"custom time exact boundary", func(c *AccountRefillConfig, a *Account) { n := 10; c.AutoFast.LeadMinutes = &n }, false, true},
		{"zero lead", func(c *AccountRefillConfig, a *Account) { n := 0; c.AutoFast.LeadMinutes = &n }, true, false},
		{"remaining equal start", func(c *AccountRefillConfig, a *Account) { a.Extra["codex_5h_used_percent"] = 40.0 }, false, false},
		{"keep below start after activation", func(c *AccountRefillConfig, a *Account) { a.Extra["codex_5h_used_percent"] = 50.0 }, true, true},
		{"stop at 85 used", func(c *AccountRefillConfig, a *Account) { a.Extra["codex_5h_used_percent"] = 85.0 }, true, false},
		{"stop at other window", func(c *AccountRefillConfig, a *Account) { a.Extra["codex_7d_used_percent"] = 85.1 }, true, false},
		{"follow plan override", func(c *AccountRefillConfig, a *Account) {
			a.Extra["refill_plan"] = "pro20x"
			c.PlanQuotaThresholds = map[string]AccountRefillQuotaThreshold{"pro20x": {MinRemaining5h: 80, MinRemaining7d: 15}}
		}, true, false},
		{"custom overrides refill floor", func(c *AccountRefillConfig, a *Account) { c.MinRemaining5h = 90; c.AutoFast.StopMode = "custom" }, false, true},
		{"custom used bound", func(c *AccountRefillConfig, a *Account) {
			c.AutoFast.StopMode = "custom"
			c.AutoFast.StopUsed7dPercent = 20
		}, true, false},
		{"unknown quota", func(c *AccountRefillConfig, a *Account) { delete(a.Extra, "codex_7d_used_percent") }, true, false},
		{"invalid quota", func(c *AccountRefillConfig, a *Account) { a.Extra["codex_5h_used_percent"] = math.NaN() }, true, false},
		{"quota reset", func(c *AccountRefillConfig, a *Account) {
			a.Extra["codex_5h_reset_at"] = now.Add(-time.Second).Format(time.RFC3339)
		}, true, false},
		{"local grace does not extend supplier", func(c *AccountRefillConfig, a *Account) { a.Extra[AccountSourceExpiryExtraKey] = now.Unix() }, true, false},
		{"unknown supplier", func(c *AccountRefillConfig, a *Account) { delete(a.Extra, AccountSourceExpiryExtraKey) }, false, false},
		{"unavailable", func(c *AccountRefillConfig, a *Account) { a.Schedulable = false }, true, false},
		{"disabled window ignored", func(c *AccountRefillConfig, a *Account) {
			c.MinRemaining7d = 0
			delete(a.Extra, "codex_7d_used_percent")
		}, false, true},
		{"no stop windows", func(c *AccountRefillConfig, a *Account) { c.MinRemaining5h = 0; c.MinRemaining7d = 0 }, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, a := autoFastFixture(now)
			tc.change(&c, &a)
			e := evaluateAccountRefill(c, []Account{a}, nil, nil, now)
			require.Len(t, e.Accounts, 1)
			var activeUntil time.Time
			if tc.active && e.Accounts[0].SourceExpiresAt != nil {
				activeUntil = *e.Accounts[0].SourceExpiresAt
			}
			require.Equal(t, tc.want, refillAccountAutoFast(c, e.Accounts[0], activeUntil, now))
		})
	}
}

func TestRefillAutoFastPersistsHysteresisWithoutPurchasing(t *testing.T) {
	s, store, _, accounts := refillHarness(t)
	now := time.Now()
	c, a := autoFastFixture(now)
	s.provider = nil // No supplier calls, even with purchasing/monitoring disabled.
	accounts.accounts = []Account{a}
	store.mutate(t, func(state *accountRefillState) { state.Config = c })
	s.sampleCapacity(context.Background())
	require.True(t, s.accountAutoFast(2, 1, time.Now()))
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Contains(t, state.AutoFastActive, int64(1))
	require.Empty(t, state.Jobs)
	stable := store.raw
	s.sampleCapacity(context.Background())
	require.Equal(t, stable, store.raw, "no settings writes for stable samples")
	accounts.accounts[0].Extra["codex_5h_used_percent"] = 50.0
	restarted := &AccountRefillService{store: store, accounts: accounts, loads: s.loads}
	restarted.sampleCapacity(context.Background())
	require.True(t, restarted.accountAutoFast(2, 1, time.Now()), "restart preserves activation below start floor")
	accounts.accounts[0].Extra["codex_5h_used_percent"] = 85.0
	restarted.sampleCapacity(context.Background())
	require.False(t, restarted.accountAutoFast(2, 1, time.Now()))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Empty(t, state.AutoFastActive)
	// Config changes invalidate immediately; old samplers cannot re-enable it.
	c.AutoFast.Enabled = false
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	s.publishRefillAutoFast(2, state.Revision, map[int64]time.Time{1: now.Add(time.Hour)}, time.Now())
	require.False(t, s.accountAutoFast(2, 1, time.Now()))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	c = state.Config
	c.AutoFast = nil
	require.NoError(t, s.Configure(context.Background(), c, "", state.Revision))
	state, err = decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.NotNil(t, state.Config.AutoFast)
	require.False(t, state.Config.AutoFast.Enabled)
}

func TestRefillAutoFastFailsClosedAndExcludesOtherAccounts(t *testing.T) {
	s, store, _, accounts := refillHarness(t)
	now := time.Now()
	c, a := autoFastFixture(now)
	accounts.accounts = []Account{a}
	store.mutate(t, func(state *accountRefillState) { state.Config = c })
	store.failSave = true
	s.sampleCapacity(context.Background())
	require.False(t, s.accountAutoFast(2, 1, time.Now()))
	store.failSave = false
	s.sampleCapacity(context.Background())
	require.True(t, s.accountAutoFast(2, 1, time.Now()))
	require.False(t, s.accountAutoFast(3, 1, time.Now()))
	require.False(t, s.accountAutoFast(2, 2, time.Now()))
	require.False(t, s.accountAutoFast(2, 1, time.Now().Add(4*time.Second)))
}

func TestRefillAutoFastRequestSnapshotHTTPAndWebSocket(t *testing.T) {
	now := time.Now()
	refill := &AccountRefillService{}
	refill.publishRefillAutoFast(2, 1, map[int64]time.Time{1: now.Add(time.Hour)}, now)
	s := newOpenAIGatewayServiceWithSettings(t, nil)
	s.accountRefill = refill
	_, a := autoFastFixture(now)
	group := &Group{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
	base := context.WithValue(context.Background(), ctxkey.Group, group)
	requestCtx := s.withRefillAutoFast(base, &a)
	body, err := s.applyOpenAIFastPolicyToBody(requestCtx, &a, "gpt-5.5", []byte(`{"model":"gpt-5.5","service_tier":"flex"}`))
	require.NoError(t, err)
	require.Equal(t, "priority", gjson.GetBytes(body, "service_tier").String())
	frame := []byte(`{"type":"response.create","model":"gpt-5.5"}`)
	first, blocked, err := s.applyOpenAIFastPolicyToWSResponseCreate(base, &a, "gpt-5.5", frame)
	require.NoError(t, err)
	require.Nil(t, blocked)
	require.Equal(t, "priority", gjson.GetBytes(first, "service_tier").String())
	refill.publishRefillAutoFast(2, 2, nil, time.Now())
	second, blocked, err := s.applyOpenAIFastPolicyToWSResponseCreate(base, &a, "gpt-5.5", frame)
	require.NoError(t, err)
	require.Nil(t, blocked)
	require.False(t, gjson.GetBytes(second, "service_tier").Exists())
	result := &OpenAIForwardResult{}
	markRefillAutoFastResult(requestCtx, &a, result)
	require.True(t, refillBillsOpenAIFastAtStandard(result, &a, "priority"), "in-flight request keeps Standard pricing after disable")
	require.False(t, refillBillsOpenAIFastAtStandard(result, &a, "ultrafast"))
	a.ID = 2
	require.False(t, refillAutoFastForRequest(requestCtx, &a), "failover cannot inherit another account's benefit")
}

func TestRefillAutoFastRespectsGroupAndGlobalPolicies(t *testing.T) {
	for _, mode := range []string{"filter", "ultrafast", "global_filter", "global_block", "other_group"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now()
			refill := &AccountRefillService{}
			refill.publishRefillAutoFast(2, 1, map[int64]time.Time{1: now.Add(time.Hour)}, now)
			s := newOpenAIGatewayServiceWithSettings(t, nil)
			s.accountRefill = refill
			_, a := autoFastFixture(now)
			group := &Group{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
			switch mode {
			case "filter":
				group.FilterOpenAIServiceTier = true
			case "ultrafast":
				group.ForceOpenAIUltrafast = true
			case "other_group":
				group.ID = 3
			}
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			if mode == "global_filter" || mode == "global_block" {
				policy := openAIFastFilterPriorityPolicy()
				if mode == "global_block" {
					policy.Rules[0].Action = BetaPolicyActionBlock
				}
				ctx = withOpenAIFastPolicyContext(ctx, policy)
			}
			out, err := s.applyOpenAIFastPolicyToBody(ctx, &a, "gpt-5.5", []byte(`{"model":"gpt-5.5"}`))
			if mode == "global_block" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if mode == "ultrafast" {
				require.Equal(t, "ultrafast", gjson.GetBytes(out, "service_tier").String())
			} else {
				require.False(t, gjson.GetBytes(out, "service_tier").Exists())
			}
		})
	}
}

func TestRefillAutoFastValidation(t *testing.T) {
	for _, change := range []func(*AccountRefillAutoFastConfig){
		func(c *AccountRefillAutoFastConfig) { c.StopMode = "bad" },
		func(c *AccountRefillAutoFastConfig) { n := -1; c.LeadMinutes = &n },
		func(c *AccountRefillAutoFastConfig) { c.StartRemainingPercent = 101 },
		func(c *AccountRefillAutoFastConfig) { c.StartRemainingPercent = math.NaN() },
		func(c *AccountRefillAutoFastConfig) { c.StopMode = "custom"; c.StopUsed5hPercent = 0 },
		func(c *AccountRefillAutoFastConfig) { c.StopMode = "custom"; c.StopUsed7dPercent = 101 },
	} {
		c, _ := autoFastFixture(time.Now())
		change(c.AutoFast)
		require.Error(t, c.Validate())
	}
}

func TestRefillAutoFastUsageChargesStandardOnlyForCapturedAccountRequest(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
		svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
		svc.resolver = NewModelPricingResolver(nil, svc.billingService)
		groupID := int64(2)
		inputPrice, outputPrice, fastMultiplier := 0.001, 0.002, 3.0
		tier := "priority"
		apiKey := &APIKey{ID: 1020, GroupID: &groupID, Group: &Group{
			ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 0.5,
			ModelPricing: []ChannelModelPricing{{Models: []string{"gpt-5.6-sol"}, BillingMode: BillingModeToken,
				InputPrice: &inputPrice, OutputPrice: &outputPrice, FastMultiplier: &fastMultiplier}},
		}}
		err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
			Result: &OpenAIForwardResult{RequestID: "auto-fast-billing", Model: "gpt-5.6-sol", ServiceTier: &tier,
				RefillAutoFast: automatic, UpstreamResponseServiceTier: "default", Duration: time.Second,
				Usage: OpenAIUsage{InputTokens: 100, OutputTokens: 50}},
			APIKey: apiKey, User: &User{ID: 2020}, Account: &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		})
		require.NoError(t, err)
		require.NotNil(t, usageRepo.lastLog)
		require.Equal(t, "priority", *usageRepo.lastLog.ServiceTier)
		require.InDelta(t, 0.6, usageRepo.lastLog.TotalCost, 1e-10, "record actual upstream Fast cost")
		expected := 0.3
		if automatic {
			expected = 0.1
		}
		require.InDelta(t, expected, usageRepo.lastLog.ActualCost, 1e-10, "only the captured automatic Fast request gets Standard customer pricing")
		require.False(t, apiKey.Group.FreeOpenAIFast, "never modify shared group settings")
	}
}

func TestRefillAutoFastHTTPEntrypointsCaptureFreeFast(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{"responses", "passthrough", "chat", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			now := time.Now()
			refill := &AccountRefillService{}
			refill.publishRefillAutoFast(2, 1, map[int64]time.Time{1: now.Add(time.Hour)}, now)
			_, account := autoFastFixture(now)
			account.Credentials = map[string]any{"access_token": "test-oauth", "chatgpt_account_id": "test-account"}
			if protocol == "passthrough" {
				account.Extra["openai_passthrough"] = true
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:   io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_auto_fast\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, accountRefill: refill, httpUpstream: upstream, cache: &stubGatewayCache{}}
			group := &Group{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			body := []byte(`{"model":"gpt-5.5","stream":false,"input":"hello"}`)
			path := "/v1/responses"
			if protocol == "chat" || protocol == "messages" {
				body = []byte(`{"model":"gpt-5.5","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
				path = "/v1/chat/completions"
				if protocol == "messages" {
					path = "/v1/messages"
				}
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			var result *OpenAIForwardResult
			var err error
			switch protocol {
			case "chat":
				result, err = svc.ForwardAsChatCompletions(ctx, c, &account, body, "", "")
			case "messages":
				result, err = svc.ForwardAsAnthropic(ctx, c, &account, body, "", "")
			default:
				result, err = svc.Forward(ctx, c, &account, body)
			}
			require.NoError(t, err)
			require.Equal(t, "priority", gjson.GetBytes(upstream.lastBody, "service_tier").String())
			require.NotNil(t, result)
			require.True(t, result.RefillAutoFast)
			require.Equal(t, "priority", *result.ServiceTier)
		})
	}
}
