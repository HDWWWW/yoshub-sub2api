//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const encryptedWireInput = `[
 {"type":"reasoning","summary":[],"encrypted_content":"reason+/=","opaque":{"n":9007199254740993}},
 {"type":"compaction","encrypted_content":"compact+/="},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible"},{"type":"encrypted_content","envelope":{"n":9007199254740993,"value":"sealed"}}]},
 {"type":"function_call","name":"lookup","call_id":"call_pair","arguments":"{}"},
 {"type":"function_call_output","call_id":"call_pair","output":"{\"encrypted_content\":\"application data\"}"},
 {"role":"user","content":"suffix"}]`

const encryptedWireError = `{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified"}}`
const encryptedWireFailed = `{"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"The encrypted content could not be verified"}}}`

func encryptedDispatchContext(t *testing.T, smart bool) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, rec := forcedWSContext(t, "/v1/responses", 12, "encrypted-thread")
	groupID := int64(7)
	c.Set("api_key", &APIKey{ID: 12, GroupID: &groupID})
	c.Request.Header.Set("session_id", "encrypted-session")
	if !smart {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, &Group{ID: groupID, Platform: PlatformOpenAI, SmartWSMode: "disabled"}))
	}
	return c, rec
}

func encryptedDispatchResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func requireEncryptedWireInput(t *testing.T, body []byte, input string) {
	t.Helper()
	var want, got []any
	require.NoError(t, decodeOpenAIJSONUseNumber([]byte(input), &want))
	require.NoError(t, decodeOpenAIJSONUseNumber([]byte(gjson.GetBytes(body, "input").Raw), &got))
	require.Equal(t, want, got, "unexpected rewrite of the dispatched input")
}

// Passthrough is a transport choice, not permission to resubmit a different
// history. A request-level 400 cannot identify a bad item among several blobs.
func TestEncryptedDispatchPassthroughHTTPRejection(t *testing.T) {
	s, a, h := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected WS dial") })
	a.Extra["openai_passthrough"] = true
	h.responses = []*http.Response{encryptedDispatchResponse(400, encryptedWireError), encryptedDispatchResponse(200, `{"id":"resp_unwanted_retry"}`)}
	c, rec := encryptedDispatchContext(t, false)
	body := []byte(`{"model":"gpt-5.1","input":` + encryptedWireInput + `,"previous_response_id":"resp_original"}`)
	_, err := s.Forward(c.Request.Context(), c, a, body)
	require.Len(t, h.bodies, 1, "the rejected turn must not be stripped and sent a second time")
	require.Error(t, err)
	var retry *UpstreamFailoverError
	require.NotErrorAs(t, err, &retry)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	requireEncryptedWireInput(t, h.bodies[0], encryptedWireInput)
	require.Equal(t, "resp_original", gjson.GetBytes(h.bodies[0], "previous_response_id").String())
	require.False(t, s.getOpenAIWSStateStore().HasAnySessionInvalidEncryptedContent(), "an unattributed rejection must not poison future turns")
}

