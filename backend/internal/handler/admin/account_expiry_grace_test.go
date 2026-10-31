//go:build unit

package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type expiryGraceAdminStub struct {
	service.AdminService
	a       *service.Account
	minutes *int
}

func (s *expiryGraceAdminStub) GetAccount(context.Context, int64) (*service.Account, error) {
	return s.a, nil
}
func (s *expiryGraceAdminStub) UpdateAccount(_ context.Context, _ int64, in *service.UpdateAccountInput) (*service.Account, error) {
	s.minutes = in.ExpiryGraceMinutes
	s.a.Extra = map[string]any{service.AccountImportExpiryGraceExtraKey: *s.minutes * 60}
	return s.a, nil
}
func TestAccountExpiryGraceEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &expiryGraceAdminStub{a: &service.Account{ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}}
	h := &AccountHandler{adminService: s}
	r := gin.New()
	r.GET("/accounts/:id/expiry-grace", h.GetAccountExpiryGrace)
	r.PUT("/accounts/:id/expiry-grace", h.UpdateAccountExpiryGrace)
	for _, tc := range []struct {
		body   string
		status int
	}{{`{}`, 400}, {`{"minutes":-1}`, 400}, {`{"minutes":1441}`, 400}, {`{"minutes":1.5}`, 400}, {`{"minutes":125}`, 200}, {`{"minutes":0}`, 200}} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/accounts/1/expiry-grace", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		require.Equal(t, tc.status, w.Code, w.Body.String())
	}
	require.NotNil(t, s.minutes)
	require.Zero(t, *s.minutes)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/accounts/1/expiry-grace", nil))
	require.Equal(t, 200, w.Code)
	require.Contains(t, w.Body.String(), `"minutes":0`)
	require.NotContains(t, w.Body.String(), service.AccountImportExpiryGraceExtraKey)
}
