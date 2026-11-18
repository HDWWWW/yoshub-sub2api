//go:build unit

package service_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/server/routes"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	parityCreated    = `{"type":"response.created","response":{"id":"resp_parity","model":"gpt-5.1","status":"in_progress","output":[]}}`
	parityBeat       = `{"type":"keepalive","sequence_number":17}`
	parityDelta      = `{"type":"response.output_text.delta","item_id":"msg_parity","output_index":0,"content_index":0,"delta":"visible-parity"}`
	parityDone       = `{"type":"response.completed","response":{"id":"resp_parity","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible-parity"}]}],"usage":{"input_tokens":3,"output_tokens":2}}}`
	parityReject     = `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"invalid_value","message":"parity rejected field"}}`
	parityAuth       = `{"type":"error","status":401,"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"Incorrect API key provided"}}`
	parityQuota      = `{"type":"error","status":429,"error":{"type":"usage_limit_reached","code":"rate_limit_exceeded","message":"The usage limit has been reached","resets_in_seconds":60}}`
	parityPolicy     = `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"parity content policy"}}`
	parityFailedAuth = `{"type":"response.failed","response":{"id":"resp_parity","status":"failed","error":{"status":401,"code":"invalid_api_key","type":"invalid_request_error","message":"Incorrect API key provided"}}}`
	parityFailed400  = `{"type":"response.failed","sequence_number":9,"response":{"id":"resp_parity","object":"response","created_at":17,"status":"failed","output":[],"usage":{"input_tokens":3,"output_tokens":2},"error":{"status":400,"type":"invalid_request_error","code":"invalid_value","message":"parity rejected field"}}}`
	parityResponses  = `{"model":"gpt-5.1","stream":true,"input":"parity"}`
	parityChat       = `{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"parity"}]}`
	parityMessages   = `{"model":"gpt-5.1","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"parity"}]}`
)

type parityAccounts struct {
	service.AccountRepository
	a           service.Account
	auth, quota atomic.Int32
}

func (p *parityAccounts) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	return []service.Account{p.a}, nil
}
func (p *parityAccounts) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, platform string) ([]service.Account, error) {
	return p.ListSchedulableByPlatform(ctx, platform)
}
func (p *parityAccounts) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return p.ListSchedulableByPlatform(ctx, platform)
}
func (p *parityAccounts) GetByID(context.Context, int64) (*service.Account, error) {
	a := p.a
	return &a, nil
}
func (p *parityAccounts) SetError(context.Context, int64, string) error { p.auth.Add(1); return nil }
func (p *parityAccounts) SetRateLimited(context.Context, int64, time.Time) error {
	p.quota.Add(1)
	return nil
}
func (p *parityAccounts) SetRateLimitedIfLater(ctx context.Context, id int64, until time.Time) error {
	return p.SetRateLimited(ctx, id, until)
}

type paritySlots struct {
	service.ConcurrencyCache
	held atomic.Bool
}

func (s *paritySlots) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	return !s.held.Load(), nil
}
func (*paritySlots) ReleaseUserSlot(context.Context, int64, string) error         { return nil }
func (*paritySlots) IncrementWaitCount(context.Context, int64, int) (bool, error) { return true, nil }
func (*paritySlots) DecrementWaitCount(context.Context, int64) error              { return nil }
func (*paritySlots) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return true, nil
}
func (*paritySlots) ReleaseAccountSlot(context.Context, int64, string) error { return nil }

type parityUsage struct{ service.UsageLogRepository }

func (*parityUsage) Create(context.Context, *service.UsageLog) (bool, error) { return true, nil }

type parityHTTP struct {
	service.HTTPUpstream
	base  *httptest.Server
	oauth bool
}

func (p *parityHTTP) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if p.oauth && r.URL.Host == "chatgpt.com" {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host = "http", p.base.Listener.Addr().String()
	}
	if r.URL.Scheme+"://"+r.URL.Host != p.base.URL {
		return nil, fmt.Errorf("unexpected non-fixture upstream: %s", r.URL.Host)
	}
	return p.base.Client().Transport.RoundTrip(r)
}

