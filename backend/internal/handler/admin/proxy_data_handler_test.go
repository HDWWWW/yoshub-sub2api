package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type proxyDataResponse struct {
	Code int         `json:"code"`
	Data DataPayload `json:"data"`
}

type proxyImportResponse struct {
	Code int              `json:"code"`
	Data DataImportResult `json:"data"`
}

func setupProxyDataRouter() (*gin.Engine, *stubAdminService) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	adminSvc := newStubAdminService()

	h := NewProxyHandler(adminSvc)
	router.GET("/api/v1/admin/proxies/data", h.ExportData)
	router.POST("/api/v1/admin/proxies/data", h.ImportData)

	return router, adminSvc
}

func TestProxyExportDataRespectsFilters(t *testing.T) {
	router, adminSvc := setupProxyDataRouter()

	adminSvc.proxies = []service.Proxy{
		{
			ID:       1,
			Name:     "proxy-a",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "user",
			Password: "pass",
			Status:   service.StatusActive,
		},
		{
			ID:       2,
			Name:     "proxy-b",
			Protocol: "https",
			Host:     "10.0.0.2",
			Port:     443,
			Username: "u",
			Password: "p",
			Status:   service.StatusDisabled,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies/data?protocol=https", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp proxyDataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Empty(t, resp.Data.Type)
	require.Equal(t, 0, resp.Data.Version)
	require.Len(t, resp.Data.Proxies, 1)
	require.Len(t, resp.Data.Accounts, 0)
	require.Equal(t, "https", resp.Data.Proxies[0].Protocol)
	require.Equal(t, 1, adminSvc.lastListProxies.calls)
	require.Equal(t, "https", adminSvc.lastListProxies.protocol)
	require.Equal(t, "id", adminSvc.lastListProxies.sortBy)
	require.Equal(t, "desc", adminSvc.lastListProxies.sortOrder)
}

func TestProxyExportDataWithSelectedIDs(t *testing.T) {
	router, adminSvc := setupProxyDataRouter()

	adminSvc.proxies = []service.Proxy{
		{
			ID:       1,
			Name:     "proxy-a",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "user",
			Password: "pass",
			Status:   service.StatusActive,
		},
		{
			ID:       2,
			Name:     "proxy-b",
			Protocol: "https",
			Host:     "10.0.0.2",
			Port:     443,
			Username: "u",
			Password: "p",
			Status:   service.StatusDisabled,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies/data?ids=2", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp proxyDataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Proxies, 1)
	require.Equal(t, "https", resp.Data.Proxies[0].Protocol)
	require.Equal(t, "10.0.0.2", resp.Data.Proxies[0].Host)
	require.Equal(t, 0, adminSvc.lastListProxies.calls)
}

func TestProxyExportDataPassesSortParams(t *testing.T) {
	router, adminSvc := setupProxyDataRouter()

	adminSvc.proxies = []service.Proxy{
		{
			ID:       1,
			Name:     "proxy-a",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "user",
			Password: "pass",
			Status:   service.StatusActive,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies/data?protocol=http&status=active&search=proxy&sort_by=name&sort_order=asc", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Equal(t, 1, adminSvc.lastListProxies.calls)
	require.Equal(t, "http", adminSvc.lastListProxies.protocol)
	require.Equal(t, "active", adminSvc.lastListProxies.status)
	require.Equal(t, "proxy", adminSvc.lastListProxies.search)
	require.Equal(t, "name", adminSvc.lastListProxies.sortBy)
	require.Equal(t, "asc", adminSvc.lastListProxies.sortOrder)
}

func TestProxyExportDataSortByAccountCountUsesAccountCountListing(t *testing.T) {
	router, adminSvc := setupProxyDataRouter()

	adminSvc.proxies = []service.Proxy{
		{
			ID:       1,
			Name:     "proxy-id-1",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Status:   service.StatusActive,
		},
		{
			ID:       2,
			Name:     "proxy-id-2",
			Protocol: "http",
			Host:     "127.0.0.2",
			Port:     8081,
			Status:   service.StatusActive,
		},
	}
	adminSvc.proxyCounts = []service.ProxyWithAccountCount{
		{
			Proxy: service.Proxy{
				ID:       2,
				Name:     "proxy-count-high",
				Protocol: "http",
				Host:     "127.0.0.2",
				Port:     8081,
				Status:   service.StatusActive,
			},
			AccountCount: 9,
		},
		{
			Proxy: service.Proxy{
				ID:       1,
				Name:     "proxy-count-low",
				Protocol: "http",
				Host:     "127.0.0.1",
				Port:     8080,
				Status:   service.StatusActive,
			},
			AccountCount: 1,
		},
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies/data?sort_by=account_count&sort_order=desc", nil)
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp proxyDataResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Len(t, resp.Data.Proxies, 2)
	require.Equal(t, "proxy-count-high", resp.Data.Proxies[0].Name)
	require.Equal(t, "proxy-count-low", resp.Data.Proxies[1].Name)
	require.Equal(t, 0, adminSvc.lastListProxies.calls)
}

func TestProxyImportDataReusesAndTriggersLatencyProbe(t *testing.T) {
	router, adminSvc := setupProxyDataRouter()

	adminSvc.proxies = []service.Proxy{
		{
			ID:       1,
			Name:     "proxy-a",
			Protocol: "http",
			Host:     "127.0.0.1",
			Port:     8080,
			Username: "user",
			Password: "pass",
			Status:   service.StatusActive,
		},
	}

	payload := map[string]any{
		"data": map[string]any{
			"type":    dataType,
			"version": dataVersion,
			"proxies": []map[string]any{
				{
					"proxy_key": "http|127.0.0.1|8080|user|pass",
					"name":      "proxy-a",
					"protocol":  "http",
					"host":      "127.0.0.1",
					"port":      8080,
					"username":  "user",
					"password":  "pass",
					"status":    "inactive",
				},
				{
					"proxy_key": "https|10.0.0.2|443|u|p",
					"name":      "proxy-b",
					"protocol":  "https",
					"host":      "10.0.0.2",
					"port":      443,
					"username":  "u",
					"password":  "p",
					"status":    "active",
				},
			},
			"accounts": []map[string]any{},
		},
	}

	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/proxies/data", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp proxyImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0, resp.Code)
	require.Equal(t, 1, resp.Data.ProxyCreated)
	require.Equal(t, 1, resp.Data.ProxyReused)
	require.Equal(t, 0, resp.Data.ProxyFailed)

	adminSvc.mu.Lock()
	updatedIDs := append([]int64(nil), adminSvc.updatedProxyIDs...)
	adminSvc.mu.Unlock()
	require.Contains(t, updatedIDs, int64(1))

	require.Eventually(t, func() bool {
		adminSvc.mu.Lock()
		defer adminSvc.mu.Unlock()
		return len(adminSvc.testedProxyIDs) == 1
	}, time.Second, 10*time.Millisecond)
}

func TestProxyExportDataExcludesManagedIPv6(t *testing.T) {
	for _, query := range []string{"", "?ids=1,2,3"} {
		t.Run(query, func(t *testing.T) {
			router, adminSvc := setupProxyDataRouter()
			adminSvc.proxies = []service.Proxy{
				{ID: 1, Name: "manual", Protocol: "http", Host: "proxy.example", Port: 8080, Username: "user", Password: "manual-secret", Status: service.StatusActive},
				{ID: 2, Name: "managed", Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "lease-user", Password: "managed-secret", ManagedIPv6: "2001:db8::2", Status: service.StatusActive},
				{ID: 3, Name: "reserved", Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "ovh6_legacy", Password: "reserved-secret", Status: service.StatusActive},
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/proxies/data"+query, nil))
			require.Equal(t, http.StatusOK, rec.Code)
			var resp proxyDataResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Len(t, resp.Data.Proxies, 1)
			require.Equal(t, "manual-secret", resp.Data.Proxies[0].Password)
			for _, forbidden := range []string{"egress.internal", "lease-user", "managed-secret", "ovh6_legacy", "reserved-secret", "2001:db8::2"} {
				require.NotContains(t, rec.Body.String(), forbidden)
			}
		})
	}
}

func TestProxyImportDataRejectsManagedIPv6BeforeReuse(t *testing.T) {
	for _, item := range []DataProxy{
		{Name: "reserved-username", Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "ovh6_lease", Password: "reserved-secret"},
		{Name: "forged-username", Protocol: "http", Host: "manual.example", Port: 8080, Username: "manual", ProxyKey: "socks5|egress.internal|1080|ovh6_lease|reserved-secret"},
		{Name: "marked-proxy", Protocol: "http", Host: "manual.example", Port: 8080, ManagedIPv6: "2001:db8::2"},
		{Name: "existing-managed-key", Protocol: "http", Host: "manual.example", Port: 8080, ProxyKey: "socks5|egress.internal|1080|lease-user|managed-secret"},
		{Name: "export-marker", Protocol: "http", Host: "manual.example", Port: 8080, ProxyKey: managedIPv6DataProxyKey},
		{Name: "managed-key-alias", Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "lease-user", Password: "managed-secret", ProxyKey: "custom-alias"},
	} {
		t.Run(item.Name, func(t *testing.T) {
			router, adminSvc := setupProxyDataRouter()
			adminSvc.proxies = []service.Proxy{
				{ID: 1, Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "ovh6_lease", Password: "reserved-secret"},
				{ID: 2, Protocol: "socks5", Host: "egress.internal", Port: 1080, Username: "lease-user", Password: "managed-secret", ManagedIPv6: "2001:db8::2"},
			}
			body, err := json.Marshal(DataImportRequest{Data: DataPayload{Proxies: []DataProxy{item}, Accounts: []DataAccount{}}})
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/proxies/data", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code)
			var resp proxyImportResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Equal(t, 1, resp.Data.ProxyFailed)
			require.Zero(t, resp.Data.ProxyCreated)
			require.Zero(t, resp.Data.ProxyReused)
			require.Empty(t, adminSvc.createdProxies)
			require.Empty(t, adminSvc.updatedProxyIDs)
			require.Len(t, resp.Data.Errors, 1)
			require.Equal(t, managedIPv6ImportError, resp.Data.Errors[0].Message)
			require.Equal(t, managedIPv6DataProxyKey, resp.Data.Errors[0].ProxyKey)
			require.NotContains(t, rec.Body.String(), "reserved-secret")
			require.NotContains(t, rec.Body.String(), "managed-secret")
		})
	}
}

func TestValidateDataProxyRejectsManagedIPv6(t *testing.T) {
	for _, change := range []func(*DataProxy){
		func(p *DataProxy) { p.Username = "  ovh6_lease  " },
		func(p *DataProxy) { p.ProxyKey = "socks5|egress.internal|1080|ovh6_lease|secret" },
		func(p *DataProxy) { p.ProxyKey = managedIPv6DataProxyKey },
		func(p *DataProxy) { p.ManagedIPv6 = "2001:db8::2" },
	} {
		item := DataProxy{Protocol: "http", Host: "proxy.example", Port: 8080}
		change(&item)
		require.EqualError(t, validateDataProxy(item), managedIPv6ImportError)
	}
	require.NoError(t, validateDataProxy(DataProxy{Protocol: "http", Host: "proxy.example", Port: 8080, Username: "manual"}))
}
