package admin

import (
	"bytes"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImportDataExplicitSelectionReplacesManagedExportWithoutReusingIt(t *testing.T) {
	marker := managedIPv6DataProxyKey
	item := DataAccount{Name: "account", Platform: "openai", Type: "oauth", Credentials: map[string]any{"access_token": "fixture"}, ProxyKey: &marker}
	unchanged, err := resolveDataAccountProxy(DataImportRequest{}, item)
	require.NoError(t, err)
	require.EqualError(t, validateDataAccount(unchanged), managedIPv6ImportError)
	for _, mode := range []string{"ovh_ipv6", "random", "least_used", "direct"} {
		selected, err := resolveDataAccountProxy(DataImportRequest{ProxyMode: mode}, item)
		require.NoError(t, err)
		require.Nil(t, selected.ProxyKey)
		require.Equal(t, mode, selected.ProxyMode)
		require.NoError(t, validateDataAccount(selected))
	}
	id := int64(50)
	selected, err := resolveDataAccountProxy(DataImportRequest{ProxyMode: "specified", ProxyID: &id}, item)
	require.NoError(t, err)
	require.Equal(t, &id, selected.ProxyID)
	require.Nil(t, selected.ProxyKey)
	_, err = resolveDataAccountProxy(DataImportRequest{ProxyMode: "ovh_ipv6", ProxyID: &id}, item)
	require.Error(t, err)
	item.ProxyMode = "ovh_ipv6"
	selected, err = resolveDataAccountProxy(DataImportRequest{}, item)
	require.NoError(t, err)
	require.Nil(t, selected.ProxyKey)
}

func TestImportDataForwardsExplicitIPv6SelectionToEveryAccount(t *testing.T) {
	router, svc := setupAccountDataRouter()
	marker := managedIPv6DataProxyKey
	request := DataImportRequest{ProxyMode: "ovh_ipv6", Data: DataPayload{Proxies: []DataProxy{}, Accounts: []DataAccount{
		{Name: "one", Platform: "openai", Type: "oauth", Credentials: map[string]any{"access_token": "fixture"}, ProxyKey: &marker},
		{Name: "two", Platform: "openai", Type: "oauth", Credentials: map[string]any{"access_token": "fixture"}},
	}}}
	b, err := json.Marshal(request)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/data", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, r)
	require.Equal(t, 200, w.Code)
	require.Len(t, svc.createdAccounts, 2)
	for _, input := range svc.createdAccounts {
		require.Equal(t, "ovh_ipv6", input.ProxyMode)
		require.Nil(t, input.ProxyID)
	}
}
