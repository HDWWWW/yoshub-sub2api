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

type healthDurationAdminStub struct {
	service.AdminService
	input *service.UpdateAccountInput
}

func (s *healthDurationAdminStub) UpdateAccount(_ context.Context, id int64, input *service.UpdateAccountInput) (*service.Account, error) {
	s.input = input
	return &service.Account{ID: id, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive}, nil
}

func TestAccountHealthDurationRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &healthDurationAdminStub{}
	h := &AccountHandler{adminService: s}
	r := gin.New()
	r.PUT("/accounts/:id", h.Update)
	for _, value := range []string{"0", "-1", "1.5", "525601", `"100"`, "100"} {
		s.input = nil
		w := httptest.NewRecorder()
		req := httptest.NewRequest("PUT", "/accounts/1", strings.NewReader(`{"health_duration_minutes":`+value+`}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if value == "100" {
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Equal(t, 100, *s.input.HealthDurationMinutes)
			require.Nil(t, s.input.ExpiresAt)
		} else {
			require.Equal(t, 400, w.Code, w.Body.String())
			require.Nil(t, s.input)
		}
	}
}