type parityScript struct {
	ws, passthrough, queue bool
	mode                   string
	handshakeError         string // Reject the upgrade with HTTP 400 before response.create.
	prefix, tail           []string
	// When set, the upstream cannot send the tail until the real client reads a frame.
	gate <-chan struct{}
	// Optional extensions for the real OAuth notification / multi-attempt wire tests.
	configure func(*service.OpenAIGatewayService, *httptest.Server)
	attempt   func(int32) parityScript
	http400   string
}

type parityGateway struct {
	server             *httptest.Server
	repo               *parityAccounts
	slots              *paritySlots
	http, ws, upgrades atomic.Int32
	request            chan []byte
}

func newParityGateway(t *testing.T, script parityScript) *parityGateway {
	t.Helper()
	f := &parityGateway{request: make(chan []byte, 8), slots: &paritySlots{}, repo: &parityAccounts{}}
	f.slots.held.Store(script.queue)
	stop := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := script
		var write func(string) error
		var payload []byte
		var err error
		if websocket.IsWebSocketUpgrade(r) {
			f.upgrades.Add(1)
			if script.handshakeError != "" {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, script.handshakeError)
				return
			}
			u := websocket.Upgrader{}
			ws, e := u.Upgrade(w, r, http.Header{"X-Request-Id": {"upstream-private"}})
			if e != nil {
				return
			}
			defer ws.Close()
			_, payload, err = ws.ReadMessage()
			if err == nil {
				n := f.ws.Add(1)
				if script.attempt != nil {
					turn = script.attempt(n)
				}
			}
			write = func(s string) error { return ws.WriteMessage(websocket.TextMessage, []byte(s)) }
		} else {
			payload, err = io.ReadAll(r.Body)
			n := f.http.Add(1)
			if script.attempt != nil {
				turn = script.attempt(n)
			}
			if turn.http400 != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, turn.http400)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Request-Id", "upstream-private")
			write = func(s string) error {
				_, e := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(s, "type").Str, s)
				w.(http.Flusher).Flush()
				return e
			}
		}
		if err != nil {
			return
		}
		select {
		case f.request <- payload:
		case <-stop:
			return
		}
		for _, ev := range turn.prefix {
			if write(ev) != nil {
				return
			}
		}
		if turn.gate != nil {
			select {
			case <-turn.gate:
			case <-r.Context().Done():
				return
			case <-stop:
				return
			}
		}
		for _, ev := range turn.tail {
			if write(ev) != nil {
				return
			}
		}
	}))
	t.Cleanup(up.Close)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Gateway.MaxBodySize = 1 << 20
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Concurrency.PingInterval = 1
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 6
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 4
	mode := script.mode
	if mode == "" {
		mode = "ctx_pool"
	}
	f.repo.a = service.Account{ID: 7301, Name: "parity", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "test-only", "base_url": up.URL}, Extra: map[string]any{"openai_responses_mode": "force_responses", "openai_apikey_responses_websockets_v2_mode": mode, "openai_passthrough": script.passthrough, "same_account_retry_count": 0}}
	bill := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(bill.Stop)
	gw := service.NewOpenAIGatewayService(f.repo, &parityUsage{}, nil, nil, nil, nil, nil, cfg, nil, nil, service.NewBillingService(cfg, nil), service.NewRateLimitService(f.repo, nil, cfg, nil, nil), bill, &parityHTTP{base: up, oauth: script.configure != nil}, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil)
	if script.configure != nil {
		cfg.Gateway.OpenAIWS.OAuthEnabled = true
		f.repo.a.Type = service.AccountTypeOAuth
		f.repo.a.Extra["same_account_retry_count"] = 1
		f.repo.a.Credentials = map[string]any{"access_token": "loopback-only", "chatgpt_account_id": "loopback"}
		script.configure(gw, up)
	}
	t.Cleanup(gw.CloseOpenAIWSPool)
	keys := service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg)
	h := handler.NewOpenAIGatewayHandler(gw, service.NewConcurrencyService(f.slots), bill, keys, nil, nil, nil, nil, cfg)
	g := &service.Group{ID: 7302, Platform: service.PlatformOpenAI, Status: service.StatusActive, Hydrated: true, AllowMessagesDispatch: true, RateMultiplier: 1}
	// The upstream tag has no SmartWSMode field; the ordinary HTTP control cases run there unchanged.
	if script.ws {
		require.NoError(t, json.Unmarshal([]byte(`{"SmartWSMode":"enabled"}`), g))
	}
	key := &service.APIKey{ID: 7303, GroupID: &g.ID, Group: g, User: &service.User{ID: 7304, Status: service.StatusActive}}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	auth := func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), key)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID, Concurrency: 1})
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, g))
		c.Next()
	}
	routes.RegisterGatewayRoutes(router, &handler.Handlers{OpenAIGateway: h}, auth, keys, nil, nil, nil, nil, cfg)
	f.server = httptest.NewServer(router)
	t.Cleanup(f.server.Close)
	t.Cleanup(func() { close(stop); f.slots.held.Store(false) })
	return f
}

