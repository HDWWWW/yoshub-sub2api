package service

import (
	"context"
	"maps"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
)

func normalizeSmartWSMode(platform, mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "inherit"
	}
	if mode != "inherit" && mode != "enabled" && mode != "disabled" {
		return "", infraerrors.BadRequest("INVALID_SMART_WS_MODE", "smart_ws_mode must be inherit, enabled or disabled")
	}
	if platform != PlatformOpenAI && platform != PlatformComposite {
		return "inherit", nil
	}
	return mode, nil
}

func smartWSGroupMode(ctx context.Context) string {
	if ctx == nil {
		return "inherit"
	}
	group, _ := ctx.Value(ctxkey.Group).(*Group)
	if !IsGroupContextValid(group) {
		return "inherit"
	}
	mode, _ := normalizeSmartWSMode(group.Platform, group.SmartWSMode)
	return mode
}

func smartWSRequestGroupMode(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return "inherit"
	}
	return smartWSGroupMode(c.Request.Context())
}

func (s *OpenAIGatewayService) smartWSDecision(c *gin.Context, account *Account) OpenAIWSProtocolDecision {
	if c != nil && c.Request != nil && strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/messages") {
		return openAIWSHTTPDecision("messages_endpoint")
	}
	if account == nil {
		return openAIWSHTTPDecision("account_missing")
	}
	if smartWSRequestGroupMode(c) != "enabled" {
		return openAIWSHTTPDecision("group_smart_ws_not_enabled")
	}
	return s.smartWSEligibleAccount(account)
}

// SupportsSmartWSHTTPContinuation uses the same transport policy as dispatch.
// The HTTP handler must not reject OAuth continuation that will use WS, nor
// admit it on compact, disabled groups, force-HTTP accounts or plugin routes.
func (s *OpenAIGatewayService) SupportsSmartWSHTTPContinuation(c *gin.Context, account *Account) bool {
	return s != nil && c != nil && c.Request != nil &&
		strings.HasSuffix(strings.TrimRight(c.Request.URL.Path, "/"), "/responses") &&
		s.smartWSDecision(c, account).Transport == OpenAIUpstreamTransportResponsesWebsocketV2
}

// A group opt-in never declares arbitrary compatible hosts WS-capable. Custom
// API-key providers must also opt in with the existing account WS setting.
func (s *OpenAIGatewayService) smartWSEligibleAccount(account *Account) OpenAIWSProtocolDecision {
	if account == nil || !account.IsOpenAI() || account.IsExcelOAuth() {
		return openAIWSHTTPDecision("smart_ws_account_not_supported")
	}
	if s.pluginManager != nil && s.pluginManager.ShouldRouteOpenAIOAuth(account) {
		return openAIWSHTTPDecision("openai_oauth_plugin")
	}
	if account.IsOpenAIApiKey() {
		base := account.GetOpenAIBaseURL()
		u, err := url.Parse(base)
		official := base == "" || (err == nil && u.Scheme == "https" && u.Host == "api.openai.com")
		if !official {
			return s.getOpenAIWSProtocolResolver().Resolve(account)
		}
	}
	forced := *account
	forced.Extra = maps.Clone(account.Extra)
	if forced.Extra == nil {
		forced.Extra = make(map[string]any)
	}
	for _, prefix := range []string{"openai_oauth_", "openai_apikey_"} {
		forced.Extra[prefix+"responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
		forced.Extra[prefix+"responses_websockets_v2_enabled"] = true
	}
	return s.getOpenAIWSProtocolResolver().Resolve(&forced)
}
