package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type ovhAdminStub struct {
	service.AdminService
	enabled bool
	calls   int
	err     error
}

func (s *ovhAdminStub) GetOVHIPv6(context.Context) (*ovhipv6.Status, error) {
	return &ovhipv6.Status{Configured: true, Available: true, Enabled: s.enabled}, s.err
}
func (s *ovhAdminStub) SetOVHIPv6(_ context.Context, enabled bool) (*ovhipv6.Status, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	s.enabled = enabled
	return s.GetOVHIPv6(context.Background())
}

func (s *ovhAdminStub) AllocateOVHIPv6(_ context.Context, name string) (*service.Proxy, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &service.Proxy{ID: 42, Name: name, ManagedIPv6: "2604:2dc0:205:b900::42", ManagedIPv6State: "reserved", Password: "must-not-be-returned"}, nil
}

func TestOVHIPv6AllocateAdminOnlyAndSafeDTO(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scoped := range []bool{false, true} {
		svc := &ovhAdminStub{}
		h := &ProxyHandler{adminService: svc}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/proxies/ovh-ipv6/allocate", strings.NewReader(`{"name":"reserved proxy"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		if scoped {
			c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{KeyID: "scoped"}))
		}
		h.AllocateOVHIPv6(c)
		if scoped {
			require.Equal(t, 403, w.Code)
			require.Zero(t, svc.calls)
		} else {
			require.Equal(t, 200, w.Code)
			require.Equal(t, 1, svc.calls)
			require.Contains(t, w.Body.String(), `"managed_ipv6_state":"reserved"`)
			require.NotContains(t, w.Body.String(), "must-not-be-returned")
		}
	}
}
func TestOVHIPv6AdminToggleAndPermissions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, body    string
		scoped, fail  bool
		status, calls int
		enabled       bool
	}{
		{"enable", `{"enabled":true}`, false, false, 200, 1, true},
		{"disable", `{"enabled":false}`, false, false, 200, 1, false},
		{"missing", `{}`, false, false, 400, 0, false},
		{"null", `{"enabled":null}`, false, false, 400, 0, false},
		{"type", `{"enabled":"true"}`, false, false, 400, 0, false},
		{"scoped", `{"enabled":true}`, true, false, 403, 0, false},
		{"unavailable", `{"enabled":true}`, false, true, 400, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &ovhAdminStub{}
			if tc.fail {
				svc.err = errors.New("出口未就绪")
			}
			h := &ProxyHandler{adminService: svc}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPut, "/proxies/ovh-ipv6", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.scoped {
				c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{KeyID: "scoped"}))
			}
			h.SetOVHIPv6(c)
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, tc.calls, svc.calls)
			require.Equal(t, tc.enabled, svc.enabled)
		})
	}
}
