package smartws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// Model the upstream connection-scoped search capability using local sockets.
func TestSearchActivationOnIndependentPool(t *testing.T) {
	var dials atomic.Int32
	var responses atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		dials.Add(1)
		initialized, initialSearch := false, false
		for {
			_, payload, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var request struct {
				Tools []struct {
					Type string `json:"type"`
				} `json:"tools"`
			}
			if json.Unmarshal(payload, &request) != nil {
				return
			}
			search := false
			for _, tool := range request.Tools {
				search = search || tool.Type == "web_search"
			}
			if !initialized {
				initialized, initialSearch = true, search
			}
			response := fmt.Sprintf(`{"type":"response.completed","response":{"id":"audit_response_%d","status":"completed","output":[],"usage":{"output_tokens":1}}}`, responses.Add(1))
			if search && !initialSearch {
				response = `{"type":"error","error":{"code":"invalid_request_error","message":"web_search was not enabled on this connection's first response.create"}}`
			}
			if ws.WriteMessage(websocket.TextMessage, []byte(response)) != nil {
				return
			}
		}
	}))
	defer server.Close()
	pool := NewPool()
	pool.config.perKey = 1 // A capability upgrade must not increase identity limits.
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := func(body string, search bool) string {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer local-audit-only")
		req.Header.Set("Session-Id", "stable-review-session")
		resp, handled, err := pool.TryWithOptions(req, "", "audit-fixed-scope", Options{AllowTarget: true, RequireWebSearch: search})
		require.NoError(t, err)
		require.True(t, handled)
		require.NotNil(t, resp)
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.NoError(t, err)
		return string(data)
	}
	first := request(`{"model":"audit","stream":true,"input":[{"role":"user","content":"hello"}]}`, false)
	require.Contains(t, first, "response.completed")
	second := request(`{"model":"audit","stream":true,"tools":[{"type":"web_search"}],"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"search now"}]}`, true)
	require.Contains(t, second, "response.completed")
	require.EqualValues(t, 2, dials.Load(), "search must initialize a compatible socket")
	third := request(`{"model":"audit","input":"no search this time"}`, false)
	require.Contains(t, third, "response.completed")
	fourth := request(`{"model":"audit","tools":[{"type":"web_search"}],"input":"search again"}`, true)
	require.Contains(t, fourth, "response.completed")
	require.EqualValues(t, 2, dials.Load(), "a search-capable socket supports both request types")
	require.EqualValues(t, 1, pool.Stats()["connections"], "capacity must remain bounded")
}