// Non-passthrough OpenAI already has a bounded, legacy recovery. Its scope is
// top-level reasoning/compaction, not message ciphertext even if the bytes match.
func TestEncryptedDispatchOrdinaryLegacyScope(t *testing.T) {
	const message = `{"type":"message","role":"assistant","encrypted_content":"shared+/=","content":[{"type":"encrypted_content","encrypted_content":"message-only+/="},{"type":"output_text","text":"keep"}],"opaque":9007199254740993}`
	const user = `{"role":"user","content":"next"}`
	const reasoning = `{"type":"reasoning","summary":[],"encrypted_content":"shared+/=","opaque":9007199254740993}`
	const recovered = `{"type":"reasoning","summary":[],"opaque":9007199254740993}`
	const compaction = `{"type":"compaction","encrypted_content":"compaction+/="}`
	for _, tc := range []struct {
		name, input, after string
		retries            bool
	}{
		{"message_only", `[` + message + `,` + user + `]`, `[` + message + `,` + user + `]`, false},
		{"mixed", `[` + reasoning + `,` + compaction + `,` + message + `,` + user + `]`, `[` + recovered + `,` + message + `,` + user + `]`, true},
	} {
		for _, seeded := range []bool{false, true} {
			name := "/rejection"
			if seeded {
				name = "/seeded"
			}
			t.Run(tc.name+name, func(t *testing.T) {
				s, a, h := forcedWSService(t, func(http.ResponseWriter, *http.Request) { t.Error("unexpected WS") })
				c, rec := encryptedDispatchContext(t, false)
				body := []byte(`{"model":"gpt-5.1","input":` + tc.input + `}`)
				success := encryptedDispatchResponse(200, `{"id":"resp_ok","usage":{"input_tokens":2,"output_tokens":1}}`)
				if seeded {
					// Existing .44 entries can contain both an official and an extended item digest.
					s.getOpenAIWSStateStore().MarkSessionInvalidEncryptedContent(7, s.GenerateSessionHash(c, body), collectOpenAIEncryptedContentDigestsRaw(body), time.Minute)
					h.responses = []*http.Response{success}
				} else {
					h.responses = []*http.Response{encryptedDispatchResponse(400, encryptedWireError), success}
				}
				_, err := s.Forward(c.Request.Context(), c, a, body)
				if seeded {
					require.NoError(t, err)
					require.Len(t, h.bodies, 1)
					requireEncryptedWireInput(t, h.bodies[0], tc.after)
				} else if tc.retries {
					require.NoError(t, err)
					require.Len(t, h.bodies, 2)
					requireEncryptedWireInput(t, h.bodies[0], tc.input)
					requireEncryptedWireInput(t, h.bodies[1], tc.after)
					marked := s.getOpenAIWSStateStore().GetSessionInvalidEncryptedContentDigests(7, s.GenerateSessionHash(c, body))
					require.Len(t, marked, 2)
					require.Contains(t, marked, openAIEncryptedContentDigest("shared+/="))
					require.Contains(t, marked, openAIEncryptedContentDigest("compaction+/="))
				} else {
					require.Len(t, h.bodies, 1)
					require.Error(t, err)
					require.Equal(t, 400, rec.Code)
					requireEncryptedWireInput(t, h.bodies[0], tc.input)
					require.False(t, s.getOpenAIWSStateStore().HasAnySessionInvalidEncryptedContent())
				}
			})
		}
	}
}

// The old cache key is group + client session, not the current decryption
// workspace. Seed the actual state store as a previous request could have left
// it, then inspect the first outbound generation, not an intermediate builder.
func TestEncryptedDispatchPassthroughIgnoresUnownedLineage(t *testing.T) {
	for _, route := range []string{"ws", "continuation_http", "oversized_http"} {
		t.Run(route, func(t *testing.T) {
			type observed struct {
				body    []byte
				headers http.Header
			}
			wire := make(chan observed, 4)
			s, a, h := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					_, msg, err := conn.ReadMessage()
					if err != nil {
						return
					}
					wire <- observed{msg, r.Header.Clone()}
					if conn.WriteMessage(websocket.TextMessage, []byte(encryptedWireFailed)) != nil {
						return
					}
				}
			})
			a.Type = AccountTypeOAuth
			a.Credentials = map[string]any{"access_token": "synthetic-token", "chatgpt_account_id": "workspace-B"}
			a.Extra["openai_passthrough"] = true
			c, rec := encryptedDispatchContext(t, true)
			c.Request.Header.Set("ChatGPT-Account-ID", "client-not-selected")
			input, prev := encryptedWireInput, ""
			switch route {
			case "continuation_http":
				// Public HTTP continuation is admitted on API-key accounts, not OAuth.
				a.Type = AccountTypeAPIKey
				a.Credentials = map[string]any{"api_key": "synthetic-token", "base_url": "https://api.openai.com"}
				prev = `,"previous_response_id":"resp_original"`
			case "oversized_http":
				s.cfg.Gateway.OpenAIWS.MaxRequestBytes = 2048
				input = strings.Replace(input, "suffix", strings.Repeat("x", 4096)+"suffix", 1)
			}
			body := []byte(`{"model":"gpt-5.1","stream":true,"input":` + input + prev + `}`)
			store := s.getOpenAIWSStateStore()
			hash := s.GenerateSessionHash(c, body)
			require.NotEmpty(t, hash)
			digests := collectOpenAIEncryptedContentDigestsRaw(body)
			require.Len(t, digests, 3)
			store.MarkSessionInvalidEncryptedContent(7, hash, digests, time.Minute)
			h.responses = []*http.Response{encryptedDispatchResponse(400, encryptedWireError)}
			_, err := s.Forward(c.Request.Context(), c, a, body)
			var stopped *UpstreamFailoverError
			require.ErrorAs(t, err, &stopped)
			require.False(t, stopped.ShouldRetryNextAccount())
			require.False(t, stopped.RetryableOnSameAccount)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "invalid_encrypted_content")
			var sent observed
			if route == "ws" {
				require.Zero(t, h.callCount)
				require.Len(t, wire, 1)
				sent = <-wire
				require.Equal(t, "response.create", gjson.GetBytes(sent.body, "type").String())
			} else {
				require.Empty(t, wire)
				require.Len(t, h.bodies, 1)
				sent = observed{h.bodies[0], h.reqs[0].Header}
				if prev != "" {
					require.Equal(t, "resp_original", gjson.GetBytes(sent.body, "previous_response_id").String())
				}
			}
			require.Equal(t, "Bearer synthetic-token", sent.headers.Get("Authorization"))
			if a.IsOpenAIOAuthLike() {
				require.Equal(t, "workspace-B", sent.headers.Get("ChatGPT-Account-ID"))
			} else {
				require.Empty(t, sent.headers.Get("ChatGPT-Account-ID"))
			}
			requireEncryptedWireInput(t, sent.body, input)
		})
	}
}