func (f *parityGateway) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	t.Cleanup(cancel)
	r, err := http.NewRequestWithContext(ctx, "POST", f.server.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream")
	resp, err := f.server.Client().Do(r)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func parityRest(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
}

func parityUntil(t *testing.T, r *bufio.Reader, marker string) string {
	t.Helper()
	var prefix strings.Builder
	for {
		line, err := r.ReadString('\n')
		prefix.WriteString(line)
		require.NoError(t, err, "before %q: %s", marker, prefix.String())
		if line == "\n" && strings.Contains(prefix.String(), marker) {
			return prefix.String()
		}
	}
}

// A visibly nonempty body is insufficient: a generic SSE decoder ignores bare JSON.
func parityErrorEvents(t *testing.T, wire string) []gjson.Result {
	t.Helper()
	var out []gjson.Result
	for _, line := range strings.Split(wire, "\n") {
		require.False(t, strings.HasPrefix(line, "{"), "bare JSON after an SSE commit: %q", wire)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		require.True(t, gjson.Valid(data), "invalid SSE data: %q", line)
		ev := gjson.Parse(data)
		if ev.Get("type").Str == "response.failed" || ev.Get("type").Str == "error" || ev.Get("error").IsObject() {
			out = append(out, ev)
		}
	}
	return out
}

func (f *parityGateway) checkTransport(t *testing.T, ws bool) []byte {
	t.Helper()
	if ws {
		require.EqualValues(t, 1, f.ws.Load())
		require.Zero(t, f.http.Load())
	} else {
		require.EqualValues(t, 1, f.http.Load())
		require.Zero(t, f.ws.Load())
	}
	select {
	case req := <-f.request:
		return req
	default:
		t.Fatal("no captured upstream request")
		return nil
	}
}

func (f *parityGateway) afterQueuePing(t *testing.T, path, body string) string {
	t.Helper()
	resp := f.post(t, path, body)
	r := bufio.NewReader(resp.Body)
	require.Equal(t, ":\n\n", parityUntil(t, r, ":\n"))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	require.Zero(t, f.upgrades.Load()+f.http.Load(), "client received the queue ping before dispatch")
	f.slots.held.Store(false)
	wire := parityRest(t, r)
	t.Logf("committed HTTP %d; after queue ping: %q", resp.StatusCode, wire)
	return wire
}

func TestV022ParityFirstOutputKeepalive(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, pass := range []bool{false, true} {
			t.Run(fmt.Sprintf("ws=%t/pass=%t", ws, pass), func(t *testing.T) {
				gate := make(chan struct{})
				release := sync.OnceFunc(func() { close(gate) })
				defer release()
				f := newParityGateway(t, parityScript{ws: ws, passthrough: pass, gate: gate, prefix: []string{parityCreated, parityBeat}, tail: []string{parityDelta, parityDone}})
				resp := f.post(t, "/v1/responses", parityResponses)
				r := bufio.NewReader(resp.Body)
				before := parityUntil(t, r, `"type":"keepalive"`)
				require.Equal(t, http.StatusOK, resp.StatusCode)
				require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
				require.Empty(t, resp.Header.Get("X-Request-Id"))
				require.Equal(t, "event: keepalive\ndata: "+parityBeat+"\n\n", before, "created, headers and content must still be staged")
				release()
				after := parityRest(t, r)
				require.NotContains(t, after, `"type":"keepalive"`)
				require.Contains(t, after, `"type":"response.completed"`)
				require.Contains(t, after, "visible-parity")
				f.checkTransport(t, ws)
			})
		}
	}
}

