//go:build embed

package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFrontendServer_ConditionalHTMLNonce(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "without_nonce_keeps_revalidation"
		if enabled {
			name = "with_nonce_returns_fresh_html"
		}
		t.Run(name, func(t *testing.T) {
			provider := &mockSettingsProvider{settings: map[string]string{"site_name": "Test"}}
			frontend, err := NewFrontendServer(provider)
			require.NoError(t, err)

			router := gin.New()
			router.Use(middleware.SecurityHeaders(config.CSPConfig{Enabled: enabled}, nil))
			router.Use(frontend.Middleware())
			server := httptest.NewServer(router)
			defer server.Close()

			get := func(etag string) (*http.Response, string) {
				t.Helper()
				req, err := http.NewRequest(http.MethodGet, server.URL+"/admin/accounts", nil)
				require.NoError(t, err)
				if etag != "" {
					req.Header.Set("If-None-Match", etag)
				}
				response, err := server.Client().Do(req)
				require.NoError(t, err)
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				return response, string(body)
			}

			first, firstBody := get("")
			require.Equal(t, http.StatusOK, first.StatusCode)
			require.Contains(t, firstBody, "window.__APP_CONFIG__=")
			cached := frontend.cache.Get()
			require.NotNil(t, cached)
			require.NotEmpty(t, cached.ETag)

			if !enabled {
				assert.Equal(t, cached.ETag, first.Header.Get("ETag"))
				assert.Equal(t, "no-cache", first.Header.Get("Cache-Control"))
				assert.Empty(t, first.Header.Get("Content-Security-Policy"))
				second, secondBody := get(first.Header.Get("ETag"))
				assert.Equal(t, http.StatusNotModified, second.StatusCode)
				assert.Empty(t, secondBody)
				assert.Equal(t, 1, provider.called)
				return
			}

			noncePattern := regexp.MustCompile(`'nonce-([^']+)'`)
			checkNonce := func(response *http.Response, body string) string {
				t.Helper()
				require.Equal(t, http.StatusOK, response.StatusCode)
				assert.Equal(t, "no-store", response.Header.Get("Cache-Control"))
				assert.Empty(t, response.Header.Get("ETag"))
				match := noncePattern.FindStringSubmatch(response.Header.Get("Content-Security-Policy"))
				require.Len(t, match, 2)
				assert.Contains(t, body, `<script nonce="`+match[1]+`">window.__APP_CONFIG__=`)
				assert.NotContains(t, body, NonceHTMLPlaceholder)
				return match[1]
			}
			previousNonce := checkNonce(first, firstBody)
			// A browser may still send the stable ETag cached before this fix.
			for _, etag := range []string{cached.ETag, "*"} {
				response, body := get(etag)
				nonce := checkNonce(response, body)
				assert.NotEqual(t, previousNonce, nonce)
				previousNonce = nonce
			}
			assert.Equal(t, 1, provider.called, "rendered settings remain cached on the server")
		})
	}
}

func TestFrontendServer_StaticAssetConditionalCacheWithCSP(t *testing.T) {
	const assetPath = "assets/index-AbCd1234.js"
	const assetBody = "console.log('cached asset')"
	files := fstest.MapFS{
		assetPath: &fstest.MapFile{Data: []byte(assetBody), ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	frontend := &FrontendServer{distFS: files, fileServer: http.FileServer(http.FS(files))}
	router := gin.New()
	router.Use(middleware.SecurityHeaders(config.CSPConfig{Enabled: true}, nil))
	router.Use(frontend.Middleware())
	server := httptest.NewServer(router)
	defer server.Close()

	first, err := server.Client().Get(server.URL + "/" + assetPath)
	require.NoError(t, err)
	firstBody, err := io.ReadAll(first.Body)
	require.NoError(t, err)
	require.NoError(t, first.Body.Close())
	require.Equal(t, http.StatusOK, first.StatusCode)
	assert.Equal(t, assetBody, string(firstBody))
	assert.Equal(t, staticAssetsCacheControl, first.Header.Get("Cache-Control"))
	require.NotEmpty(t, first.Header.Get("Last-Modified"))

	req, err := http.NewRequest(http.MethodGet, server.URL+"/"+assetPath, nil)
	require.NoError(t, err)
	req.Header.Set("If-Modified-Since", first.Header.Get("Last-Modified"))
	second, err := server.Client().Do(req)
	require.NoError(t, err)
	defer second.Body.Close()
	secondBody, err := io.ReadAll(second.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotModified, second.StatusCode)
	assert.Empty(t, secondBody)
	assert.Equal(t, staticAssetsCacheControl, second.Header.Get("Cache-Control"))
}
