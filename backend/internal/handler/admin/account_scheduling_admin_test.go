//go:build unit

package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type schedulingAdminStub struct {
	service.AdminService
	calls int
}

func (s *schedulingAdminStub) GetAccountsByIDs(context.Context, []int64) ([]*service.Account, error) {
	s.calls++
	return []*service.Account{{ID: 1, Credentials: map[string]any{"access_token": "NEVER_RETURN"}, Extra: map[string]any{"supplier_secret": "NEVER_RETURN"}}}, nil
}
func (s *schedulingAdminStub) AccountSchedulingCandidates(context.Context, int64, string) ([]service.Account, error) {
	s.calls++
	return []service.Account{{ID: 1, Credentials: map[string]any{"access_token": "NEVER_RETURN"}}}, nil
}
func (s *schedulingAdminStub) GetProxyBindingLimits(context.Context, []int64) ([]service.ProxyBindingLimit, error) {
	s.calls++
	return []service.ProxyBindingLimit{{ProxyID: 1, MaxAccounts: 1}}, nil
}
func (s *schedulingAdminStub) SetProxyBindingLimits(context.Context, []int64, int) error {
	s.calls++
	return nil
}

func TestSchedulingExtensionEndpointsRedactAndRejectScopedKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scoped := range []bool{false, true} {
		s := &schedulingAdminStub{}
		h := &AccountHandler{adminService: s}
		p := &ProxyHandler{adminService: s}
		r := gin.New()
		if scoped {
			r.Use(func(c *gin.Context) {
				c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{}))
				c.Next()
			})
		}
		r.POST("/metadata", h.GetAccountSchedulingMetadata)
		r.GET("/preview", h.GetAccountSchedulingPreview)
		r.POST("/limits", p.GetProxyBindingLimits)
		r.PUT("/limits", p.SetProxyBindingLimits)
		for _, tc := range []struct{ method, path, body string }{
			{"POST", "/metadata", `{"account_ids":[1]}`}, {"GET", "/preview", ""},
			{"POST", "/limits", `{"proxy_ids":[1]}`}, {"PUT", "/limits", `{"proxy_ids":[1],"max_accounts":2}`},
		} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			expected := 200
			if scoped {
				expected = 403
			}
			require.Equal(t, expected, w.Code, w.Body.String())
			require.NotContains(t, w.Body.String(), "NEVER_RETURN")
			require.NotContains(t, w.Body.String(), "credentials")
		}
		if scoped {
			require.Zero(t, s.calls)
		}
	}
}

func TestSchedulingMetadataRejectsUnboundedOrInvalidIDs(t *testing.T) {
	h := &AccountHandler{adminService: &schedulingAdminStub{}}
	r := gin.New()
	r.POST("/metadata", h.GetAccountSchedulingMetadata)
	for _, body := range []string{`{}`, `{"account_ids":[0]}`, `{"account_ids":[1,1]}`, `{"account_ids":[1.5]}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/metadata", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		require.Equal(t, 400, w.Code, w.Body.String())
	}
}