func TestV022ParityErrorAfterQueuePing(t *testing.T) {
	for _, ws := range []bool{false, true} {
		for _, rt := range []struct {
			name, path, body string
			pass             bool
		}{
			{"responses", "/v1/responses", parityResponses, false}, {"pass", "/v1/responses", parityResponses, true}, {"chat", "/v1/chat/completions", parityChat, false},
		} {
			t.Run(fmt.Sprintf("ws=%t/%s", ws, rt.name), func(t *testing.T) {
				f := newParityGateway(t, parityScript{ws: ws, passthrough: rt.pass, queue: true, tail: []string{parityReject}})
				wire := f.afterQueuePing(t, rt.path, rt.body)
				f.checkTransport(t, ws)
				ev := parityErrorEvents(t, wire)
				require.Len(t, ev, 1)
				require.Contains(t, ev[0].Raw, "parity rejected field")
				if ws {
					err := ev[0].Get("error")
					if rt.name == "chat" {
						require.True(t, strings.HasPrefix(wire, "data: "))
						require.False(t, ev[0].Get("type").Exists(), "no Responses envelope inside Chat SSE")
					} else {
						require.True(t, strings.HasPrefix(wire, "event: response.failed\ndata: "))
						require.Equal(t, "response.failed", ev[0].Get("type").Str)
						require.Equal(t, "failed", ev[0].Get("response.status").Str)
						require.Equal(t, gjson.Number, ev[0].Get("sequence_number").Type)
						require.Positive(t, ev[0].Get("response.created_at").Int())
						err = ev[0].Get("response.error")
					}
					require.Equal(t, "invalid_value", err.Get("code").Str)
					require.Equal(t, "invalid_request_error", err.Get("type").Str)
					require.Equal(t, "parity rejected field", err.Get("message").Str)
				}
			})
		}
	}
}

func TestV022ParityImmediateErrorWithoutPing(t *testing.T) {
	for _, rt := range []struct {
		name, path, body string
		pass             bool
	}{
		{"responses", "/v1/responses", parityResponses, false}, {"pass", "/v1/responses", parityResponses, true}, {"chat", "/v1/chat/completions", parityChat, false},
	} {
		t.Run(rt.name, func(t *testing.T) {
			f := newParityGateway(t, parityScript{ws: true, passthrough: rt.pass, tail: []string{parityReject}})
			resp := f.post(t, rt.path, rt.body)
			wire := parityRest(t, resp.Body)
			f.checkTransport(t, true)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, wire)
			require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
			require.JSONEq(t, parityReject, wire)
		})
	}
}

