package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationIPv6RoutesEachKeyAndSavedTests(t *testing.T) {
	for _, engine := range []string{ContentModerationEngineOpenAI, ContentModerationEngineTypeSafe} {
		t.Run(engine, func(t *testing.T) {
			proxies := &contentModerationTestProxyRepo{proxies: map[int64]*Proxy{}}
			cfg := defaultContentModerationConfig()
			cfg.BaseURL, cfg.APIKeys = "http://audit-ipv6.invalid", []string{"key-a", "key-b", "key-legacy"}
			cfg.ProxyID = moderationProxyIDPtr(3)
			cfg.KeyIPv6 = map[string]ContentModerationKeyIPv6{}
			var counts [3]atomic.Int64
			for i, key := range cfg.APIKeys {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "Bearer "+key, r.Header.Get("Authorization"), "a key must never use another key's proxy")
					counts[i].Add(1)
					if engine == ContentModerationEngineTypeSafe {
						answers := map[string]any{}
						for category := range typeSafeModerationRules {
							answers[category] = map[string]any{"type": "noul", "noul": 0}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"model": "jev-test", "answers": answers})
					} else {
						_ = json.NewEncoder(w).Encode(moderationAPIResponse{Results: []moderationAPIResult{{Flagged: false}}})
					}
				}))
				t.Cleanup(server.Close)
				u, err := url.Parse(server.URL)
				require.NoError(t, err)
				port, err := strconv.Atoi(u.Port())
				require.NoError(t, err)
				id := int64(i + 1)
				proxies.proxies[id] = &Proxy{ID: id, Host: u.Hostname(), Port: port, Protocol: "http", Status: StatusActive}
				if i < 2 {
					cfg.KeyIPv6[scopedModerationKeyHash(key, engine)] = ContentModerationKeyIPv6{ProxyID: id, Address: "2001:db8::" + strconv.Itoa(i+1)}
				}
			}
			if engine == ContentModerationEngineTypeSafe {
				cfg.TypeSafe = cfg.engineProfile(ContentModerationEngineOpenAI)
			}
			cfg.Engine = engine
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			settings := &contentModerationTestSettingRepo{values: map[string]string{SettingKeyContentModerationConfig: string(raw)}}
			svc := NewContentModerationService(settings, nil, nil, nil, nil, proxies, nil, nil)
			// Rotation, retries and key tests all share the same per-key route selection.
			for i := 0; i < 6; i++ {
				_, err = svc.callModeration(context.Background(), cfg.effectiveEngine(engine), "hello")
				require.NoError(t, err)
			}
			result, err := svc.TestAPIKeys(context.Background(), TestContentModerationAPIKeysInput{Engine: engine})
			require.NoError(t, err)
			require.Len(t, result.Items, 3)
			for i := range counts {
				require.EqualValues(t, 3, counts[i].Load())
			}
			require.EqualValues(t, 3, proxies.getCalls.Load(), "alternating keys should retain separate proxy cache entries")
			require.Equal(t, "2001:db8::1", result.Items[0].ManagedIPv6)
			require.Equal(t, "2001:db8::2", result.Items[1].ManagedIPv6)
			require.Empty(t, result.Items[2].ManagedIPv6)
			view, err := svc.GetConfig(context.Background())
			require.NoError(t, err)
			require.Equal(t, "2001:db8::1", view.APIKeyStatuses[0].ManagedIPv6)
			status, err := svc.GetStatus(context.Background())
			require.NoError(t, err)
			require.Equal(t, "2001:db8::2", status.APIKeyStatuses[1].ManagedIPv6)
			encoded, err := json.Marshal(view)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "key-a")
		})
	}
}

func TestContentModerationIPv6FailureNeverUsesDefaultRoute(t *testing.T) {
	for _, failure := range []string{"missing", "inactive", "invalid"} {
		t.Run(failure, func(t *testing.T) {
			var direct atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
			defer server.Close()
			cfg := defaultContentModerationConfig()
			cfg.BaseURL = server.URL
			cfg.KeyIPv6 = map[string]ContentModerationKeyIPv6{scopedModerationKeyHash("key-a"): {ProxyID: 1, Address: "2001:db8::1"}}
			proxies := &contentModerationTestProxyRepo{proxies: map[int64]*Proxy{1: {ID: 1, Protocol: "http", Host: "127.0.0.1", Port: 1, Status: "inactive"}}}
			if failure == "missing" {
				proxies.getByIDErr = errors.New("removed")
			}
			if failure == "invalid" {
				cfg.KeyIPv6[scopedModerationKeyHash("key-a")] = ContentModerationKeyIPv6{}
			}
			svc := NewContentModerationService(nil, nil, nil, nil, nil, proxies, nil, nil)
			_, err := svc.callModerationOnceWithInput(context.Background(), cfg, "key-a", "hello", nil)
			require.Error(t, err)
			require.Zero(t, direct.Load())
		})
	}
}

func TestContentModerationIPv6UnsavedKeyRequiresSave(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.AutoAssignIPv6 = true
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	_, err := svc.moderationHTTPClient(context.Background(), cfg, "new-key")
	require.ErrorContains(t, err, "先保存")
	cfg.APIKeys = []string{"legacy-key"}
	_, err = svc.moderationHTTPClient(context.Background(), cfg, "legacy-key")
	require.NoError(t, err, "enabling auto allocation does not silently allocate existing keys")
}
