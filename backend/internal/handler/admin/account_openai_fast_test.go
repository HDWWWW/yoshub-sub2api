//go:build unit

package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type accountFastAdminStub struct {
	*stubAdminService
	account         *service.Account
	getErr, saveErr error
	updates         map[string]any
}

func (s *accountFastAdminStub) GetAccount(context.Context, int64) (*service.Account, error) {
	return s.account, s.getErr
}

func (s *accountFastAdminStub) UpdateAccountExtra(_ context.Context, _ int64, updates map[string]any) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.updates = updates
	for key, value := range updates {
		s.account.Extra[key] = value
	}
	return nil
}

func TestAccountForceFastToggle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &accountFastAdminStub{stubAdminService: newStubAdminService(), account: &service.Account{
		ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Schedulable: true,
		Extra: map[string]any{"runtime_usage": 42},
	}}
	h := &AccountHandler{adminService: svc}
	router := gin.New()
	router.POST("/accounts/:id/force-fast", h.SetOpenAIForceFast)
	for _, body := range []string{`{"enabled":true}`, `{"enabled":false}`} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/accounts/7/force-fast", strings.NewReader(body)))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		enabled := gjson.Get(body, "enabled").Bool()
		require.Equal(t, enabled, gjson.Get(rec.Body.String(), "data.enabled").Bool())
		require.Equal(t, map[string]any{service.OpenAIForceFastExtraKey: enabled}, svc.updates)
		require.Equal(t, 42, svc.account.Extra["runtime_usage"])
		require.True(t, svc.account.Schedulable)
		// The list's lite projection must preserve the saved flag on reload.
		listItem := dto.AccountListItemFromAccount(dto.AccountFromServiceShallow(svc.account))
		require.Equal(t, enabled, listItem.Extra[service.OpenAIForceFastExtraKey])
	}
}

func TestAccountForceFastRejectsInvalidOrUnsavedChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, id, body, platform string
		getErr, saveErr          error
		status                   int
	}{
		{name: "missing", body: `{}`, status: 400},
		{name: "null", body: `{"enabled":null}`, status: 400},
		{name: "string", body: `{"enabled":"true"}`, status: 400},
		{name: "malformed", body: `{`, status: 400},
		{name: "invalid id", id: "abc", body: `{"enabled":true}`, status: 400},
		{name: "zero id", id: "0", body: `{"enabled":true}`, status: 400},
		{name: "other platform", platform: service.PlatformAnthropic, body: `{"enabled":true}`, status: 400},
		{name: "not found", getErr: service.ErrAccountNotFound, body: `{"enabled":true}`, status: 404},
		{name: "save failed", saveErr: errors.New("database unavailable"), body: `{"enabled":true}`, status: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.id == "" {
				tc.id = "7"
			}
			if tc.platform == "" {
				tc.platform = service.PlatformOpenAI
			}
			svc := &accountFastAdminStub{stubAdminService: newStubAdminService(), getErr: tc.getErr, saveErr: tc.saveErr,
				account: &service.Account{ID: 7, Platform: tc.platform, Extra: map[string]any{}}}
			h := &AccountHandler{adminService: svc}
			router := gin.New()
			router.POST("/accounts/:id/force-fast", h.SetOpenAIForceFast)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/accounts/"+tc.id+"/force-fast", strings.NewReader(tc.body)))
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.Empty(t, svc.updates)
			require.NotContains(t, svc.account.Extra, service.OpenAIForceFastExtraKey)
		})
	}
}
