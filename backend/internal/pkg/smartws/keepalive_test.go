package smartws

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestKeepaliveControlFramesPreserveResponseValidation(t *testing.T) {
	for _, tc := range []struct {
		name, before, after, wantError string
		ping                           bool
	}{
		{name: "before_created", before: `{"type":"keepalive"}`},
		{name: "after_created", after: `{"type":"keepalive"}`},
		{name: "standard_ping", ping: true},
		{name: "existing_codex_control", before: `{"type":"codex.rate_limits"}`},
		{name: "unknown_before_created", before: `{"type":"unknown.control"}`, wantError: "before response.created"},
		{name: "content_before_created", before: `{"type":"response.output_text.delta","delta":"unexpected"}`, wantError: "before response.created"},
		{name: "keepalive_wrong_response", after: `{"type":"keepalive","response_id":"resp_other"}`, wantError: "response ID mismatch"},
		{name: "keepalive_conflicting_ids", after: `{"type":"keepalive","response_id":"resp_other","response":{"id":"resp_keepalive"}}`, wantError: "conflicting upstream response IDs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = ws.Close() }()
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
				if tc.ping {
					if err := ws.WriteControl(websocket.PingMessage, []byte("alive"), time.Now().Add(time.Second)); err != nil {
						return
					}
				}
				for _, frame := range []string{tc.before,
					`{"type":"response.created","response":{"id":"resp_keepalive"}}`, tc.after,
					`{"type":"response.completed","response":{"id":"resp_keepalive","status":"completed","usage":{"output_tokens":1}}}`,
				} {
					if frame != "" {
						if err := ws.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
							return
						}
					}
				}
				_, _, _ = ws.ReadMessage()
			}))
			defer server.Close()
			pool := testPool(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(`{"model":"test","stream":true,"input":"hello"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer local-test")
			resp, handled, err := pool.TryWithOptions(req, "", "keepalive-test", Options{AllowTarget: true, ReadTimeout: time.Second})
			require.NoError(t, err)
			require.True(t, handled)
			require.NotNil(t, resp)
			defer func() { _ = resp.Body.Close() }()
			data, err := io.ReadAll(resp.Body)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				var sent *SentError
				require.ErrorAs(t, err, &sent)
				require.False(t, sent.TransportFailure, "protocol violations must not become recoverable connection drops")
				require.NotContains(t, string(data), "response.completed")
				return
			}
			require.NoError(t, err)
			require.Contains(t, string(data), "response.completed")
			if strings.Contains(tc.before+tc.after, `"type":"keepalive"`) {
				require.Contains(t, string(data), "event: keepalive\ndata: ")
			}
		})
	}
}

func TestReusedSocketBoundsStaleControlDrain(t *testing.T) {
	const prior = `{"type":"response.completed","response":{"id":"resp_prior","status":"completed"}}`
	const late = `{"type":"keepalive","response_id":"resp_prior","usage":{"input_tokens":11}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if _, _, err := ws.ReadMessage(); err != nil || ws.WriteMessage(websocket.TextMessage, []byte(prior)) != nil {
			return
		}
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
		for i := 0; i <= maxIgnoredFrames; i++ {
			if ws.WriteMessage(websocket.TextMessage, []byte(late)) != nil {
				return
			}
		}
	}))
	defer srv.Close()
	p := testPool(t)
	for n := 0; n < 2; n++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"model":"test","input":"hello"}`))
		require.NoError(t, err)
		req.Header.Set("Session-Id", "scope")
		req.Header.Set("Authorization", "Bearer local-test")
		var observed int
		r, handled, err := p.TryWithOptions(req, "", "scope", Options{AllowTarget: true, ReadTimeout: time.Second, OnEvent: func([]byte, bool) { observed++ }})
		require.True(t, handled)
		require.NoError(t, err)
		require.NotNil(t, r)
		data, err := io.ReadAll(r.Body)
		require.NoError(t, r.Body.Close())
		if n == 0 {
			require.NoError(t, err)
			require.Equal(t, 1, observed)
			continue
		}
		require.Equal(t, "true", r.Header.Get("X-Sub2-WS-Reused"))
		var sent *SentError
		require.ErrorAs(t, err, &sent)
		require.Equal(t, "stale_response", sent.Reason)
		require.False(t, sent.TransportFailure)
		require.Empty(t, data)
		require.Zero(t, observed)
		require.EqualValues(t, maxIgnoredFrames, p.Stats()["stale_frame_ignored"])
	}
}
