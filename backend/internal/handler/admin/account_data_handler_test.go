package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type dataResponse struct {
	Code int         `json:"code"`
	Data dataPayload `json:"data"`
}

type dataPayload struct {
	Type           string        `json:"type"`
	Version        int           `json:"version"`
	Proxies        []dataProxy   `json:"proxies"`
	Accounts       []dataAccount `json:"accounts"`
	SkippedShadows int           `json:"skipped_shadows"`
}

type dataProxy struct {
	ProxyKey string `json:"proxy_key"`
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Status   string `json:"status"`
}

type dataAccount struct {
	Name        string         `json:"name"`
	Platform    string         `json:"platform"`
	Type        string         `json:"type"`
	Credentials map[string]any `json:"credentials"`
	Extra       map[string]any `json:"extra"`
	ProxyKey    *string        `json:"proxy_key"`
	Concurrency int            `json:"concurrency"`
	Priority    int            `json:"priority"`
}

func setupAccountDataRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminSvc := newStubAdminService()

	h := NewAccountHandler(
		adminSvc,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	router.GET("/api/v1/admin/accounts/data", h.ExportData)
	router.POST("/api/v1/admin/accounts/data", h.ImportData)
	return router, adminSvc
}

func TestExportDataIncludesSecrets(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	proxyID := int64(11)
	adminSvc.proxies = []service.Proxy{
		{
			ID:       proxyID,
			Name:     "proxy",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "user",
			Password: "pass",
			Status:   service.StatusActive,
		},
		{
			ID:       12,
			Name:     "orphan",
			Protocol: "https",
			Host:     "10.0.0.1",
			Port:     443,
			Username: "o",
			Password: "p",
			Status:   service.StatusActive,
		},
	}
	adminSvc.accounts = []service.Account{
		{
			ID:          21,
			Name:        "account",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Credentials: map[string]any{"token": "secret"},
			Extra:       map[string]any{"note": "x"},
			ProxyID:     &proxyID,
			Concurrency: 3,
			Priority:    50,
			Status:      service.StatusDisabled,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp dataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Empty(t, resp.Data.Type)
	require.Equal(t, 0, resp.Data.Version)
	require.Len(t, resp.Data.Proxies, 1)
	require.Equal(t, "pass", resp.Data.Proxies[0].Password)
	require.Len(t, resp.Data.Accounts, 1)
	require.Equal(t, "secret", resp.Data.Accounts[0].Credentials["token"])
}

func TestExportDataWithoutProxies(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	proxyID := int64(11)
	adminSvc.proxies = []service.Proxy{
		{
			ID:       proxyID,
			Name:     "proxy",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "user",
			Password: "pass",
			Status:   service.StatusActive,
		},
	}
	adminSvc.accounts = []service.Account{
		{
			ID:          21,
			Name:        "account",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Credentials: map[string]any{"token": "secret"},
			ProxyID:     &proxyID,
			Concurrency: 3,
			Priority:    50,
			Status:      service.StatusDisabled,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data?include_proxies=false", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp dataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Proxies, 0)
	require.Len(t, resp.Data.Accounts, 1)
	require.Nil(t, resp.Data.Accounts[0].ProxyKey)
}

// TestExportDataExcludesSparkShadow 验证外审第5轮 P1/P2:导出时排除 spark 影子账号
// (影子无凭据、导入侧强制 credentials 非空,混入会产出无法还原的坏备份),并透出跳过计数。
func TestExportDataExcludesSparkShadow(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	parentID := int64(21)
	adminSvc.accounts = []service.Account{
		{
			ID:          parentID,
			Name:        "mother",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Credentials: map[string]any{"token": "secret"},
			Status:      service.StatusActive,
		},
		{
			ID:              22,
			Name:            "mother (Spark)",
			Platform:        service.PlatformOpenAI,
			Type:            service.AccountTypeOAuth,
			Credentials:     map[string]any{}, // 影子恒空凭据
			ParentAccountID: &parentID,        // 影子标记
			QuotaDimension:  service.QuotaDimensionSpark,
			Status:          service.StatusActive,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data?include_proxies=false", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp dataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Accounts, 1, "影子应被排除,仅导出母账号")
	require.Equal(t, "mother", resp.Data.Accounts[0].Name)
	require.Equal(t, 1, resp.Data.SkippedShadows, "跳过的影子数量应透出")
}

func TestExportDataPassesAccountFiltersAndSort(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()
	adminSvc.accounts = []service.Account{
		{ID: 1, Name: "acc-1", Status: service.StatusActive},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/admin/accounts/data?platform=openai&type=oauth&status=active&group=12&privacy_mode=blocked&search=keyword&sort_by=priority&sort_order=desc",
		nil,
	)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Equal(t, 1, adminSvc.lastListAccounts.calls)
	require.Equal(t, "openai", adminSvc.lastListAccounts.platform)
	require.Equal(t, "oauth", adminSvc.lastListAccounts.accountType)
	require.Equal(t, "active", adminSvc.lastListAccounts.status)
	require.Equal(t, int64(12), adminSvc.lastListAccounts.groupID)
	require.Equal(t, "blocked", adminSvc.lastListAccounts.privacyMode)
	require.Equal(t, "keyword", adminSvc.lastListAccounts.search)
	require.Equal(t, "priority", adminSvc.lastListAccounts.sortBy)
	require.Equal(t, "desc", adminSvc.lastListAccounts.sortOrder)
}

func TestExportDataSelectedIDsOverrideFilters(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/admin/accounts/data?ids=1,2&platform=openai&search=keyword&sort_by=priority&sort_order=desc",
		nil,
	)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp dataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Accounts, 2)
	require.Equal(t, 0, adminSvc.lastListAccounts.calls)
}

func TestImportDataReusesProxyAndSkipsDefaultGroup(t *testing.T) {
	router, adminSvc := setupAccountDataRouter()

	adminSvc.proxies = []service.Proxy{
		{
			ID:       1,
			Name:     "proxy",
			Protocol: "socks5",
			Host:     "1.2.3.4",
			Port:     1080,
			Username: "u",
			Password: "p",
			Status:   service.StatusActive,
		},
	}

	dataPayload := map[string]any{
		"data": map[string]any{
			"type":    dataType,
			"version": dataVersion,
			"proxies": []map[string]any{
				{
					"proxy_key": "socks5|1.2.3.4|1080|u|p",
					"name":      "proxy",
					"protocol":  "socks5",
					"host":      "1.2.3.4",
					"port":      1080,
					"username":  "u",
					"password":  "p",
					"status":    "active",
				},
			},
			"accounts": []map[string]any{
				{
					"name":        "acc",
					"platform":    service.PlatformOpenAI,
					"type":        service.AccountTypeOAuth,
					"credentials": map[string]any{"token": "x"},
					"proxy_key":   "socks5|1.2.3.4|1080|u|p",
					"concurrency": 3,
					"priority":    50,
				},
			},
		},
		"skip_default_group_bind": true,
	}

	body, _ := json.Marshal(dataPayload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Len(t, adminSvc.createdProxies, 0)
	require.Len(t, adminSvc.createdAccounts, 1)
	require.True(t, adminSvc.createdAccounts[0].SkipDefaultGroupBind)
}

func TestExportDataManagedIPv6RequiresNewAllocation(t *testing.T) {
	for _, query := range []string{"", "?include_proxies=false"} {
		t.Run(query, func(t *testing.T) {
			router, adminSvc := setupAccountDataRouter()
			manualID, managedID, reservedID := int64(1), int64(2), int64(3)
			adminSvc.proxies = []service.Proxy{
				{ID: manualID, Name: "manual", Protocol: "http", Host: "proxy.example", Port: 8080, Username: "manual-user", Password: "manual-secret", Status: service.StatusActive},
				{ID: managedID, Name: "managed", Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "lease-user", Password: "managed-secret", ManagedIPv6: "2001:db8::2", Status: service.StatusActive},
				{ID: reservedID, Name: "reserved", Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "ovh6_lease", Password: "reserved-secret", Status: service.StatusActive},
			}
			adminSvc.accounts = nil
			for _, proxyID := range []*int64{&manualID, &managedID, &reservedID} {
				adminSvc.accounts = append(adminSvc.accounts, service.Account{
					ID: *proxyID, Name: "account", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
					Credentials: map[string]any{"token": "account-secret"}, ProxyID: proxyID, Status: service.StatusActive,
				})
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data"+query, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			var resp proxyDataResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Len(t, resp.Data.Accounts, 3)
			if query == "" {
				require.Len(t, resp.Data.Proxies, 1)
				require.Equal(t, "manual-secret", resp.Data.Proxies[0].Password)
				require.NotNil(t, resp.Data.Accounts[0].ProxyKey)
			} else {
				require.Empty(t, resp.Data.Proxies)
				require.Nil(t, resp.Data.Accounts[0].ProxyKey)
			}
			for _, account := range resp.Data.Accounts {
				require.Equal(t, "account-secret", account.Credentials["token"])
			}
			for _, account := range resp.Data.Accounts[1:] {
				if query == "" {
					require.NotNil(t, account.ProxyKey)
					require.Equal(t, managedIPv6DataProxyKey, *account.ProxyKey)
				} else {
					require.Nil(t, account.ProxyKey)
					require.NotContains(t, rec.Body.String(), "proxy_key")
				}
			}
			for _, forbidden := range []string{"lease-user", "managed-secret", "ovh6_lease", "reserved-secret", "egress.internal", "2001:db8::2"} {
				require.NotContains(t, rec.Body.String(), forbidden)
			}

			// Full backups retain lease protection; account-only exports import
			// without any provider-specific proxy binding.
			body, err := json.Marshal(DataImportRequest{Data: resp.Data})
			require.NoError(t, err)
			importRec := httptest.NewRecorder()
			importReq := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
			importReq.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(importRec, importReq)
			require.Equal(t, http.StatusOK, importRec.Code)
			var imported proxyImportResponse
			require.NoError(t, json.Unmarshal(importRec.Body.Bytes(), &imported))
			if query == "" {
				require.Equal(t, 1, imported.Data.AccountCreated)
				require.Equal(t, 2, imported.Data.AccountFailed)
				require.Len(t, adminSvc.createdAccounts, 1)
			} else {
				require.Equal(t, 3, imported.Data.AccountCreated)
				require.Zero(t, imported.Data.AccountFailed)
				require.Len(t, adminSvc.createdAccounts, 3)
			}
			for _, problem := range imported.Data.Errors {
				require.Equal(t, managedIPv6ImportError, problem.Message)
			}
		})
	}
}

func TestImportDataRejectsManagedIPv6References(t *testing.T) {
	for _, test := range []struct {
		name    string
		key     string
		proxies []DataProxy
	}{
		{name: "export marker", key: managedIPv6DataProxyKey},
		{name: "existing lease without proxy import", key: "socks5|egress.internal|1080|ovh6_lease|reserved-secret"},
		{name: "existing managed metadata", key: "socks5|egress.internal|1080|lease-user|managed-secret"},
		{name: "forged proxy fields with lease key", key: "socks5|egress.internal|1080|ovh6_lease|reserved-secret", proxies: []DataProxy{
			{Name: "forged", Protocol: "http", Host: "manual.example", Port: 8080, Username: "manual", ProxyKey: "socks5|egress.internal|1080|ovh6_lease|reserved-secret"},
		}},
		{name: "retired lease absent from database", key: "socks5|egress.internal|1080|ovh6_retired|retired-secret"},
		{name: "alias for managed proxy", key: "custom-proxy-alias", proxies: []DataProxy{
			{Name: "alias", Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "lease-user", Password: "managed-secret", ProxyKey: "custom-proxy-alias"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			router, adminSvc := setupAccountDataRouter()
			adminSvc.proxies = []service.Proxy{
				{ID: 1, Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "ovh6_lease", Password: "reserved-secret"},
				{ID: 2, Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "lease-user", Password: "managed-secret", ManagedIPv6: "2001:db8::2"},
			}
			proxies := append([]DataProxy{}, test.proxies...)
			body, err := json.Marshal(DataImportRequest{Data: DataPayload{
				Proxies: proxies,
				Accounts: []DataAccount{{Name: "account", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
					Credentials: map[string]any{"token": "secret"}, ProxyKey: &test.key}},
			}})
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
			var resp proxyImportResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Zero(t, resp.Data.AccountCreated)
			require.Equal(t, 1, resp.Data.AccountFailed)
			require.Zero(t, resp.Data.ProxyCreated)
			require.Zero(t, resp.Data.ProxyReused)
			require.Equal(t, len(test.proxies), resp.Data.ProxyFailed)
			require.Empty(t, adminSvc.createdAccounts)
			require.Empty(t, adminSvc.createdProxies)
			require.Empty(t, adminSvc.updatedProxyIDs)
			for _, problem := range resp.Data.Errors {
				require.Equal(t, managedIPv6ImportError, problem.Message)
			}
			require.NotContains(t, rec.Body.String(), "reserved-secret")
			require.NotContains(t, rec.Body.String(), "managed-secret")
			require.NotContains(t, rec.Body.String(), "retired-secret")
		})
	}
}
