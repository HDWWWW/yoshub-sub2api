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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSmartWSObservedUsageSurvivesFailure(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, passthrough := range []bool{false, true} {
				for _, terminal := range []string{"absent", "zero", "authoritative", "unrelated_tail", "disconnect"} {
					t.Run(fmt.Sprintf("ws=%t/stream=%t/passthrough=%t/%s", ws, stream, passthrough, terminal), func(t *testing.T) {
						frames := []string{`{"type":"response.in_progress","response":{"id":"resp_observed","status":"in_progress","model":"gpt-5.1","usage":{"input_tokens":13,"output_tokens":2,"input_tokens_details":{"cached_tokens":3}}}}`}
						usage := ""
						input, output, cached := 13, 2, 3
						switch terminal {
						case "zero":
							usage = `,"usage":{"input_tokens":0,"output_tokens":0}`
						case "authoritative":
							usage = `,"usage":{"input_tokens":5,"output_tokens":1}`
							input, output, cached = 5, 1, 0
						}
						failure := `{"type":"response.failed","response":{"id":"resp_observed","status":"failed","error":{"code":"audit_failure","message":"original explicit error"}` + usage + `}}`
						if terminal == "unrelated_tail" {
							frames = append(frames, `{"type":"error","response_id":"resp_observed","error":{"code":"audit_failure","message":"original explicit error"}}`,
								`{"type":"response.failed","response":{"id":"resp_unrelated","status":"failed","usage":{"input_tokens":999,"output_tokens":999}}}`)
						} else if terminal != "disconnect" {
							frames = append(frames, failure)
						}
						var writes atomic.Int32
						svc, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
							if !ws {
								w.WriteHeader(426)
								return
							}
							conn, err := forcedWSUpgrader.Upgrade(w, r, nil)
							if err != nil {
								return
							}
							defer conn.Close()
							if _, _, err := conn.ReadMessage(); err != nil {
								return
							}
							writes.Add(1)
							for _, frame := range frames {
								if conn.WriteMessage(websocket.TextMessage, []byte(frame)) != nil {
									return
								}
							}
							if terminal != "disconnect" {
								_, _, _ = conn.ReadMessage()
							}
						})
						account.Extra["openai_passthrough"] = passthrough
						body := "data: " + strings.Join(frames, "\n\ndata: ") + "\n\n"
						transport.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}}
						c, rec := forcedWSContext(t, "/v1/responses", 1, "observed-usage-before-failure")
						result, err := svc.Forward(c.Request.Context(), c, account, []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%t,"input":"synthetic audit","reasoning":{"effort":"high"},"service_tier":"priority"}`, stream)))
						require.Error(t, err)
						require.NotNil(t, result, "observed usage must survive the failed turn")
						assert.Equal(t, input, result.Usage.InputTokens)
						assert.Equal(t, output, result.Usage.OutputTokens)
						assert.Equal(t, cached, result.Usage.CacheReadInputTokens)
						assert.Equal(t, ws, result.OpenAIWSMode)
						assert.Equal(t, "high", *result.ReasoningEffort)
						assert.Equal(t, "priority", *result.ServiceTier)
						assert.Equal(t, 1, int(writes.Load())+transport.callCount, "no replay")
						if terminal == "disconnect" {
							assert.Contains(t, rec.Body.String(), smartWSUncertainCode)
						} else {
							assert.Contains(t, rec.Body.String(), "audit_failure")
							assert.NotContains(t, rec.Body.String(), smartWSUncertainCode)
						}
					})
				}
			}
		}
	}
}
