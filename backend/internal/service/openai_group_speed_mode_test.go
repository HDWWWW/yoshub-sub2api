package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGroupSpeedModesHTTPAndWebSocket(t *testing.T) {
	for _, mode := range []string{"off", "priority", "ultrafast", "filter"} {
		for _, tier := range []string{"", "null", `""`, `"priority"`, `"fast"`, `"ultrafast"`, `"flex"`, `"default"`, `"auto"`, `"scale"`} {
			t.Run(mode+"/"+tier, func(t *testing.T) {
				svc := newOpenAIGatewayServiceWithSettings(t, DefaultOpenAIFastPolicySettings())
				group := &Group{ID: 7, Platform: PlatformComposite, Status: StatusActive, Hydrated: true,
					ForceOpenAIFast: mode == "priority", ForceOpenAIUltrafast: mode == "ultrafast", FilterOpenAIServiceTier: mode == "filter"}
				ctx := context.WithValue(context.Background(), ctxkey.Group, group)
				account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
				body := `{"type":"response.create","model":"gpt-6.1-sol","input":"keep me"`
				if tier != "" {
					body += `,"service_tier":` + tier
				}
				body += `}`
				httpBody, err := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6.1-sol", []byte(body))
				require.NoError(t, err)
				wsBody, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, "gpt-6.1-sol", []byte(body))
				require.NoError(t, err)
				require.Nil(t, blocked)
				require.JSONEq(t, string(httpBody), string(wsBody))
				require.Equal(t, "keep me", gjson.GetBytes(httpBody, "input").String())
				switch mode {
				case "priority", "ultrafast":
					require.Equal(t, mode, gjson.GetBytes(httpBody, "service_tier").String())
				case "filter":
					require.False(t, gjson.GetBytes(httpBody, "service_tier").Exists())
				default:
					want := gjson.Get(body, "service_tier").String()
					if want == "fast" {
						want = "priority"
					}
					require.Equal(t, want, gjson.GetBytes(httpBody, "service_tier").String())
				}
				cancel := []byte(`{"type":"response.cancel","service_tier":"priority"}`)
				untouched, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, "gpt-6.1-sol", cancel)
				require.NoError(t, err)
				require.Nil(t, blocked)
				require.Equal(t, cancel, untouched)
			})
		}
	}
}

func TestGroupSpeedModeGlobalPolicyPrecedence(t *testing.T) {
	for _, mode := range []string{"ultrafast", "filter"} {
		for _, action := range []string{BetaPolicyActionPass, BetaPolicyActionFilter, BetaPolicyActionBlock, OpenAIFastPolicyActionForcePriority} {
			t.Run(mode+"/"+action, func(t *testing.T) {
				svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
					ServiceTier: OpenAIFastTierAny, Action: action, Scope: BetaPolicyScopeAll, ErrorMessage: "blocked tier",
				}}})
				ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{ID: 7, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true,
					ForceOpenAIUltrafast: mode == "ultrafast", FilterOpenAIServiceTier: mode == "filter"})
				account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				body := []byte(`{"type":"response.create","service_tier":"priority"}`)
				httpBody, httpErr := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6.1-sol", body)
				wsBody, blocked, err := svc.applyOpenAIFastPolicyToWSResponseCreate(ctx, account, "gpt-6.1-sol", body)
				require.NoError(t, err)
				if action == BetaPolicyActionBlock {
					var denied *OpenAIFastBlockedError
					require.ErrorAs(t, httpErr, &denied)
					require.Equal(t, "blocked tier", denied.Message)
					require.Equal(t, denied, blocked)
					return
				}
				require.NoError(t, httpErr)
				require.Nil(t, blocked)
				require.JSONEq(t, string(httpBody), string(wsBody))
				if mode == "filter" || action == BetaPolicyActionFilter {
					require.False(t, gjson.GetBytes(httpBody, "service_tier").Exists())
				} else if action == OpenAIFastPolicyActionForcePriority {
					require.Equal(t, "priority", gjson.GetBytes(httpBody, "service_tier").String())
				} else {
					require.Equal(t, "ultrafast", gjson.GetBytes(httpBody, "service_tier").String())
				}
			})
		}
	}
}

func TestGroupFilterPreventsMissingTierUpgrade(t *testing.T) {
	svc := newOpenAIGatewayServiceWithSettings(t, &OpenAIFastPolicySettings{Rules: []OpenAIFastPolicyRule{{
		ServiceTier: OpenAIFastTierMissing, Action: OpenAIFastPolicyActionForcePriority, Scope: BetaPolicyScopeAll,
	}}})
	ctx := context.WithValue(context.Background(), ctxkey.Group, &Group{ID: 7, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, FilterOpenAIServiceTier: true})
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.False(t, svc.shouldForceOpenAIFastPriorityForMissingTier(ctx, account, "gpt-6.1-sol"))
	body := []byte(`{"type":"response.create","model":"gpt-6.1-sol"}`)
	got, err := svc.applyOpenAIFastPolicyToBody(ctx, account, "gpt-6.1-sol", body)
	require.NoError(t, err)
	require.Equal(t, body, got)
}

func TestGroupSpeedModeRequiresTrustedOpenAIContext(t *testing.T) {
	for _, filter := range []bool{false, true} {
		for _, scope := range []string{"unhydrated", "other-account", "other-group", "nil-account", "nil-context"} {
			t.Run(scope, func(t *testing.T) {
				group := &Group{ID: 7, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, ForceOpenAIUltrafast: !filter, FilterOpenAIServiceTier: filter}
				account := &Account{Platform: PlatformOpenAI}
				if scope == "unhydrated" {
					group.Hydrated = false
				}
				if scope == "other-account" {
					account.Platform = PlatformGrok
				}
				if scope == "other-group" {
					group.Platform = PlatformAnthropic
				}
				if scope == "nil-account" {
					account = nil
				}
				var ctx = context.WithValue(context.Background(), ctxkey.Group, group)
				if scope == "nil-context" {
					ctx = nil
				}
				require.Empty(t, openAIGroupForcedServiceTier(ctx, account, "gpt-6.1-sol"))
				require.False(t, openAIGroupFiltersServiceTier(ctx, account))
			})
		}
	}
}
