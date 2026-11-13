//go:build unit

package routes

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type readOnlyUserRepo struct{ service.UserRepository }

func (*readOnlyUserRepo) GetByID(context.Context, int64) (*service.User, error) {
	return &service.User{ID: 1, Role: service.RoleReadOnlyAdmin, Status: service.StatusActive}, nil
}

func (*readOnlyUserRepo) GetUserAvatar(context.Context, int64) (*service.UserAvatar, error) {
	return nil, nil
}

func TestReadOnlyAdminRejectsEveryRegisteredManagementMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	users := service.NewUserService(&readOnlyUserRepo{}, nil, nil, nil)
	auth := service.NewAuthService(nil, nil, nil, nil, &config.Config{JWT: config.JWTConfig{Secret: "readonly-routes-test", ExpireHour: 1}}, nil, nil, nil, nil, nil, nil, nil, nil)
	authMiddleware := middleware.NewAdminAuthMiddleware(auth, users, nil, nil)
	next := func(c *gin.Context) { c.Next() }
	RegisterAdminRoutes(r.Group("/api/v1"), &handler.Handlers{Admin: &handler.AdminHandlers{}}, authMiddleware, middleware.AuditLogMiddleware(next), middleware.StepUpAuthMiddleware(next), nil, nil)
	RegisterPaymentRoutes(r.Group("/api/v1"), &handler.PaymentHandler{}, &handler.PaymentWebhookHandler{}, &adminhandler.PaymentHandler{}, middleware.JWTAuthMiddleware(next), authMiddleware, middleware.AuditLogMiddleware(next), nil, nil, nil)
	token, err := auth.GenerateToken(context.Background(), &service.User{ID: 1, Role: service.RoleAdmin})
	require.NoError(t, err)
	statistics := map[string]bool{
		"/api/v1/admin/dashboard/users-usage": true, "/api/v1/admin/dashboard/api-keys-usage": true,
		"/api/v1/admin/accounts/usage/batch": true, "/api/v1/admin/accounts/today-stats/batch": true,
		"/api/v1/admin/accounts/lifetime-stats/batch": true, "/api/v1/admin/user-attributes/batch": true,
	}
	checked := 0
	for _, route := range r.Routes() {
		if !strings.HasPrefix(route.Path, "/api/v1/admin/") || route.Method == "GET" || (route.Method == "POST" && statistics[route.Path]) {
			continue
		}
		parts := strings.Split(route.Path, "/")
		for i, part := range parts {
			if strings.HasPrefix(part, ":") {
				parts[i] = "1"
			}
		}
		req := httptest.NewRequest(route.Method, strings.Join(parts, "/"), strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		r.ServeHTTP(res, req)
		require.Equal(t, 403, res.Code, route.Method+" "+route.Path)
		require.Contains(t, res.Body.String(), "ADMIN_READ_ONLY", route.Path)
		checked++
	}
	require.Greater(t, checked, 100)
	t.Logf("Rejected %d registered management mutations before reaching handlers", checked)
}
