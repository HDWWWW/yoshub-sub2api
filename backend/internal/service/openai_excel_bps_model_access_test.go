package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestExcelBPSModelsDiscoveryAndSelectedCache(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"allowed":true,"model_catalog":{"models":[{"id":"gpt-6-astra","label":"Astra"},{"id":"gpt-5.6-sol"},{"id":"gpt-denied"}],"restricted_models":["gpt-denied"]}}`))}}
	s := openAIClientToolsTestService(upstream)
	a := excelAccount()
	list, err := s.FetchOpenAIModelsList(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, "bps.openai.com", upstream.lastReq.URL.Host)
	require.Equal(t, "/basispoints/api/responses/access", upstream.lastReq.URL.Path)
	require.Equal(t, "true", upstream.lastReq.URL.Query().Get("include_models"))
	require.Empty(t, upstream.lastReq.Header.Get("originator"))
	require.Equal(t, int64(2), gjson.GetBytes(list.Body, "data.#").Int())
	a.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
	list, err = s.FetchOpenAIModelsList(context.Background(), a)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, int64(1), gjson.GetBytes(list.Body, "data.#").Int())
	manifest, err := s.FetchCodexModelsManifest(context.Background(), a, "", "")
	require.NoError(t, err)
	require.Equal(t, "gpt-6-astra", gjson.GetBytes(manifest.Body, "models.0.slug").String())
	unchanged, err := s.FetchCodexModelsManifest(context.Background(), a, "", manifest.ETag)
	require.NoError(t, err)
	require.True(t, unchanged.NotModified)
	a.Extra["openai_excel_bps"] = false
	_, err = s.FetchOpenAIModelsList(context.Background(), a)
	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
}

func TestExcelBPSDeniedCatalogStaysEmpty(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"allowed":false}`))}}
	s := openAIClientToolsTestService(upstream)
	manifest, err := s.FetchCodexModelsManifest(context.Background(), excelAccount(), "", "")
	require.NoError(t, err)
	require.JSONEq(t, `{"models":[]}`, string(manifest.Body))
}
