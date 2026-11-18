//go:build unit

package service

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// v0.2.22's output accumulator preserves sealed tool calls. Exercise the
// adapter as well: the terminal has no output, so it cannot mask lost deltas.
func TestIndependentSmartWSSealedToolOutput(t *testing.T) {
	const added = `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_sealed","name":"lookup","namespace":"tools","encrypted_function_args":["key"],"encrypted_content":"sealed+/="}}`
	const delta = `{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"key\":\"value\"}"}`
	const completed = `{"type":"response.completed","response":{"id":"resp_force","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":3}}}`
	for _, tc := range []struct {
		name                string
		passthrough, stream bool
	}{{"buffered", false, false}, {"buffered_passthrough", true, false}, {"streaming", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var dispatched atomic.Int32
			s, a, h := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				ws, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer ws.Close()
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
				dispatched.Add(1)
				for _, frame := range []string{forcedWSCreated, added, delta, completed} {
					if ws.WriteMessage(websocket.TextMessage, []byte(frame)) != nil {
						return
					}
				}
			})
			a.Extra["openai_passthrough"] = tc.passthrough
			c, rec := forcedWSContext(t, "/v1/responses", 1, "sealed")
			result, err := s.Forward(c.Request.Context(), c, a, []byte(fmt.Sprintf(`{"model":"gpt-5.1","input":"lookup","stream":%t}`, tc.stream)))
			require.NoError(t, err)
			require.EqualValues(t, 1, dispatched.Load())
			require.Zero(t, h.callCount)
			require.Equal(t, http.StatusOK, rec.Code)
			require.NotNil(t, result)
			require.Equal(t, 7, result.Usage.InputTokens)
			response := gjson.ParseBytes(rec.Body.Bytes())
			if tc.stream {
				_, terminal, ok := extractOpenAISSETerminalEvent(rec.Body.String())
				require.True(t, ok)
				response = gjson.GetBytes(terminal, "response")
			}
			require.Equal(t, int64(1), response.Get("output.#").Int())
			call := response.Get("output.0")
			require.Equal(t, "call_sealed", call.Get("call_id").Str)
			require.Equal(t, "tools", call.Get("namespace").Str)
			require.Equal(t, `{"key":"value"}`, call.Get("arguments").Str)
			require.Equal(t, `["key"]`, call.Get("encrypted_function_args").Raw)
			require.Equal(t, "sealed+/=", call.Get("encrypted_content").Str)
		})
	}
}
