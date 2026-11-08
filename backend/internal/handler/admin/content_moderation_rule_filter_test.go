package admin

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRuleFilterHandlerRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := &moderationEngineSettings{value: `{}`}
	h := NewContentModerationHandler(service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil))
	r := gin.New()
	r.GET("/catalog", h.RuleFilterCatalog)
	r.POST("/test", h.TestRuleFilter)
	r.PUT("/config", h.UpdateConfig)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w := call("GET", "/catalog", "")
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), service.ContentModerationRuleVersion)
	config := `{"enabled":true,"mode":"block","threshold":50,"strict_threshold":90,"strict_terminal_enabled":true,"custom_patterns":[],"disabled_patterns":[]}`
	w = call("PUT", "/config", `{"rule_filter":`+config+`}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	saved := settings.value
	w = call("POST", "/test", `{"config":`+config+`,"text":"普通编程帮助"}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"action":"allow"`)
	require.Equal(t, saved, settings.value)
	w = call("POST", "/test", `{"text":"hello"}`)
	require.Equal(t, 400, w.Code)
	w = call("PUT", "/config", `{"rule_filter":{"enabled":true,"mode":"invalid"}}`)
	require.Equal(t, 400, w.Code)
	require.Equal(t, saved, settings.value)
	w = call("PUT", "/config", `{"record_non_hits":true}`)
	require.Equal(t, 200, w.Code)
	require.Contains(t, settings.value, `"rule_filter":{"enabled":true`)
}
