package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

func TestAccountForceFastHTTPAndWebSocket(t *testing.T) {
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeAPIKey} {
		for _, tier := range []string{"", "null", `"default"`, `"auto"`, `"scale"`, `"flex"`, `"fast"`, `"priority"`, `"ultrafast"`} {
			t.Run(accountType+"/"+tier, func(t *testing.T) {
				svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
				account := &Account{Platform: PlatformOpenAI, Type: accountType, Extra: map[string]any{OpenAIForceFastExtraKey: true}}
				body := `{"type":"response.create","model":"gpt-6.1-sol","input":"keep me"`
				if tier != "" {
					body += `,"service_tier":` + tier
				}
				body += `}`
				got, err := svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-6.1-sol", []byte(body))
				require.NoError(t, err)
				ws, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(context.Background(), account, "gpt-6.1-sol", []byte(body))
				require.NoError(t, err)
				require.Nil(t, blocked)
				require.JSONEq(t, string(got), string(ws))
				require.Equal(t, "priority", gjson.GetBytes(got, "service_tier").String())
				require.Equal(t, "keep me", gjson.GetBytes(got, "input").String())

				// Closing the switch restores the request's original tier selection.
				account.Extra[OpenAIForceFastExtraKey] = false
				got, err = svc.applyOpenAIFastPolicyToBody(context.Background(), account, "gpt-6.1-sol", []byte(body))
				require.NoError(t, err)
				want := gjson.Get(body, "service_tier").String()
				if want == "fast" {
					want = "priority"
				}
				require.Equal(t, want, gjson.GetBytes(got, "service_tier").String())
			})
		}
	}
}

func TestAccountForceFastPrecedence(t *testing.T) {
	for _, groupMode := range []string{"off", "priority", "ultrafast", "both", "filter"} {
		for _, action := range []string{BetaPolicyActionPass, BetaPolicyActionFilter, BetaPolicyActionBlock} {
			t.Run(groupMode+"/"+action, func(t *testing.T) {
				svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
					ServiceTier: OpenAIFastTierPriority, Action: action, Scope: BetaPolicyScopeAll,
				}}})
				group := &Group{ID: 7, Platform: PlatformComposite, Status: StatusActive, Hydrated: true,
					ForceOpenAIFast: groupMode == "priority" || groupMode == "both", ForceOpenAIUltrafast: groupMode == "ultrafast" || groupMode == "both", FilterOpenAIServiceTier: groupMode == "filter"}
				ctx := context.WithValue(context.Background(), ctxkey.Group, group)
				account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{OpenAIForceFastExtraKey: true}}
				body := []byte(`{"type":"response.create","model":"gpt-6.1-sol"}`)
				got, httpErr := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6.1-sol", body)
				ws, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, "gpt-6.1-sol", body)
				require.NoError(t, err)
				if action == BetaPolicyActionBlock {
					var denied *OpenAIFastBlockedError
					require.ErrorAs(t, httpErr, &denied)
					require.NotNil(t, blocked)
					return
				}
				require.NoError(t, httpErr)
				require.Nil(t, blocked)
				require.JSONEq(t, string(got), string(ws))
				if action == BetaPolicyActionFilter {
					require.False(t, gjson.GetBytes(got, "service_tier").Exists())
				} else {
					require.Equal(t, "priority", gjson.GetBytes(got, "service_tier").String())
				}
			})
		}
	}
}

func TestAccountForceFastScopeAndControlFrames(t *testing.T) {
	svc := &OpenAIGatewayService{}
	for _, account := range []*Account{nil, {Platform: PlatformOpenAI},
		{Platform: PlatformOpenAI, Extra: map[string]any{OpenAIForceFastExtraKey: "true"}},
		{Platform: PlatformAnthropic, Extra: map[string]any{OpenAIForceFastExtraKey: true}}} {
		require.False(t, account.IsOpenAIForceFastEnabled())
		require.False(t, billsOpenAIFastAtStandard(nil, account, "priority"))
	}
	account := &Account{Platform: PlatformOpenAI, Extra: map[string]any{OpenAIForceFastExtraKey: true}}
	for _, frame := range []string{`{"type":"response.cancel"}`, `{"type":"session.update"}`, `{"type":"conversation.item.create"}`, `invalid json`} {
		got, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(context.Background(), account, "gpt-6.1-sol", []byte(frame))
		require.NoError(t, err)
		require.Nil(t, blocked)
		require.Equal(t, frame, string(got))
	}
	for _, tier := range []string{"", "default", "flex"} {
		require.False(t, billsOpenAIFastAtStandard(nil, account, tier))
	}
}

func TestAccountForceFastForwardResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%t/enabled=%t", passthrough, enabled), func(t *testing.T) {
				body := []byte(`{"model":"gpt-5.6-sol","stream":true,"instructions":"test","input":"hi","service_tier":"flex"}`)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1")
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
				}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
					Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
					Extra: map[string]any{"openai_passthrough": passthrough, OpenAIForceFastExtraKey: enabled,
						"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff},
					Status: StatusActive, Schedulable: true}
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				want := "flex"
				if enabled {
					want = "priority"
				}
				require.Equal(t, want, gjson.GetBytes(upstream.lastBody, "service_tier").String())
				require.NotNil(t, result.ServiceTier)
				require.Equal(t, want, *result.ServiceTier)
			})
		}
	}
}

type accountFastSubscriptionRepo struct {
	openAIRecordUsageSubRepoStub
	amount float64
}

func TestAccountForceFastChatAndMessagesForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"chat/completions", "messages"} {
		for _, responsesSupported := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/responses=%t", endpoint, responsesSupported), func(t *testing.T) {
				body := []byte(`{"model":"gpt-5.6-sol","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/"+endpoint, bytes.NewReader(body))
				payload := `{"id":"chatcmpl_fast","object":"chat.completion","model":"gpt-5.6-sol","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`
				contentType, upstreamPath := "application/json", "/v1/chat/completions"
				if responsesSupported {
					contentType, upstreamPath = "text/event-stream", "/v1/responses"
					payload = "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_fast","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":100,"output_tokens":50}}}` + "\n\n"
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
					Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(payload))}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
					Credentials: map[string]any{"api_key": "test-key"},
					Extra:       map[string]any{OpenAIForceFastExtraKey: true, "openai_responses_supported": responsesSupported}}
				var result *OpenAIForwardResult
				var err error
				if endpoint == "messages" {
					result, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")
				} else {
					result, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, upstreamPath, upstream.lastReq.URL.Path)
				require.Equal(t, "priority", gjson.GetBytes(upstream.lastBody, "service_tier").String())
				require.NotNil(t, result.ServiceTier)
				require.Equal(t, "priority", *result.ServiceTier)
			})
		}
	}
}

func (s *accountFastSubscriptionRepo) IncrementUsage(ctx context.Context, id int64, amount float64) error {
	s.amount = amount
	return s.openAIRecordUsageSubRepoStub.IncrementUsage(ctx, id, amount)
}

func TestAccountForceFastRecordUsageChargesStandard(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, subscription := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/subscription=%t", enabled, subscription), func(t *testing.T) {
				usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
				userRepo := &openAIRecordUsageUserRepoStub{}
				subRepo := &accountFastSubscriptionRepo{}
				svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, subRepo, nil)
				svc.resolver = NewModelPricingResolver(nil, svc.billingService)
				inputPrice, outputPrice, fastMultiplier := 0.001, 0.002, 3.0
				group := &Group{ID: 77, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 0.5,
					ModelPricing: []ChannelModelPricing{{Models: []string{"gpt-5.6-sol"}, BillingMode: BillingModeToken,
						InputPrice: &inputPrice, OutputPrice: &outputPrice, FastMultiplier: &fastMultiplier}}}
				quota := &openAIRecordUsageAPIKeyQuotaStub{}
				tier := "priority"
				input := &OpenAIRecordUsageInput{
					Result: &OpenAIForwardResult{RequestID: "resp_account_fast", Model: "gpt-5.6-sol", ServiceTier: &tier,
						UpstreamResponseServiceTier: "default", Usage: OpenAIUsage{InputTokens: 100, OutputTokens: 50}, Duration: time.Second},
					APIKey: &APIKey{ID: 10, GroupID: &group.ID, Group: group, Quota: 100},
					User:   &User{ID: 20}, Account: &Account{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
						Extra: map[string]any{OpenAIForceFastExtraKey: enabled}}, APIKeyService: quota,
				}
				if subscription {
					group.SubscriptionType = SubscriptionTypeSubscription
					input.Subscription = &UserSubscription{ID: 99}
				}
				require.NoError(t, svc.RecordUsage(context.Background(), input))
				want := (100*inputPrice + 50*outputPrice) * 0.5
				if !enabled {
					want *= fastMultiplier
				}
				require.NotNil(t, usageRepo.lastLog)
				require.InDelta(t, want, usageRepo.lastLog.ActualCost, 1e-10)
				require.InDelta(t, (100*inputPrice+50*outputPrice)*fastMultiplier, usageRepo.lastLog.TotalCost, 1e-10)
				require.Equal(t, "priority", *usageRepo.lastLog.ServiceTier)
				require.InDelta(t, want, quota.lastAmount, 1e-10)
				if subscription {
					require.InDelta(t, want, subRepo.amount, 1e-10)
					require.Zero(t, userRepo.deductCalls)
				} else {
					require.InDelta(t, want, userRepo.lastAmount, 1e-10)
					require.Equal(t, 1, userRepo.deductCalls)
				}
				require.False(t, group.FreeOpenAIFast, "account policy must not alter other accounts in the group")
			})
		}
	}
}

type accountFastShadowRepository struct {
	AccountRepository
	parent *Account
}

func (r accountFastShadowRepository) GetByID(context.Context, int64) (*Account, error) {
	return r.parent, nil
}

func TestAccountForceFastShadowBillingUsesScheduledAccount(t *testing.T) {
	for _, parentEnabled := range []bool{false, true} {
		for _, shadowEnabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("parent=%t/shadow=%t", parentEnabled, shadowEnabled), func(t *testing.T) {
				usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
				userRepo := &openAIRecordUsageUserRepoStub{}
				svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
				svc.resolver = NewModelPricingResolver(nil, svc.billingService)
				parentID := int64(30)
				svc.accountRepo = accountFastShadowRepository{parent: &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Extra: map[string]any{OpenAIForceFastExtraKey: parentEnabled}}}
				inputPrice, outputPrice, fastMultiplier := 0.001, 0.002, 3.0
				group := &Group{ID: 77, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RateMultiplier: 0.5,
					ModelPricing: []ChannelModelPricing{{Models: []string{"gpt-5.6-sol"}, BillingMode: BillingModeToken,
						InputPrice: &inputPrice, OutputPrice: &outputPrice, FastMultiplier: &fastMultiplier}}}
				tier := "priority"
				shadow := &Account{ID: 31, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					ParentAccountID: &parentID, QuotaDimension: QuotaDimensionSpark,
					Extra: map[string]any{OpenAIForceFastExtraKey: shadowEnabled}}
				require.NoError(t, svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
					Result: &OpenAIForwardResult{RequestID: "shadow-fast", Model: "gpt-5.6-sol", ServiceTier: &tier,
						Usage: OpenAIUsage{InputTokens: 100, OutputTokens: 50}},
					APIKey: &APIKey{ID: 10, GroupID: &group.ID, Group: group}, User: &User{ID: 20}, Account: shadow,
				}))
				want := (100*inputPrice + 50*outputPrice) * 0.5
				if !shadowEnabled {
					want *= fastMultiplier
				}
				require.InDelta(t, want, userRepo.lastAmount, 1e-10)
				require.InDelta(t, want, usageRepo.lastLog.ActualCost, 1e-10)
				require.InDelta(t, 0.6, usageRepo.lastLog.TotalCost, 1e-10)
				require.Equal(t, shadow.ID, usageRepo.lastLog.AccountID)
			})
		}
	}
}
