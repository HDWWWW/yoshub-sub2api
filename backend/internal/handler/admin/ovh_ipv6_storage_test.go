package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type ovhStorageAdminStub struct {
	service.AdminService
	calls int
}

func (s *ovhStorageAdminStub) GetOVHIPv6Storage(context.Context) (*ovhipv6.StorageStats, error) {
	s.calls++
	return &ovhipv6.StorageStats{Files: ovhipv6.StorageFiles{Error: "未配置统计挂载"}}, nil
}

func TestOVHIPv6StorageAdminOnlyAndNoCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scoped := range []bool{false, true} {
		svc := &ovhStorageAdminStub{}
		h := &ProxyHandler{adminService: svc}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/proxies/ovh-ipv6/storage", nil)
		if scoped {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{KeyID: "scoped"}))
		}
		h.GetOVHIPv6Storage(c)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		if scoped {
			require.Equal(t, 403, w.Code)
			require.Zero(t, svc.calls)
		} else {
			require.Equal(t, 200, w.Code, "unavailable sections are still reviewable")
			require.Equal(t, 1, svc.calls)
			require.Contains(t, w.Body.String(), `"issued":null`)
			require.Contains(t, w.Body.String(), `"counter_bytes":null`)
		}
	}
}
