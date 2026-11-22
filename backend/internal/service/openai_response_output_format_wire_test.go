//go:build unit

package service_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const outputFormatSchema = `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`

func outputFormatRequest(path, kind string) string {
	format := `{"type":"json_object"}`
	if kind == "json_schema" {
		format = `{"type":"json_schema","strict":true,"schema":` + outputFormatSchema + `}`
		if path == "/v1/chat/completions" {
			format = `{"type":"json_schema","json_schema":{"strict":true,"schema":` + outputFormatSchema + `}}`
		}
	}
	if path == "/v1/chat/completions" {
		return `{"model":"gpt-5.1","stream":true,"service_tier":"priority","messages":[{"role":"user","content":"give a title"}],"response_format":` + format + `}`
	}
	return `{"model":"gpt-5.1","stream":true,"service_tier":"priority","input":[{"role":"user","content":"give a title"}],"text":{"format":` + format + `}}`
}

func assertOutputFormatWire(t *testing.T, body []byte, kind string) {
	t.Helper()
	require.Equal(t, kind, gjson.GetBytes(body, "text.format.type").Str)
	require.Equal(t, "priority", gjson.GetBytes(body, "service_tier").Str, "Responses Fast behavior must be preserved")
	if kind == "json_object" {
		require.Equal(t, "developer", gjson.GetBytes(body, "input.0.role").Str)
		require.Equal(t, "Return a valid JSON object.", gjson.GetBytes(body, "input.0.content.0.text").Str)
		require.EqualValues(t, 2, gjson.GetBytes(body, "input.#").Int(), "normalizing twice must not insert two instructions")
	} else {
		require.Equal(t, "response_schema", gjson.GetBytes(body, "text.format.name").Str)
		require.JSONEq(t, outputFormatSchema, gjson.GetBytes(body, "text.format.schema").Raw)
		require.True(t, gjson.GetBytes(body, "text.format.strict").Bool())
	}
}

func TestOutputFormatHTTPAndSmartWSWire(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, pass := range []bool{false, true} {
			for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
				for _, kind := range []string{"json_object", "json_schema"} {
					t.Run(fmt.Sprintf("ws=%t/pass=%t/%s/%s", ws, pass, path, kind), func(t *testing.T) {
						f := newParityGateway(t, parityScript{ws: ws, passthrough: pass, tail: []string{parityCreated, parityDelta, parityDone}})
						resp := f.post(t, path, outputFormatRequest(path, kind))
						wire := parityRest(t, resp.Body)
						require.Equal(t, http.StatusOK, resp.StatusCode, wire)
						require.Contains(t, wire, "visible-parity")
						assertOutputFormatWire(t, f.checkTransport(t, ws), kind)
					})
				}
			}
		}
	}
}

func TestOutputFormatStrictValidationNeverDispatches(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
			t.Run(fmt.Sprintf("ws=%t/%s", ws, path), func(t *testing.T) {
				f := newParityGateway(t, parityScript{ws: ws, tail: []string{parityDone}})
				body := strings.Replace(outputFormatRequest(path, "json_schema"), `"required":["title"]`, `"required":[]`, 1)
				resp := f.post(t, path, body)
				wire := parityRest(t, resp.Body)
				require.Equal(t, http.StatusBadRequest, resp.StatusCode, wire)
				require.Equal(t, "invalid_request_error", gjson.Get(wire, "error.type").Str)
				param := "text.format.schema.required"
				if path == "/v1/chat/completions" {
					param = "response_format.json_schema.schema.required"
				}
				require.Equal(t, param, gjson.Get(wire, "error.param").Str)
				require.Contains(t, gjson.Get(wire, "error.message").Str, "title")
				require.Zero(t, f.http.Load()+f.ws.Load()+f.upgrades.Load(), "validation must not consume an upstream request or trigger failover")
				require.Zero(t, f.repo.auth.Load()+f.repo.quota.Load(), "invalid client schemas must not penalize accounts")
			})
		}
	}
}

func TestOutputFormatNativeWebSocketWire(t *testing.T) {
	for _, mode := range []string{"ctx_pool", "passthrough", "http_bridge"} {
		for _, kind := range []string{"json_object", "json_schema"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				f := newParityGateway(t, parityScript{mode: mode, tail: []string{parityCreated, parityDone}})
				ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
				defer cancel()
				ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/responses", nil)
				require.NoError(t, err)
				defer ws.Close()
				deadline, _ := ctx.Deadline()
				require.NoError(t, ws.SetReadDeadline(deadline))
				body := strings.Replace(outputFormatRequest("/v1/responses", kind), `"stream":true`, `"type":"response.create"`, 1)
				require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(body)))
				for {
					_, frame, err := ws.ReadMessage()
					require.NoError(t, err)
					typ := gjson.GetBytes(frame, "type").Str
					require.NotEqual(t, "error", typ, string(frame))
					if typ == "response.completed" {
						break
					}
				}
				assertOutputFormatWire(t, f.checkTransport(t, mode != "http_bridge"), kind)
			})
		}
	}
}

func TestOutputFormatNativeWebSocketInvalidSchema(t *testing.T) {
	for _, mode := range []string{"ctx_pool", "passthrough", "http_bridge"} {
		t.Run(mode, func(t *testing.T) {
			f := newParityGateway(t, parityScript{mode: mode, tail: []string{parityDone}})
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
			defer cancel()
			ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			defer ws.Close()
			deadline, _ := ctx.Deadline()
			require.NoError(t, ws.SetReadDeadline(deadline))
			body := strings.Replace(outputFormatRequest("/v1/responses", "json_schema"), `"stream":true`, `"type":"response.create"`, 1)
			body = strings.Replace(body, `"required":["title"]`, `"required":[]`, 1)
			require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(body)))
			_, _, err = ws.ReadMessage()
			var closeErr *websocket.CloseError
			require.ErrorAs(t, err, &closeErr)
			require.Equal(t, websocket.ClosePolicyViolation, closeErr.Code)
			require.Contains(t, closeErr.Text, "text.format.schema.required")
			require.Zero(t, f.http.Load()+f.ws.Load())
			require.Zero(t, f.repo.auth.Load()+f.repo.quota.Load())
		})
	}
}
