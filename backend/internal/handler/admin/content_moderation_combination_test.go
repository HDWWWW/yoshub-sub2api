package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestContentModerationCombinationHandlerRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := &moderationEngineSettings{value: `{}`}
	svc := service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	handler := NewContentModerationHandler(svc)
	router := gin.New()
	router.PUT("/config", handler.UpdateConfig)
	router.POST("/test", handler.TestCombinationRules)
	rule := `{"id":"custom","name":"Custom rule","enabled":true,"action":"block","scope":"conversation","window_characters":100,"groups":[{"name":"first","keywords":["猫"]},{"name":"second","keywords":["画"]}]}`
	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	w := call(http.MethodPut, "/config", `{"combination_rules":[`+rule+`]}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	saved := settings.value
	require.Contains(t, saved, `"combination_rules"`)
	w = call(http.MethodPost, "/test", `{"rules":[`+rule+`],"text":"请画一只猫"}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	var result struct {
		Data struct {
			Matches []service.ContentModerationCombinationHit `json:"matches"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Len(t, result.Data.Matches, 1)
	require.Equal(t, saved, settings.value, "testing a draft never modifies saved settings")
	w = call(http.MethodPut, "/config", `{"combination_rules":[`+rule+`,`+rule+`]}`)
	require.Equal(t, 400, w.Code)
	require.Equal(t, saved, settings.value)
	w = call(http.MethodPut, "/config", `{}`)
	require.Equal(t, 200, w.Code)
	require.Contains(t, settings.value, `"id":"custom"`)
	w = call(http.MethodPut, "/config", `{"combination_rules":[]}`)
	require.Equal(t, 200, w.Code)
	require.Contains(t, settings.value, `"combination_rules":[]`)
}
