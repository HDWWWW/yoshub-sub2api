//go:build unit

package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func privacyImportRequest(t *testing.T, router *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

func assertPrivacyImportSelection(t *testing.T, setting string, value *bool) {
	t.Helper()
	if setting == "omitted" {
		require.Nil(t, value)
	} else {
		require.NotNil(t, value)
		require.Equal(t, setting == "enabled", *value)
	}
}

func TestAccountPrivacyCodexImportPreservesOptionalFalseOnCreateAndUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, setting := range []string{"omitted", "disabled", "enabled"} {
		t.Run(setting, func(t *testing.T) {
			svc := newCodexImportMemoryAdminService(nil)
			h := &AccountHandler{adminService: svc}
			router := gin.New()
			router.POST("/import", h.ImportCodexSession)
			content, err := json.Marshal(buildCodexRefreshImportValue(t, "workspace", "user", "fixture-refresh"))
			require.NoError(t, err)
			body := map[string]any{"content": string(content), "skip_default_group_bind": true}
			if setting != "omitted" {
				body["privacy_enabled"] = setting == "enabled"
			}
			for range 2 {
				rec := privacyImportRequest(t, router, "/import", body)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			}
			require.Len(t, svc.createdAccounts, 1)
			require.Len(t, svc.updatedAccounts, 1)
			assertPrivacyImportSelection(t, setting, svc.createdAccounts[0].PrivacyEnabled)
			assertPrivacyImportSelection(t, setting, svc.updatedAccounts[0].input.PrivacyEnabled)
		})
	}
}

func TestAccountPrivacyBatchCreatePreservesOptionalFalse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	h := &AccountHandler{adminService: svc}
	router := gin.New()
	router.POST("/batch", h.BatchCreate)
	settings := []string{"omitted", "disabled", "enabled"}
	accounts := []map[string]any{}
	for _, setting := range settings {
		item := map[string]any{"name": setting, "platform": "openai", "type": "oauth", "credentials": map[string]any{"access_token": "fixture"}}
		if setting != "omitted" {
			item["privacy_enabled"] = setting == "enabled"
		}
		accounts = append(accounts, item)
	}
	rec := privacyImportRequest(t, router, "/batch", map[string]any{"accounts": accounts})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, svc.createdAccounts, 3)
	for i, setting := range settings {
		assertPrivacyImportSelection(t, setting, svc.createdAccounts[i].PrivacyEnabled)
	}
}

type privacyImportRoundTripper func(*http.Request) (*http.Response, error)

func (f privacyImportRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAccountPrivacyPATCreatePreservesOptionalFalse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	proxy := service.Proxy{ID: 913, Protocol: "http", Host: "127.0.0.1", Port: 9, Username: "privacy-pat-test"}
	client, err := httpclient.GetClient(httpclient.Options{ProxyURL: proxy.URL(), Timeout: 20 * time.Second, ResponseHeaderTimeout: 15 * time.Second})
	require.NoError(t, err)
	original := client.Transport
	defer func() { client.Transport = original }()
	validations := 0
	client.Transport = privacyImportRoundTripper(func(req *http.Request) (*http.Response, error) {
		validations++
		require.Equal(t, "auth.openai.com", req.URL.Host)
		require.Equal(t, "Bearer at-fixture", req.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"email":"fixture@example.com","chatgpt_user_id":"user","chatgpt_account_id":"workspace","chatgpt_plan_type":"plus","chatgpt_account_is_fedramp":false}`))}, nil
	})
	for _, setting := range []string{"omitted", "disabled", "enabled"} {
		t.Run(setting, func(t *testing.T) {
			svc := newStubAdminService()
			svc.proxies = []service.Proxy{proxy}
			h := &OpenAIOAuthHandler{adminService: svc, openaiOAuthService: service.NewOpenAIOAuthService(nil, nil)}
			router := gin.New()
			router.POST("/pat", h.CreateAccountFromCodexPAT)
			body := map[string]any{"access_token": "at-fixture", "proxy_id": proxy.ID}
			if setting != "omitted" {
				body["privacy_enabled"] = setting == "enabled"
			}
			rec := privacyImportRequest(t, router, "/pat", body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Len(t, svc.createdAccounts, 1)
			assertPrivacyImportSelection(t, setting, svc.createdAccounts[0].PrivacyEnabled)
		})
	}
	require.Equal(t, 3, validations)
}