func TestV022ParityCommittedErrorBodies(t *testing.T) {
	t.Run("real_failed_with_usage", func(t *testing.T) {
		f := newParityGateway(t, parityScript{ws: true, queue: true, tail: []string{parityFailed400}})
		wire := f.afterQueuePing(t, "/v1/responses", parityResponses)
		f.checkTransport(t, true)
		ev := parityErrorEvents(t, wire)
		require.Len(t, ev, 1)
		require.JSONEq(t, parityFailed400, ev[0].Raw)
	})
	for _, chat := range []bool{false, true} {
		t.Run(fmt.Sprintf("plain_handshake/chat=%t", chat), func(t *testing.T) {
			const message = "parity handshake\ndata: injected"
			f := newParityGateway(t, parityScript{ws: true, queue: true, handshakeError: message})
			path, body, errPath := "/v1/responses", parityResponses, "response.error"
			if chat {
				path, body, errPath = "/v1/chat/completions", parityChat, "error"
			}
			wire := f.afterQueuePing(t, path, body)
			require.EqualValues(t, 1, f.upgrades.Load())
			require.Zero(t, f.ws.Load()+f.http.Load(), "handshake rejection never dispatched a model request")
			ev := parityErrorEvents(t, wire)
			require.Len(t, ev, 1)
			require.Equal(t, "upstream_error", ev[0].Get(errPath+".code").Str)
			require.Contains(t, ev[0].Get(errPath+".message").Str, "parity handshake")
		})
	}
}

func TestV022ParityCleanMetadataAfterError(t *testing.T) {
	for _, pass := range []bool{false, true} {
		t.Run(fmt.Sprintf("pass=%t", pass), func(t *testing.T) {
			gate := make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			defer release()
			f := newParityGateway(t, parityScript{ws: true, passthrough: pass, gate: gate, prefix: []string{parityCreated, parityDelta}, tail: []string{parityAuth, parityBeat}})
			resp := f.post(t, "/v1/responses", parityResponses)
			r := bufio.NewReader(resp.Body)
			parityUntil(t, r, "visible-parity")
			release()
			wire := parityRest(t, r)
			f.checkTransport(t, true)
			t.Logf("after 401 + clean metadata: auth=%d quota=%d wire=%q", f.repo.auth.Load(), f.repo.quota.Load(), wire)
			require.EqualValues(t, 1, f.repo.auth.Load())
			require.Zero(t, f.repo.quota.Load())
			ev := parityErrorEvents(t, wire)
			require.Len(t, ev, 1)
			require.Contains(t, ev[0].Raw, "invalid_api_key")
			require.NotContains(t, wire, `"type":"keepalive"`)
		})
	}
}

func TestV022ParityPermissionAfterQueuePing(t *testing.T) {
	const message = "The requested Cyber access program is not authorized for this model."
	const denied = `{"type":"error","status":403,"error":{"code":"permission_denied","type":"permission_error","message":"` + message + `"}}`
	for _, ws := range []bool{false, true} {
		for _, rt := range []struct{ name, path, body string }{{"responses", "/v1/responses", parityResponses}, {"chat", "/v1/chat/completions", parityChat}} {
			t.Run(fmt.Sprintf("ws=%t/%s", ws, rt.name), func(t *testing.T) {
				f := newParityGateway(t, parityScript{ws: ws, queue: true, tail: []string{denied}})
				wire := f.afterQueuePing(t, rt.path, rt.body)
				f.checkTransport(t, ws)
				require.Zero(t, f.repo.auth.Load()+f.repo.quota.Load(), "model entitlement is not a bad credential")
				ev := parityErrorEvents(t, wire)
				require.Len(t, ev, 1)
				require.Contains(t, ev[0].Raw, message)
				if ws {
					errPath := "response.error"
					if rt.name == "chat" {
						errPath = "error"
					}
					require.Equal(t, "permission_denied", ev[0].Get(errPath+".code").Str)
				}
			})
		}
	}
	t.Run("uncommitted", func(t *testing.T) {
		f := newParityGateway(t, parityScript{ws: true, tail: []string{denied}})
		resp := f.post(t, "/v1/responses", parityResponses)
		wire := parityRest(t, resp.Body)
		f.checkTransport(t, true)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
		require.JSONEq(t, `{"error":{"type":"permission_error","message":"`+message+`"}}`, wire)
		require.Zero(t, f.repo.auth.Load()+f.repo.quota.Load())
	})
}

