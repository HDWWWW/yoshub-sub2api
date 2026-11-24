//go:build unit

package service_test

import (
	"bufio"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The upstream cannot send its first failure before the HTTP client has read
// the complete, locally generated OAuth notification. Chat never gets one.
func TestHappyFirstTokenWire(t *testing.T) {
	const meta = `{"type":"codex.response.metadata","metadata":{}}`
	const noWork = `{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"wire rate limited"}}`
	const invalidHTTP = `{"error":{"type":"invalid_request_error","code":"invalid_value","param":"input","message":"parity rejected field"}}`
	for _, ws := range []bool{false, true} {
		for _, route := range []string{"responses", "passthrough", "chat"} {
			chat := route == "chat"
			for _, tc := range []struct {
				name, code string
				calls      int32
			}{
				{"retry_success", "", 2}, {"retry_then_400", "invalid_value", 2},
				{"retry_then_400_off", "invalid_value", 2}, {"retry_then_400_pending", "invalid_value", 2},
				{"stream_400", "invalid_value", 1}, {"output_then_400", "invalid_value", 1},
				{"accepted_then_429", "rate_limit_exceeded", 1}, {"accepted_then_429_off", "rate_limit_exceeded", 1},
				{"usage_then_disconnect", "smart_ws_dispatch_uncertain", 1}, {"preoutput_disconnect", "", 2},
				{"retry_exhausted", "rate_limit_exceeded", 2},
			} {
				if !ws && (tc.name == "usage_then_disconnect" || tc.name == "preoutput_disconnect") {
					continue
				}
				pending := tc.name == "retry_then_400_pending"
				if (pending || tc.name == "retry_then_400_off") && (ws || chat) {
					continue // The second attempt is an actual HTTP 400, not a WS error event.
				}
				t.Run(fmt.Sprintf("ws=%t/%s/%s", ws, route, tc.name), func(t *testing.T) {
					gate := make(chan struct{})
					release := sync.OnceFunc(func() { close(gate) })
					defer release()
					deltaMode := route == "passthrough"
					happyOn := !strings.HasSuffix(tc.name, "_off")
					wantNotice := happyOn && !pending && !chat
					delayMS := 0
					if pending {
						delayMS = 60000 // The request deadline is eight seconds; the real timer cannot fire.
					}
					calls, code := tc.calls, tc.code
					if strings.HasPrefix(tc.name, "accepted_then_429") && !ws && !chat {
						calls, code = 2, ""
					}
					if tc.name == "preoutput_disconnect" && chat {
						// response.created already produced the downstream role chunk.
						calls, code = 1, "smart_ws_dispatch_uncertain"
					}
					f := newParityGateway(t, parityScript{ws: ws, passthrough: deltaMode,
						configure: func(gw *service.OpenAIGatewayService, up *httptest.Server) {
							service.ConfigureHappyWireForTest(t, gw, up, happyOn, deltaMode, delayMS)
						},
						attempt: func(n int32) parityScript {
							if n > 1 {
								if strings.HasPrefix(tc.name, "retry_then_400") {
									return parityScript{http400: invalidHTTP, tail: []string{parityReject}}
								}
								if tc.name == "retry_exhausted" {
									return parityScript{tail: []string{noWork}}
								}
								return parityScript{tail: []string{parityCreated, parityDelta, parityDone}}
							}
							p := parityScript{prefix: []string{meta}, gate: gate}
							switch tc.name {
							case "retry_success", "retry_then_400", "retry_then_400_off", "retry_then_400_pending", "retry_exhausted":
								p.tail = []string{noWork}
							case "accepted_then_429", "accepted_then_429_off":
								p.prefix, p.tail = []string{parityCreated}, []string{noWork}
							case "preoutput_disconnect":
								p.prefix = []string{parityCreated}
							case "usage_then_disconnect":
								p.tail = []string{parityCreated, `{"type":"response.metadata","response":{"id":"resp_parity","usage":{"input_tokens":9,"output_tokens":0}}}`}
							case "output_then_400":
								p.tail = []string{parityCreated, parityDelta, parityReject}
							case "stream_400":
								p.tail = []string{parityReject}
							}
							return p
						},
					})
					path, body := "/v1/responses", parityResponses
					if chat {
						path, body = "/v1/chat/completions", parityChat
					}
					if !wantNotice {
						release()
					}
					resp := f.post(t, path, body)
					r := bufio.NewReader(resp.Body)
					if wantNotice {
						readHappyWireNotice(t, f, r, deltaMode)
						release()
					}
					wire := parityRest(t, r)
					t.Logf("HTTP %d %s; HTTP POST=%d WS create=%d; tail=%q", resp.StatusCode, resp.Header.Get("Content-Type"), f.http.Load(), f.ws.Load(), wire)
					if ws {
						require.Equal(t, calls, f.ws.Load())
						require.Zero(t, f.http.Load())
					} else {
						require.Equal(t, calls, f.http.Load())
						require.Zero(t, f.ws.Load()+f.upgrades.Load())
					}
					require.NotContains(t, wire, "gateway.status")
					if pending || tc.name == "retry_then_400_off" {
						require.Equal(t, 400, resp.StatusCode, "no notification committed the HTTP response")
						if route == "responses" {
							require.JSONEq(t, invalidHTTP, wire)
						}
					}
					if resp.StatusCode >= 400 {
						require.True(t, !wantNotice && tc.name != "output_then_400" && code != "")
						status := map[string]int{"invalid_value": 400, "rate_limit_exceeded": 429, "smart_ws_dispatch_uncertain": 502}[code]
						require.Equal(t, status, resp.StatusCode)
						require.True(t, gjson.Valid(wire))
						if chat && !ws && tc.name == "retry_then_400" {
							// Upstream v0.2.22's uncommitted HTTP Chat writer exposes type/message.
							require.Equal(t, "invalid_request_error", gjson.Get(wire, "error.type").Str)
							require.Equal(t, "parity rejected field", gjson.Get(wire, "error.message").Str)
						} else if route == "passthrough" && (pending || tc.name == "retry_then_400_off") {
							require.JSONEq(t, `{"error":{"type":"upstream_error","message":"Upstream request failed"}}`, wire)
						} else {
							require.Equal(t, code, gjson.Get(wire, "error.code").Str)
						}
						if chat {
							require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
						}
						return
					}
					require.Equal(t, 200, resp.StatusCode)
					require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
					errs := parityErrorEvents(t, wire)
					if code == "" {
						require.Empty(t, errs)
						require.Contains(t, wire, "visible-parity")
						terminal := `"type":"response.completed"`
						if chat {
							terminal = "data: [DONE]"
						}
						require.Equal(t, 1, strings.Count(wire, terminal))
						return
					}
					require.Len(t, errs, 1)
					require.NotContains(t, wire, "response.completed")
					require.NotContains(t, wire, "[DONE]")
					if tc.name == "output_then_400" {
						require.Contains(t, strings.Split(wire, errs[0].Raw)[0], "visible-parity")
					}
					if tc.name == "usage_then_disconnect" && !chat {
						// A transport interruption is not an explicit provider refusal. Its
						// existing Responses error has top-level code/message, not WS error.*.
						require.Equal(t, "error", errs[0].Get("type").Str)
						require.Equal(t, code, errs[0].Get("code").Str)
						require.Contains(t, errs[0].Get("message").Str, "not replayed")
						return
					}
					errPath := "response.error"
					if chat {
						errPath = "error"
						require.False(t, errs[0].Get("type").Exists())
						if ws && strings.HasPrefix(tc.name, "accepted_then_429") {
							require.Contains(t, strings.Split(wire, errs[0].Raw)[0], `"role":"assistant"`)
							require.EqualValues(t, 429, errs[0].Get("error.status_code").Int())
						}
					} else if ws && (tc.name == "accepted_then_429" || (route == "responses" && (tc.name == "stream_400" || tc.name == "output_then_400"))) {
						errPath = "error"
						expected := parityReject
						if tc.name == "accepted_then_429" {
							expected = noWork
						}
						require.JSONEq(t, expected, errs[0].Raw)
					} else {
						require.Equal(t, "response.failed", errs[0].Get("type").Str)
						require.Contains(t, wire, "event: response.failed\ndata: ")
					}
					wantCode := code
					// HTTP passthrough deliberately sanitizes a non-stream upstream 400.
					if !ws && route == "passthrough" && tc.name == "retry_then_400" {
						wantCode = "upstream_error"
					}
					require.Equal(t, wantCode, errs[0].Get(errPath+".code").Str)
					require.NotEmpty(t, errs[0].Get(errPath+".message").Str)
					if !ws && !chat && tc.name == "retry_then_400" {
						require.EqualValues(t, 400, errs[0].Get(errPath+".status_code").Int())
						if route == "responses" {
							require.Equal(t, "input", errs[0].Get(errPath+".param").Str)
						}
						if route == "passthrough" {
							require.NotContains(t, wire, "parity rejected field")
							require.NotContains(t, wire, "invalid_value")
						}
					}
				})
			}
		}
	}
}

func readHappyWireNotice(t *testing.T, f *parityGateway, r *bufio.Reader, delta bool) {
	t.Helper()
	kind, extra := "gateway.status", ""
	if delta {
		kind, extra = "gateway.status.delta", `,"delta":"upstream_connected"`
	}
	want := fmt.Sprintf("event: %s\ndata: {\"type\":%q%s,\"metadata\":{\"gateway_status\":\"upstream_connected\"}}\n\n", kind, kind, extra)
	require.Equal(t, want, parityUntil(t, r, "upstream_connected"))
	require.EqualValues(t, 1, f.ws.Load()+f.http.Load(), "notification is after a model dispatch")
}

func TestHappyFirstTokenSmartWSWirePayloads(t *testing.T) {
	const created = `{"type":"response.created","response":{"id":"resp_parity","model":"gpt-5.1","created_at":17,"status":"in_progress","output":[]}}`
	const nested = `{"type":"error","status":502,"error":{"type":"server_error","code":"server_error","message":"original wire error"},"usage":{"input_tokens":9},"trace":{"example":42}}`
	const flat = `{"type":"error","status":502,"code":"server_error","message":"original flat error","usage":{"input_tokens":9},"trace":{"example":42}}`
	const failed = `{"type":"response.failed","sequence_number":7,"response":{"id":"resp_parity","created_at":17,"status":"failed","output":[],"error":{"status":502,"code":"server_error","type":"server_error","message":"authoritative error"},"usage":{"input_tokens":9,"output_tokens":1}},"trace":{"example":42}}`
	for _, pass := range []bool{false, true} {
		for _, tc := range []struct {
			name string
			tail []string
		}{
			{"bare_nested", []string{nested}},
			{"bare_flat", []string{flat}},
			{"failed", []string{failed}},
			{"bare_then_failed", []string{nested, failed}},
		} {
			t.Run(fmt.Sprintf("pass=%t/%s", pass, tc.name), func(t *testing.T) {
				var withoutNotice string
				for _, on := range []bool{false, true} {
					gate := make(chan struct{})
					release := sync.OnceFunc(func() { close(gate) })
					defer release()
					f := newParityGateway(t, parityScript{ws: true, passthrough: pass, prefix: []string{created},
						gate: gate, tail: append([]string{parityDelta}, tc.tail...),
						configure: func(gw *service.OpenAIGatewayService, up *httptest.Server) {
							service.ConfigureHappyWireForTest(t, gw, up, on, pass)
						},
					})
					if !on {
						release()
					}
					resp := f.post(t, "/v1/responses", parityResponses)
					r := bufio.NewReader(resp.Body)
					if on {
						readHappyWireNotice(t, f, r, pass)
						release()
					}
					wire := parityRest(t, r)
					f.checkTransport(t, true)
					require.Equal(t, 200, resp.StatusCode)
					require.Contains(t, wire, "visible-parity")
					require.NotContains(t, wire, "gateway.status")
					events := parityErrorEvents(t, wire)
					require.Len(t, events, 1)
					err := events[0]
					t.Logf("notification=%t; WS create=1; terminal=%s", on, err.Raw)
					// Both modes must expose the authoritative same-turn usage before
					// downstream consumers decide whether the operation can be retried.
					if tc.name == "bare_then_failed" || tc.name == "failed" {
						expected, deleteErr := sjson.Delete(failed, "trace")
						require.NoError(t, deleteErr)
						require.JSONEq(t, expected, err.Raw)
					} else {
						require.Equal(t, "response.failed", err.Get("type").Str)
						require.Equal(t, "resp_parity", err.Get("response.id").Str)
						require.EqualValues(t, 17, err.Get("response.created_at").Int())
						require.EqualValues(t, 9, err.Get("response.usage.input_tokens").Int())
						message := gjson.Get(tc.tail[0], "error.message").Str
						if message == "" {
							message = gjson.Get(tc.tail[0], "message").Str
						}
						require.Equal(t, message, err.Get("response.error.message").Str)
						require.Equal(t, "server_error", err.Get("response.error.code").Str)
					}
					require.False(t, err.Get("trace").Exists())
					if on {
						require.JSONEq(t, withoutNotice, err.Raw)
					} else {
						withoutNotice = err.Raw
					}
				}
			})
		}
	}
}
