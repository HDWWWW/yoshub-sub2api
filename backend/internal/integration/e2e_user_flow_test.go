//go:build e2e

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests require a disposable installation with public registration enabled.
// Every flow owns its user, so the tests also work alone or in shuffled order.
func TestUserRegistrationAndLogin(t *testing.T) {
	token, email := registerAndLoginE2EUser(t)
	profile := e2eJSON(t, http.MethodGet, "/api/v1/user/profile", nil, token)
	require.Equal(t, email, profile["email"])
}

func TestAPIKeyLifecycle(t *testing.T) {
	token, _ := registerAndLoginE2EUser(t)
	groupsResponse := e2eRequest(t, http.MethodGet, "/api/v1/groups/available", nil, token)
	defer func() { _ = groupsResponse.Body.Close() }()
	require.Equal(t, http.StatusOK, groupsResponse.StatusCode)
	var groups struct {
		Data []struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(groupsResponse.Body).Decode(&groups))
	require.NotEmpty(t, groups.Data, "disposable installation must contain a nonexclusive standard group")
	created := e2eJSON(t, http.MethodPost, "/api/v1/keys", map[string]any{"name": "e2e-lifecycle", "group_id": groups.Data[0].ID}, token)
	key, ok := created["key"].(string)
	require.True(t, ok, "create response must contain an API key")
	require.NotEmpty(t, key)
	id, ok := created["id"].(float64)
	require.True(t, ok, "create response must contain an ID")
	require.Positive(t, id)
	keyPath := fmt.Sprintf("/api/v1/keys/%.0f", id)
	t.Cleanup(func() { e2eJSON(t, http.MethodDelete, keyPath, nil, token) })

	saved := e2eJSON(t, http.MethodGet, keyPath, nil, token)
	require.Equal(t, "e2e-lifecycle", saved["name"])

	// Models are a local catalog. No paid provider request is sent here.
	response := e2eRequest(t, http.MethodGet, "/v1/models", nil, key)
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		require.NoError(t, err)
		t.Fatalf("new API key gateway status %d: %s", response.StatusCode, body)
	}
	var catalog struct {
		Object string `json:"object"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&catalog))
	require.Equal(t, "list", catalog.Object)

	stats := e2eJSON(t, http.MethodGet, "/api/v1/usage/dashboard/stats", nil, token)
	require.Contains(t, stats, "total_requests")
}

func registerAndLoginE2EUser(t *testing.T) (string, string) {
	t.Helper()
	email := fmt.Sprintf("e2e-%d@example.com", time.Now().UnixNano())
	credentials := map[string]string{"email": email, "password": "E2eTest@12345"}
	e2eJSON(t, http.MethodPost, "/api/v1/auth/register", credentials, "")
	login := e2eJSON(t, http.MethodPost, "/api/v1/auth/login", credentials, "")
	token, ok := login["access_token"].(string)
	require.True(t, ok, "login response must contain access_token")
	require.NotEmpty(t, token)
	return token, email
}

func e2eRequest(t *testing.T, method, path string, payload any, token string) *http.Response {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, baseURL+path, body)
	require.NoError(t, err)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	require.NoError(t, err, "%s %s", method, path)
	return response
}

func e2eJSON(t *testing.T, method, path string, payload any, token string) map[string]any {
	t.Helper()
	response := e2eRequest(t, method, path, payload, token)
	defer func() { _ = response.Body.Close() }()
	require.Equal(t, http.StatusOK, response.StatusCode, "%s %s", method, path)
	var envelope struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&envelope))
	require.Zero(t, envelope.Code, "%s %s", method, path)
	return envelope.Data
}