func TestV022ParityPostOutputAccountEffects(t *testing.T) {
	for _, rt := range []struct {
		name, path, body string
		ws, wantWS, pass bool
	}{
		{"http_chat", "/v1/chat/completions", parityChat, false, false, false}, {"ws_chat", "/v1/chat/completions", parityChat, true, true, false},
		{"http_messages", "/v1/messages", parityMessages, false, false, false}, {"enabled_messages", "/v1/messages", parityMessages, true, false, false},
		{"http_responses", "/v1/responses", parityResponses, false, false, false}, {"http_passthrough", "/v1/responses", parityResponses, false, false, true},
		{"ws_responses", "/v1/responses", parityResponses, true, true, false}, {"ws_passthrough", "/v1/responses", parityResponses, true, true, true},
	} {
		for _, tc := range []struct {
			name, frame string
			auth, quota int32
		}{{"auth", parityAuth, 1, 0}, {"auth_failed", parityFailedAuth, 1, 0}, {"quota", parityQuota, 0, 1}, {"request_policy", parityPolicy, 0, 0}} {
			t.Run(rt.name+"/"+tc.name, func(t *testing.T) {
				gate := make(chan struct{})
				release := sync.OnceFunc(func() { close(gate) })
				defer release()
				f := newParityGateway(t, parityScript{ws: rt.ws, passthrough: rt.pass, gate: gate, prefix: []string{parityCreated, parityDelta}, tail: []string{tc.frame}})
				resp := f.post(t, rt.path, rt.body)
				r := bufio.NewReader(resp.Body)
				parityUntil(t, r, "visible-parity")
				release()
				wire := parityRest(t, r)
				f.checkTransport(t, rt.wantWS)
				t.Logf("HTTP %d; auth=%d quota=%d; terminal %q", resp.StatusCode, f.repo.auth.Load(), f.repo.quota.Load(), wire)
				require.Equal(t, 200, resp.StatusCode)
				assert.Equal(t, tc.auth, f.repo.auth.Load(), "auth writes")
				assert.Equal(t, tc.quota, f.repo.quota.Load(), "quota writes")
				require.Len(t, parityErrorEvents(t, wire), 1)
			})
		}
	}
}

func TestV022ParitySchemaAndContextOnWire(t *testing.T) {
	const schema = `{"type":"object","properties":{"s":{"type":"string","pattern":"(?<js>x)","default":"(?<js>x)"},"v":{"type":"string","pattern":"^[a-z]+$"}},"patternProperties":{"^\\p{L}+$":{"type":"string"}},"additionalProperties":false}`
	const external = `{"type":"function_call_output","namespace":"agent","name":"browser_info","output":"parity external"}`
	for _, ws := range []bool{false, true} {
		for _, rt := range []struct {
			name, path, body string
			pass             bool
		}{
			{"responses", "/v1/responses", `{"model":"gpt-5.1","stream":true,"input":[` + external + `,{"role":"user","content":"do not rewrite (?<js>x)"}],"tools":[{"type":"function","name":"probe","parameters":` + schema + `}]}`, false},
			{"passthrough", "/v1/responses", `{"model":"gpt-5.1","stream":true,"input":[` + external + `],"tools":[{"type":"function","name":"probe","parameters":` + schema + `}]}`, true},
			{"messages", "/v1/messages", `{"model":"gpt-5.1","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"probe","input_schema":` + schema + `}]}`, false},
			{"chat", "/v1/chat/completions", `{"model":"gpt-5.1","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"probe","parameters":` + schema + `}}]}`, false},
		} {
			t.Run(fmt.Sprintf("ws=%t/%s", ws, rt.name), func(t *testing.T) {
				f := newParityGateway(t, parityScript{ws: ws, passthrough: rt.pass, tail: []string{parityCreated, parityDelta, parityDone}})
				resp := f.post(t, rt.path, rt.body)
				body := parityRest(t, resp.Body)
				require.Equal(t, 200, resp.StatusCode, body)
				require.Contains(t, body, "visible-parity")
				req := f.checkTransport(t, ws && rt.name != "messages")
				tool := gjson.GetBytes(req, `tools.#(name=="probe").parameters`)
				require.True(t, tool.IsObject(), string(req))
				require.Equal(t, "^[a-z]+$", tool.Get("properties.v.pattern").Str)
				require.Equal(t, "(?<js>x)", tool.Get("properties.s.default").Str)
				if rt.name == "chat" {
					t.Logf("Chat upstream regular expression retained: %t", tool.Get("properties.s.pattern").Exists())
					return
				} // Compare with the unmodified official HTTP route, not a new Chat policy.
				require.False(t, tool.Get("properties.s.pattern").Exists())
				require.Empty(t, tool.Get("patternProperties").Map())
				require.NotEqual(t, "false", tool.Get("additionalProperties").Raw)
				if rt.path == "/v1/responses" {
					item := gjson.GetBytes(req, "input.0")
					require.Equal(t, "user", item.Get("role").Str)
					require.Equal(t, "<external_browser_info>parity external</external_browser_info>", item.Get("content.0.text").Str)
					require.False(t, item.Get("call_id").Exists())
					if !rt.pass {
						require.Equal(t, "do not rewrite (?<js>x)", gjson.GetBytes(req, "input.1.content").Str)
					}
				}
			})
		}
	}
}

