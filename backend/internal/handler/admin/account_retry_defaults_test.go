package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDefaultAccountRetryAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		body   string
		status int
		scoped bool
	}{
		{`{"count":0}`, 200, false}, {`{"count":3}`, 200, false}, {`{"count":10}`, 200, false},
		{`{}`, 400, false}, {`{"count":null}`, 400, false}, {`{"count":-1}`, 400, false},
		{`{"count":11}`, 400, false}, {`{"count":1.5}`, 400, false}, {`{"count":3}`, 403, true},
	} {
		repo := &astraPriorityHandlerRepo{}
		h := &SettingHandler{settingService: service.NewSettingService(repo, nil)}
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/accounts/retry-default", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		if tc.scoped {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{KeyID: "scoped"}))
		}
		h.SetDefaultAccountRetryCount(c)
		require.Equal(t, tc.status, rec.Code, tc.body)
		if tc.status != 200 {
			require.Zero(t, repo.calls)
		}
	}
}
