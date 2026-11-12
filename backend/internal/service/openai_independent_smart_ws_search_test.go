//go:build unit

package service

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestIndependentSmartWSSearchCapability(t *testing.T) {
	for _, tc := range []struct {
		name, first, next, accountType string
		dials                          int32
	}{
		{"late_tools", ``, `,"tools":[{"type":"web_search"}]`, AccountTypeOAuth, 2},
		{"late_additional_tools", ``, `,"input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]`, AccountTypeOAuth, 2},
		{"already_enabled", `,"tools":[{"type":"web_search"}]`, `,"tools":[{"type":"web_search"}]`, AccountTypeOAuth, 1},
		{"function_named_search", ``, `,"tools":[{"type":"function","name":"web_search"}]`, AccountTypeOAuth, 1},
		{"apikey_provider", ``, `,"tools":[{"type":"web_search"}]`, AccountTypeAPIKey, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dials, responses atomic.Int32
			svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				dials.Add(1)
				initialized, searchEnabled := false, false
				for {
					_, body, err := conn.ReadMessage()
					if err != nil {
						return
					}
					search := strings.Contains(string(body), `"type":"web_search"`)
					if !initialized {
						initialized, searchEnabled = true, search
					}
					if search && !searchEnabled && tc.accountType == AccountTypeOAuth {
						_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"invalid_request_error","message":"search not enabled on first create"}}`))
						return
					}
					payload := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`, responses.Add(1))
					if conn.WriteMessage(websocket.TextMessage, []byte(payload)) != nil {
						return
					}
				}
			})
			account.Type = tc.accountType
			for _, fields := range []string{tc.first, tc.next} {
				body := `{"model":"gpt-5.1","stream":true` + fields + `}`
				c, _ := forcedWSContext(t, "/v1/responses", 1, "search-session")
				req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "http://example.test/v1/responses", strings.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer search-fixture")
				response, err := svc.doOpenAIHTTPOverWS(c, req, "", account)
				require.NoError(t, err)
				data, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				require.NoError(t, err)
				require.Contains(t, string(data), "response.completed")
			}
			require.Equal(t, tc.dials, dials.Load())
			require.Zero(t, transport.callCount)
		})
	}
}