func TestEncryptedDispatchSelectedWorkspace(t *testing.T) {
	testEncryptedDispatchSelectedWorkspace(t, false)
}

func TestIndependentSmartWSStreamingReuseAfterComplete(t *testing.T) {
	testEncryptedDispatchSelectedWorkspace(t, true)
}

func testEncryptedDispatchSelectedWorkspace(t *testing.T, stream bool) {
	t.Helper()
	type observed struct {
		connID    int32
		workspace string
		bearer    string
		body      []byte
	}
	var handshakes, responses atomic.Int32
	wire := make(chan observed, 4)
	s, a, h := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		id := handshakes.Add(1)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			wire <- observed{id, r.Header.Get("ChatGPT-Account-ID"), r.Header.Get("Authorization"), raw}
			responseID := "resp_workspace_" + strconv.Itoa(int(responses.Add(1)))
			if conn.WriteMessage(websocket.TextMessage, []byte(strings.Replace(forcedWSCompleted, "resp_force", responseID, 1))) != nil {
				return
			}
		}
	})
	a.Type = AccountTypeOAuth
	a.Extra["openai_passthrough"] = true
	var connections []int32
	for _, workspace := range []string{"workspace-A", "workspace-A", "workspace-B"} {
		a.Credentials = map[string]any{"access_token": "synthetic-token", "chatgpt_account_id": workspace}
		c, _ := encryptedDispatchContext(t, true)
		c.Request.Header.Set("ChatGPT-Account-ID", "client-not-selected")
		_, err := s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":`+strconv.FormatBool(stream)+`,"prompt_cache_key":"encrypted-workspace","input":`+encryptedWireInput+`}`))
		require.NoError(t, err)
		require.Len(t, wire, 1)
		got := <-wire
		require.Equal(t, workspace, got.workspace)
		require.Equal(t, "Bearer synthetic-token", got.bearer)
		requireEncryptedWireInput(t, got.body, encryptedWireInput)
		connections = append(connections, got.connID)
	}
	require.Equal(t, connections[0], connections[1], "same workspace control should exercise socket reuse")
	require.NotEqual(t, connections[1], connections[2], "changing the selected workspace must not reuse its predecessor's handshake")
	require.Zero(t, h.callCount)
}

