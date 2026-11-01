//go:build unit

package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type privacyPreferenceAdminStub struct {
	*stubAdminService
	account       *service.Account
	saveErr       error
	privacyCalls  int
	privacyFailed bool
}

func (s *privacyPreferenceAdminStub) GetAccount(context.Context, int64) (*service.Account, error) {
	return s.account, nil
}
func (s *privacyPreferenceAdminStub) UpdateAccountExtra(_ context.Context, _ int64, extra map[string]any) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.account.Extra == nil {
		s.account.Extra = map[string]any{}
	}
	for key, value := range extra {
		s.account.Extra[key] = value
	}
	return nil
}
func (s *privacyPreferenceAdminStub) ForceOpenAIPrivacy(_ context.Context, account *service.Account) string {
	s.privacyCalls++
	mode := service.PrivacyModeTrainingOff
	if !service.OpenAIPrivacyEnabled(account.Extra) {
		mode = service.PrivacyModeTrainingOn
	}
	if s.privacyFailed {
		mode = service.PrivacyModeFailed
	}
	account.Extra["privacy_mode"] = mode
	return mode
}

func TestAccountPrivacySetSupportsBothDirectionsAndLegacyEmptyBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		body    string
		enabled bool
	}{
		{"", true}, {`{}`, true}, {`{"enabled":true}`, true}, {`{"enabled":false}`, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			svc := &privacyPreferenceAdminStub{stubAdminService: newStubAdminService(), account: &service.Account{
				ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
				Extra: map[string]any{service.OpenAIPrivacyEnabledExtraKey: false},
			}}
			h := &AccountHandler{adminService: svc}
			router := gin.New()
			router.POST("/accounts/:id/set-privacy", h.SetPrivacy)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/accounts/1/set-privacy", strings.NewReader(tc.body)))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, tc.enabled, svc.account.Extra[service.OpenAIPrivacyEnabledExtraKey])
			require.Equal(t, 1, svc.privacyCalls)
			mode := service.PrivacyModeTrainingOn
			if tc.enabled {
				mode = service.PrivacyModeTrainingOff
			}
			require.Contains(t, rec.Body.String(), `"privacy_mode":"`+mode+`"`)
		})
	}
}

func TestAccountPrivacySetDoesNotConflateFailureWithDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &privacyPreferenceAdminStub{stubAdminService: newStubAdminService(), privacyFailed: true, account: &service.Account{
		ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
	}}
	h := &AccountHandler{adminService: svc}
	router := gin.New()
	router.POST("/accounts/:id/set-privacy", h.SetPrivacy)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/accounts/1/set-privacy", strings.NewReader(`{"enabled":false}`)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, false, svc.account.Extra[service.OpenAIPrivacyEnabledExtraKey])
	require.Contains(t, rec.Body.String(), `"privacy_mode":"training_set_failed"`)
	require.NotContains(t, rec.Body.String(), `"privacy_mode":"training_on"`)
}

func TestAccountPrivacySetRejectsInvalidOrUnsavedIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, body string
		saveErr    error
		status     int
	}{
		{"invalid", `{"enabled":"false"}`, nil, http.StatusBadRequest},
		{"save failed", `{"enabled":false}`, errors.New("database unavailable"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &privacyPreferenceAdminStub{stubAdminService: newStubAdminService(), saveErr: tc.saveErr, account: &service.Account{
				ID: 1, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
			}}
			h := &AccountHandler{adminService: svc}
			router := gin.New()
			router.POST("/accounts/:id/set-privacy", h.SetPrivacy)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/accounts/1/set-privacy", strings.NewReader(tc.body)))
			require.Equal(t, tc.status, rec.Code)
			require.Zero(t, svc.privacyCalls)
			require.NotContains(t, svc.account.Extra, service.OpenAIPrivacyEnabledExtraKey)
		})
	}
}

func TestAccountPrivacyCreateUpdatePreserveOptionalFalse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, setting := range []string{"", `,"privacy_enabled":false`, `,"privacy_enabled":true`} {
		t.Run(setting, func(t *testing.T) {
			svc := newStubAdminService()
			router := setupAccountMixedChannelRouter(svc)
			for _, operation := range []struct{ method, path, body string }{
				{http.MethodPost, "/api/v1/admin/accounts", `{"name":"fixture","platform":"openai","type":"oauth","credentials":{"access_token":"fixture"}` + setting + `}`},
				{http.MethodPut, "/api/v1/admin/accounts/1", `{"name":"fixture"` + setting + `}`},
			} {
				rec := httptest.NewRecorder()
				request := httptest.NewRequest(operation.method, operation.path, strings.NewReader(operation.body))
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(rec, request)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
			require.Len(t, svc.createdAccounts, 1)
			create, update := svc.createdAccounts[0].PrivacyEnabled, svc.lastUpdateAccountInput.PrivacyEnabled
			if setting == "" {
				require.Nil(t, create)
				require.Nil(t, update)
			} else {
				require.NotNil(t, create)
				require.NotNil(t, update)
				require.Equal(t, strings.Contains(setting, "true"), *create)
				require.Equal(t, *create, *update)
			}
		})
	}
}
