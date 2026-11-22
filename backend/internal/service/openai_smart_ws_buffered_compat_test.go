//go:build unit

package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// A local upstream that records each dispatched request, even if the gateway
// reuses the same socket. All frames are synthetic, with no external account.
func smartWSReplayPeer(requests chan<- []byte, frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			select {
			case requests <- payload:
			default:
				return
			}
			for _, frame := range frames {
				if conn.WriteMessage(websocket.TextMessage, []byte(frame)) != nil {
					return
				}
			}
		}
	}
}

// In .49 a tool that finished before the message came back first in Responses,
// and Chat returned usage but neither the text nor the tool call.
func TestSmartWSBufferedOutOfOrderResults(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{"complete_items", `{"type":"response.output_item.done","response_id":"resp_force","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"local reply"}]}}`},
		{"message_only_in_delta", `{"type":"response.output_text.delta","response_id":"resp_force","output_index":0,"content_index":0,"item_id":"msg_1","delta":"local reply"}`},
	} {
		for _, protocol := range []string{"responses", "chat"} {
			t.Run(tc.name+"/"+protocol, func(t *testing.T) {
				requests := make(chan []byte, 4)
				svc, account, fallback := forcedWSService(t, smartWSReplayPeer(requests, forcedWSCreated,
					`{"type":"response.output_item.done","response_id":"resp_force","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_temperature","arguments":"{}","status":"completed"}}`,
					tc.message,
					`{"type":"response.completed","response":{"id":"resp_force","model":"gpt-5.1","status":"completed","output":[],"usage":{"input_tokens":12,"output_tokens":7}}}`))
				path := "/v1/responses"
				body := []byte(`{"model":"gpt-5.1","input":"hello","stream":false}`)
				if protocol == "chat" {
					path = "/v1/chat/completions"
					body = []byte(`{"model":"gpt-5.1","messages":[{"role":"user","content":"hello"}],"stream":false}`)
				}
				c, rec := forcedWSContext(t, path, 1, "out-of-order")
				var result *OpenAIForwardResult
				var err error
				if protocol == "chat" {
					result, err = svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
				} else {
					result, err = svc.Forward(c.Request.Context(), c, account, body)
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.OpenAIWSMode)
				require.Equal(t, http.StatusOK, rec.Code)
				require.Len(t, requests, 1)
				require.Zero(t, fallback.callCount)
				require.Equal(t, 12, result.Usage.InputTokens)
				require.Equal(t, 7, result.Usage.OutputTokens)
				res := gjson.ParseBytes(rec.Body.Bytes())
				if protocol == "chat" {
					require.Equal(t, "local reply", res.Get("choices.0.message.content").String())
					calls := res.Get("choices.0.message.tool_calls").Array()
					require.Len(t, calls, 1)
					require.Equal(t, "call_1", calls[0].Get("id").String())
					require.Equal(t, "get_temperature", calls[0].Get("function.name").String())
					require.Equal(t, "{}", calls[0].Get("function.arguments").String())
					return
				}
				items := res.Get("output").Array()
				require.Len(t, items, 2)
				require.Equal(t, "msg_1", items[0].Get("id").String())
				require.Equal(t, "local reply", items[0].Get("content.0.text").String())
				require.Equal(t, "call_1", items[1].Get("call_id").String())
				require.Equal(t, "get_temperature", items[1].Get("name").String())
				require.Equal(t, "{}", items[1].Get("arguments").String())
			})
		}
	}
}

func TestSmartWSPrettyCompletionIsOneSSEDataLine(t *testing.T) {
	requests := make(chan []byte, 4)
	svc, account, fallback := forcedWSService(t, smartWSReplayPeer(requests, forcedWSCreated,
		`{"type":"response.completed","response":{"id":"resp_force","status":"completed","model":"gpt-5.1","output":[],"usage":{
		"input_tokens":12,
		"output_tokens":7}}}`))
	c, rec := forcedWSContext(t, "/v1/responses", 1, "pretty-frame")
	result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","input":"hello","stream":true}`))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Len(t, requests, 1)
	require.Zero(t, fallback.callCount)
	completed := 0
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			continue
		}
		require.True(t, json.Valid([]byte(data)), "incomplete data line: %s", line)
		if gjson.Get(data, "type").String() == "response.completed" {
			completed++
			require.EqualValues(t, 7, gjson.Get(data, "response.usage.output_tokens").Int())
		}
	}
	require.Equal(t, 1, completed)
}

func TestSmartWSNativeIDESelectionOnWire(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			requests := make(chan []byte, 4)
			svc, account, fallback := forcedWSService(t, smartWSReplayPeer(requests, forcedWSCreated, forcedWSCompleted))
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = mode
			const selection = `{"type":"function_call_output","namespace":"agent","name":"ide_context","output":"local selection"}`
			frames, _ := nativeContractFramesWithCompletion(t, svc, account, true,
				`{"type":"response.create","model":"gpt-5.1","input":[`+selection+`,{"role":"user","content":"hello"}]}`)
			require.NotEmpty(t, frames)
			require.Equal(t, "response.completed", gjson.GetBytes(frames[len(frames)-1], "type").String())
			require.Len(t, requests, 1)
			require.Zero(t, fallback.callCount)
			require.JSONEq(t, selection, gjson.GetBytes(<-requests, "input.0").Raw)
		})
	}
}