func TestEncryptedDispatchOutputThenDisconnect(t *testing.T) {
	for _, tc := range []struct {
		name, item string
		wantRetry  bool
	}{
		{"message_cipher", `{"type":"message","role":"assistant","encrypted_content":"sealed"}`, false},
		{"reasoning_cipher_part", `{"type":"reasoning","summary":[],"content":[{"type":"encrypted_content","envelope":{"opaque":"sealed"}}]}`, false},
		{"empty_reasoning", `{"type":"reasoning","encrypted_content":null,"summary":[],"content":[]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dispatches atomic.Int32
			s, a, h := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				dispatches.Add(1)
				if conn.WriteMessage(websocket.TextMessage, []byte(forcedWSCreated)) != nil {
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_item.added","output_index":0,"item":`+tc.item+`}`))
				// Interrupt the transport without a terminal event.
			})
			c, rec := encryptedDispatchContext(t, true)
			_, err := s.Forward(c.Request.Context(), c, a, []byte(`{"model":"gpt-5.1","stream":false,"input":"new turn"}`))
			var failure *UpstreamFailoverError
			require.ErrorAs(t, err, &failure)
			require.EqualValues(t, 1, dispatches.Load())
			require.Zero(t, h.callCount)
			require.Equal(t, tc.wantRetry, failure.ShouldRetryNextAccount(), "encrypted output is evidence of work even without visible text or usage")
			require.Equal(t, tc.wantRetry, failure.RetryableOnSameAccount)
			if !tc.wantRetry {
				require.Contains(t, rec.Body.String(), smartWSUncertainCode)
			}
		})
	}
}

func TestEncryptedDispatchNativeContinuation(t *testing.T) {
	for _, route := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, "large_http"} {
		t.Run(route, func(t *testing.T) {
			wire := make(chan []byte, 4)
			var handshakes atomic.Int32
			s, a, h := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				handshakes.Add(1)
				for turn := 0; ; turn++ {
					_, payload, err := conn.ReadMessage()
					if err != nil {
						return
					}
					wire <- payload
					event := forcedWSCompleted
					if turn != 0 {
						event = encryptedWireFailed
					}
					if conn.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
						return
					}
					// Native passthrough stays connected after a per-turn failure.
					if turn == 1 {
						_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
						return
					}
				}
			})
			first := `{"type":"response.create","model":"gpt-5.1","prompt_cache_key":"encrypted-native","input":[{"role":"user","content":"first"}]}`
			second := `{"type":"response.create","model":"gpt-5.1","prompt_cache_key":"encrypted-native","input":` + encryptedWireInput + `,"previous_response_id":"resp_force"}`
			// nativeContractFrames supplies key 73 and no group or session headers.
			seed, _ := gin.CreateTestContext(httptest.NewRecorder())
			seed.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			scope, _ := resolveOpenAIWSExecutionScope(seed, []byte(second), 73)
			require.NotEmpty(t, scope)
			s.getOpenAIWSStateStore().MarkSessionInvalidEncryptedContent(0, scope, collectOpenAIEncryptedContentDigestsRaw([]byte(second)), time.Minute)
			if route == "large_http" {
				s.cfg.Gateway.OpenAIWS.HTTPBridgeEnabled = true
				s.cfg.Gateway.OpenAIWS.HTTPBridgeThresholdBytes = 2048
				first = strings.Replace(first, `"first"`, `"`+strings.Repeat("1", 4096)+`"`, 1)
				h.responses = []*http.Response{
					{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + forcedWSCompleted + "\n\n"))},
					encryptedDispatchResponse(400, encryptedWireError),
				}
			} else {
				a.Extra["openai_apikey_responses_websockets_v2_mode"] = route
			}
			frames, _ := nativeContractFrames(t, s, a, first, second)
			require.Len(t, frames, 2)
			require.Equal(t, "resp_force", gjson.GetBytes(frames[0], "response.id").String())
			require.Equal(t, "invalid_encrypted_content", openAIStreamFailedEventErrorCode(frames[1]))
			var sent []byte
			if route == "large_http" {
				require.Zero(t, handshakes.Load())
				require.Len(t, h.bodies, 2, "one HTTP dispatch per client turn")
				sent = h.bodies[1]
			} else {
				require.EqualValues(t, 1, handshakes.Load())
				require.Zero(t, h.callCount)
				require.Len(t, wire, 2, "one WS dispatch per client turn on the same connection")
				<-wire
				sent = <-wire
			}
			require.Equal(t, "resp_force", gjson.GetBytes(sent, "previous_response_id").String())
			requireEncryptedWireInput(t, sent, encryptedWireInput)
		})
	}
}
