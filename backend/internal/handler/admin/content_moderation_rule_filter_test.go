package admin

import (
	"encoding/json"
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

func TestContentModerationChecksHandlerRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := &moderationEngineSettings{value: `{"api_keys":["retained-secret"],"blocked_keywords":["retained-keyword"]}`}
	h := NewContentModerationHandler(service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil))
	r := gin.New()
	r.PUT("/config", h.UpdateConfig)
	r.GET("/config", h.GetConfig)
	for _, mode := range []string{"disabled", "api_only", "keyword_only", "keyword_and_api"} {
		for _, enabled := range []bool{true, false} {
			cfg := service.DefaultContentModerationRuleFilter()
			cfg.Enabled = enabled
			body, err := json.Marshal(map[string]any{"keyword_blocking_mode": mode, "rule_filter": cfg})
			require.NoError(t, err)
			w := httptest.NewRecorder()
			req := httptest.NewRequest("PUT", "/config", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			require.Equal(t, 200, w.Code, w.Body.String())
			w = httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest("GET", "/config", nil))
			var result struct {
				Data service.ContentModerationConfigView `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			require.Equal(t, mode, result.Data.KeywordBlockingMode)
			require.Equal(t, enabled, result.Data.RuleFilter.Enabled)
			require.NotContains(t, w.Body.String(), "retained-secret")
			require.Contains(t, settings.value, "retained-secret")
			require.Equal(t, []string{"retained-keyword"}, result.Data.BlockedKeywords)
		}
	}
}
