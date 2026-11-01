package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type astraPriorityHandlerRepo struct {
	service.SettingRepository
	value string
	fail  bool
	calls int
}

func (r *astraPriorityHandlerRepo) GetValue(context.Context, string) (string, error) {
	if r.value == "" {
		return "", service.ErrSettingNotFound
	}
	return r.value, nil
}

func (r *astraPriorityHandlerRepo) Set(_ context.Context, key, value string) error {
	r.calls++
	if r.fail {
		return errors.New("write unavailable")
	}
	r.value = value
	return nil
}

func TestAstraPriorityAdminSwitch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, body string
		scoped     bool
		fail       bool
		status     int
		calls      int
		want       bool
	}{
		{"enable", `{"enabled":true}`, false, false, 200, 1, true},
		{"disable", `{"enabled":false}`, false, false, 200, 1, false},
		{"missing", `{}`, false, false, 400, 0, false},
		{"null", `{"enabled":null}`, false, false, 400, 0, false},
		{"wrong_type", `{"enabled":"false"}`, false, false, 400, 0, false},
		{"scoped", `{"enabled":false}`, true, false, 403, 0, false},
		{"storage_failure", `{"enabled":true}`, false, true, 500, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &astraPriorityHandlerRepo{fail: tc.fail}
			h := &SettingHandler{settingService: service.NewSettingService(repo, nil)}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPut, "/accounts/astra-priority", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			if tc.scoped {
				c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), &adminaccess.Principal{KeyID: "scoped"}))
			}
			h.SetAstraPriority(c)
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, tc.calls, repo.calls)
			if tc.status == http.StatusOK {
				var out struct {
					Data service.AstraPrioritySettings `json:"data"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
				require.Equal(t, tc.want, out.Data.Enabled)
			}
		})
	}
}
