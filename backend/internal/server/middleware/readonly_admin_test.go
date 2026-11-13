//go:build unit

package middleware

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestReadOnlyAdminJWTAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	auth := service.NewAuthService(nil, nil, nil, nil, &config.Config{JWT: config.JWTConfig{Secret: "readonly-test", ExpireHour: 1}}, nil, nil, nil, nil, nil, nil, nil, nil)
	user := &service.User{ID: 7, Role: service.RoleReadOnlyAdmin, Status: service.StatusActive}
	users := service.NewUserService(&stubUserRepo{getByID: func(context.Context, int64) (*service.User, error) { return user, nil }}, nil, nil, nil)
	// Deliberately stale full-admin claim: the current database role must win.
	token, err := auth.GenerateToken(context.Background(), &service.User{ID: user.ID, Role: service.RoleAdmin})
	require.NoError(t, err)
	router := gin.New()
	router.Use(gin.HandlerFunc(NewAdminAuthMiddleware(auth, users, nil, nil)))
	called := 0
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/accounts"}, {"PUT", "/api/v1/admin/accounts/:id"},
		{"GET", "/api/v1/admin/settings/admin-api-key"}, {"GET", "/api/v1/admin/ops/smart-ws"},
		{"POST", "/api/v1/admin/payment/orders/:id/refund"}, {"GET", "/api/v1/admin/ops/ws/qps"},
	} {
		router.Handle(route.method, route.path, func(c *gin.Context) {
			called++
			response.Success(c, gin.H{"name": "visible", "smart_ws_mode": "enabled", "key": "never-visible"})
		})
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/v1/admin/accounts", 200}, {"PUT", "/api/v1/admin/accounts/2", 403},
		{"GET", "/api/v1/admin/settings/admin-api-key", 403}, {"GET", "/api/v1/admin/ops/smart-ws", 403},
		{"POST", "/api/v1/admin/payment/orders/2/refund", 403},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		require.Equal(t, tc.status, res.Code, tc.path)
		require.NotContains(t, res.Body.String(), "never-visible")
		require.NotContains(t, res.Body.String(), "smart_ws_mode")
	}
	require.Equal(t, 1, called, "denied requests must never reach a handler")

	for _, path := range []string{"/api/v1/admin/ops/ws/qps", "/api/v1/admin/ops/smart-ws"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Sec-WebSocket-Protocol", "sub2api-admin, jwt."+token)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if path == "/api/v1/admin/ops/ws/qps" {
			require.Equal(t, 200, res.Code)
		} else {
			require.Equal(t, 403, res.Code)
		}
	}

	user.Role = service.RoleAdmin
	req := httptest.NewRequest("PUT", "/api/v1/admin/accounts/2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	require.Equal(t, 200, res.Code)
	require.Contains(t, res.Body.String(), "never-visible", "full administrator response is unchanged")
}

func TestReadOnlyAdminCannotUsePersonalKeysOrMCP(t *testing.T) {
	user := &service.User{ID: 1, Role: service.RoleReadOnlyAdmin, Status: service.StatusActive}
	router, auth := newJWTTestEnv(map[int64]*service.User{1: user})
	token, err := auth.GenerateToken(context.Background(), user)
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/keys", "/api/v1/keys/1", "/api/v1/user/mcp/authorizations"} {
		for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
			router.Handle(method, path, func(c *gin.Context) { c.Status(200) })
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			require.Equal(t, 403, res.Code, method+" "+path)
		}
	}
	// Personal account security and native user access remain available.
	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	require.Equal(t, 200, res.Code)
	user.Role = service.RoleUser
	req = httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res = httptest.NewRecorder()
	router.ServeHTTP(res, req)
	require.Equal(t, 200, res.Code)
}