func TestV022ParityUpstreamKeepaliveThenError(t *testing.T) {
	for _, pass := range []bool{false, true} {
		t.Run(fmt.Sprintf("pass=%t", pass), func(t *testing.T) {
			gate := make(chan struct{})
			release := sync.OnceFunc(func() { close(gate) })
			defer release()
			f := newParityGateway(t, parityScript{ws: true, passthrough: pass, prefix: []string{parityBeat}, gate: gate, tail: []string{parityReject}})
			resp := f.post(t, "/v1/responses", parityResponses)
			r := bufio.NewReader(resp.Body)
			before := parityUntil(t, r, `"type":"keepalive"`)
			require.Equal(t, "event: keepalive\ndata: "+parityBeat+"\n\n", before)
			release()
			wire := parityRest(t, r)
			f.checkTransport(t, true)
			t.Logf("after upstream keepalive: %q", wire)
			errs := parityErrorEvents(t, wire)
			require.Len(t, errs, 1)
			require.Contains(t, errs[0].Raw, "parity rejected field")
		})
	}
}

// GET /v1/responses is not the HTTP Codex context adapter. Inspect the actual
// response.create frame or bridged POST, without demanding HTTP-only rewriting.
func TestV022ParityNativeSchemaOnWire(t *testing.T) {
	const body = `{"type":"response.create","model":"gpt-5.1","input":[{"type":"function_call_output","namespace":"agent","name":"browser_info","output":"parity external"},{"role":"user","content":"native"}],"tools":[{"type":"function","name":"probe","parameters":{"type":"object","properties":{"s":{"type":"string","pattern":"(?<js>x)","default":"(?<js>x)"}}}}]}`
	for _, mode := range []string{"ctx_pool", "passthrough", "http_bridge"} {
		t.Run(mode, func(t *testing.T) {
			f := newParityGateway(t, parityScript{mode: mode, tail: []string{parityCreated, parityDelta, parityDone}})
			ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
			defer cancel()
			ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			defer ws.Close()
			deadline, _ := ctx.Deadline()
			require.NoError(t, ws.SetReadDeadline(deadline))
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
			req := f.checkTransport(t, mode != "http_bridge")
			p := gjson.GetBytes(req, `tools.#(name=="probe").parameters.properties.s`)
			require.Equal(t, "(?<js>x)", p.Get("default").Str)
			require.False(t, p.Get("pattern").Exists())
			t.Logf("upstream context item: %s", gjson.GetBytes(req, "input.0").Raw)
		})
	}
}
