package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type ingressCancelUpstream struct {
	frames               chan []byte
	written              chan struct{}
	closed               chan struct{}
	writeOnce, closeOnce sync.Once
}

func (c *ingressCancelUpstream) WriteJSON(ctx context.Context, _ any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.writeOnce.Do(func() { close(c.written) })
	return nil
}
func (c *ingressCancelUpstream) ReadMessage(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, errOpenAIWSConnClosed
	case frame := <-c.frames:
		return frame, nil
	}
}
func (c *ingressCancelUpstream) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	frame, err := c.ReadMessage(ctx)
	return coderws.MessageText, frame, err
}
func (c *ingressCancelUpstream) WriteFrame(ctx context.Context, _ coderws.MessageType, data []byte) error {
	return c.WriteJSON(ctx, json.RawMessage(data))
}
func (c *ingressCancelUpstream) Ping(context.Context) error { return nil }
func (c *ingressCancelUpstream) Close() error               { c.closeOnce.Do(func() { close(c.closed) }); return nil }

func TestOpenAIWSIngressDisconnectCancelsActiveTurnAndKeepsObservedUsage(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		for _, phase := range []string{"before_first_output", "after_usage", "after_terminal"} {
			t.Run(mode+"/"+phase, func(t *testing.T) {
				upstream := &ingressCancelUpstream{frames: make(chan []byte, 4), written: make(chan struct{}), closed: make(chan struct{})}
				cfg := &config.Config{}
				cfg.Security.URLAllowlist.Enabled = false
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				cfg.Gateway.OpenAIWS.Enabled, cfg.Gateway.OpenAIWS.OAuthEnabled, cfg.Gateway.OpenAIWS.APIKeyEnabled = true, true, true
				cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2, cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true, true
				cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
				cfg.Gateway.OpenAIWS.MaxConnsPerAccount, cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2, 2
				cfg.Gateway.OpenAIWS.ReadTimeoutSeconds, cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 30, 3
				dialer := &openAIWSSingleConnDialer{conn: upstream}
				pool := newOpenAIWSConnPool(cfg)
				defer pool.Close()
				pool.setClientDialerForTest(dialer)
				svc := &OpenAIGatewayService{cfg: cfg, cache: &stubGatewayCache{}, httpUpstream: &httpUpstreamRecorder{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool, openaiWSPassthroughDialer: dialer}
				account := &Account{ID: 9912, Name: "cancel-fixture", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 2,
					Credentials: map[string]any{"api_key": "sk-test"}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": mode}}
				// A different request is already using another lease on this same
				// account. Aborting our turn must leave that connection untouched.
				otherUpstream := &ingressCancelUpstream{frames: make(chan []byte), written: make(chan struct{}), closed: make(chan struct{})}
				otherConn := newOpenAIWSConn("other-active-turn", account.ID, otherUpstream, nil)
				require.True(t, otherConn.tryAcquire())
				accountPool := pool.getOrCreateAccountPool(account.ID)
				accountPool.mu.Lock()
				accountPool.conns[otherConn.id] = otherConn
				accountPool.mu.Unlock()
				defer otherConn.release()
				type turnOutcome struct {
					result *OpenAIForwardResult
					err    error
				}
				turns := make(chan turnOutcome, 4)
				serverDone := make(chan error, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := coderws.Accept(w, r, nil)
					if err != nil {
						serverDone <- err
						return
					}
					defer conn.CloseNow()
					_, first, err := conn.Read(r.Context())
					if err != nil {
						serverDone <- err
						return
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = r
					hooks := &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, err error) { turns <- turnOutcome{result, err} }}
					serverDone <- svc.ProxyResponsesWebSocketFromClient(r.Context(), c, conn, account, "sk-test", first, hooks)
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				require.NoError(t, err)
				defer client.CloseNow()
				require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":true}`)))
				select {
				case <-upstream.written:
				case <-ctx.Done():
					t.Fatal("upstream never received request")
				}
				if phase != "before_first_output" {
					event := `{"type":"response.in_progress","response":{"id":"resp_partial","model":"gpt-5.1","usage":{"input_tokens":7,"output_tokens":2}}}`
					if phase == "after_terminal" {
						event = strings.Replace(event, "response.in_progress", "response.completed", 1)
					}
					upstream.frames <- []byte(event)
					_, _, err := client.Read(ctx)
					require.NoError(t, err)
				}
				require.NoError(t, client.CloseNow())
				select {
				case err := <-serverDone:
					if phase == "after_terminal" {
						require.NoError(t, err)
					} else {
						require.ErrorIs(t, err, context.Canceled)
					}
				case <-time.After(time.Second):
					t.Fatal("client close did not promptly cancel the active turn")
				}
				select {
				case turn := <-turns:
					if phase == "after_terminal" {
						require.NoError(t, turn.err)
					} else {
						require.ErrorIs(t, turn.err, context.Canceled)
					}
					require.NotNil(t, turn.result)
					if phase == "before_first_output" {
						require.Zero(t, turn.result.Usage.InputTokens)
					} else {
						require.Equal(t, 7, turn.result.Usage.InputTokens)
						require.Equal(t, 2, turn.result.Usage.OutputTokens)
					}
					if phase != "after_terminal" {
						require.True(t, turn.result.ClientDisconnect)
					}
				case <-time.After(time.Second):
					t.Fatal("turn did not settle")
				}
				if phase != "after_terminal" {
					select {
					case <-upstream.closed:
					case <-time.After(time.Second):
						t.Fatal("upstream connection remained alive")
					}
				}
				require.Empty(t, turns, "settle the turn once")
				select {
				case <-otherUpstream.closed:
					t.Fatal("cancellation closed another active request")
				default:
				}
				require.NoError(t, otherConn.writeJSONWithTimeout(ctx, json.RawMessage(`{"type":"response.create"}`), time.Second))
			})
		}
	}
}

func TestOpenAIWSIngressReaderKeepsQueuedFramesAcrossAttemptsAndStopsAtBound(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(map[bool]string{false: "retain_next_turn", true: "bounded_queue"}[overflow], func(t *testing.T) {
			readers := make(chan *openAIWSIngressClientReader, 1)
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				conn, err := coderws.Accept(w, req, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				r := openAIWSIngressReader(c, conn)
				if openAIWSIngressReader(c, conn) != r {
					panic("reader not reused")
				}
				readers <- r
				<-done
			}))
			defer server.Close()
			defer close(done)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			r := <-readers
			count := 2
			if overflow {
				count = 17
			}
			for i := 0; i < count; i++ {
				_ = client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1"}`))
			}
			if overflow {
				select {
				case <-r.done:
				case <-ctx.Done():
					t.Fatal("queue must be bounded")
				}
				var closeErr *OpenAIWSClientCloseError
				require.ErrorAs(t, r.err, &closeErr)
				require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
			} else {
				for i := 0; i < count; i++ {
					_, payload, err := r.read(ctx, time.Second)
					require.NoError(t, err)
					require.Contains(t, string(payload), "response.create")
				}
				require.NoError(t, client.CloseNow())
				select {
				case <-r.done:
				case <-ctx.Done():
					t.Fatal("raw reader leaked after socket close")
				}
			}
		})
	}
}
