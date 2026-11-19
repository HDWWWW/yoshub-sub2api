package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIImportThresholdAPI(t *testing.T) {
	repo := &astraPriorityHandlerRepo{}
	handler := &SettingHandler{settingService: service.NewSettingService(repo, nil)}
	request := func(method, body string, scoped bool) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(method, "/accounts/refill/import-threshold", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		if scoped {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{KeyID: "scoped"}))
		}
		if method == http.MethodGet {
			handler.GetOpenAIImportThreshold(c)
		} else {
			handler.SetOpenAIImportThreshold(c)
		}
		return rec
	}
	read := func() int {
		res := request(http.MethodGet, "", false)
		require.Equal(t, http.StatusOK, res.Code)
		var result struct {
			Data struct {
				Percent int `json:"percent"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(res.Body.Bytes(), &result))
		return result.Data.Percent
	}
	require.Equal(t, 99, read())
	for _, body := range []string{`{}`, `{"percent":null}`, `{"percent":0}`, `{"percent":101}`, `{"percent":1.5}`} {
		require.Equal(t, http.StatusBadRequest, request(http.MethodPut, body, false).Code)
	}
	require.Equal(t, http.StatusForbidden, request(http.MethodGet, "", true).Code)
	require.Equal(t, http.StatusForbidden, request(http.MethodPut, `{"percent":5}`, true).Code)
	require.Zero(t, repo.calls)
	require.Equal(t, http.StatusOK, request(http.MethodPut, `{"percent":1}`, false).Code)
	require.Equal(t, 1, read())
	require.Equal(t, http.StatusOK, request(http.MethodPut, `{"percent":100}`, false).Code)
	require.Equal(t, 100, read())
}
