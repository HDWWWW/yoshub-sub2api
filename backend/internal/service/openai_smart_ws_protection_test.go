//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestSmartWS_PreDispatchFallbackPreservesEntireBody(t *testing.T) {
	for _, kind := range []string{"unsupported", "unsupported_404", "background", "large", "configured_large"} {
		t.Run(kind, func(t *testing.T) {
			var handshakes atomic.Int32
			s, a, httpTransport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				handshakes.Add(1)
				if kind == "unsupported_404" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusUpgradeRequired)
			})
			body := `{"model":"gpt-5.1","stream":true,"input":"hello","reasoning":{"effort":"high"},"service_tier":"priority"}`
			if kind == "background" {
				body = `{"model":"gpt-5.1","background":true,"input":"hello"}`
			}
			if kind == "large" {
				body = `{"model":"gpt-5.1","input":"` + strings.Repeat("x", int(openAIWSMessageReadLimitBytes)+16) + `"}`
			}
			if kind == "configured_large" {
				s.cfg.Gateway.OpenAIWS.MaxRequestBytes = 2048
				body = `{"model":"gpt-5.1","input":"` + strings.Repeat("x", 4096) + `"}`
			}
			httpTransport.responses = []*http.Response{{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"ok"}`))}}
			c, _ := forcedWSContext(t, "/v1/responses", 1, "fallback")
			req, err := http.NewRequestWithContext(c.Request.Context(), "POST", "http://example.test/v1/responses", strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer test")
			resp, err := s.doOpenAIHTTPOverWS(c, req, "", a)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, 1, httpTransport.callCount)
			if strings.HasPrefix(kind, "unsupported") {
				require.EqualValues(t, 1, handshakes.Load())
			} else {
				require.Zero(t, handshakes.Load())
			}
			require.Equal(t, body, string(httpTransport.bodies[0]))
			require.True(t, smartWSHasDispatched(c), "HTTP fallback is now dispatched and must not be replayed either")
			require.False(t, isOpenAIHTTPWSResponse(resp), "fallback must retain HTTP usage attribution")
			require.Equal(t, "Bearer test", httpTransport.reqs[0].Header.Get("Authorization"))
		})
	}
}

func TestSmartWS_GroupPolicyAndAdminPersistence(t *testing.T) {
	s, a, _ := forcedWSService(t, func(http.ResponseWriter, *http.Request) {})
	c, _ := forcedWSContext(t, "/v1/responses", 1, "group")
	g := &Group{ID: 7, Platform: PlatformOpenAI, Status: "active", Hydrated: true, SmartWSMode: "disabled"}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, g))
	require.Equal(t, openAIWSHTTPDecision("test").Transport, s.smartWSDecision(c, a).Transport)
	g.SmartWSMode = "enabled"
	a.Extra["unrelated_flag"] = false
	require.Equal(t, OpenAIUpstreamTransportResponsesWebsocketV2, s.smartWSDecision(c, a).Transport)
	require.Equal(t, false, a.Extra["unrelated_flag"])
	s.cfg.Gateway.OpenAIWS.ForceHTTP = true
	require.Equal(t, openAIWSHTTPDecision("test").Transport, s.smartWSDecision(c, a).Transport)
	for _, platform := range []string{PlatformOpenAI, PlatformComposite} {
		repo := &groupRepoStubForAdmin{}
		admin := &adminServiceImpl{groupRepo: repo}
		created, err := admin.CreateGroup(context.Background(), &CreateGroupInput{Name: "SmartWS", Platform: platform, RateMultiplier: 1, SmartWSMode: "enabled"})
		require.NoError(t, err)
		require.Equal(t, "enabled", created.SmartWSMode)
		repo.getByID = created
		mode := "disabled"
		updated, err := admin.UpdateGroup(context.Background(), 7, &UpdateGroupInput{SmartWSMode: &mode})
		require.NoError(t, err)
		require.Equal(t, mode, updated.SmartWSMode)
		mode = "invalid"
		_, err = admin.UpdateGroup(context.Background(), 7, &UpdateGroupInput{SmartWSMode: &mode})
		require.Error(t, err)
	}
}

func TestSmartWS_CapacityAndQueueAreBoundedAcrossSessions(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnsGlobal = 2
	cfg.Gateway.OpenAIWS.MaxConnsPerIdentity = 1
	cfg.Gateway.OpenAIWS.MaxQueuedGlobal = 2
	cfg.Gateway.OpenAIWS.MaxQueuedPerIdentity = 1
	p := newOpenAIWSConnPool(cfg)
	defer p.Close()
	req := openAIWSAcquireRequest{Account: &Account{ID: 1, Type: AccountTypeAPIKey}, WSURL: "wss://example.test/v1/responses", Headers: make(http.Header)}
	req.Headers.Set("Authorization", "Bearer identity")
	release, done, err := p.reserveSmartWSDial(context.Background(), req)
	require.NoError(t, err)
	done()
	req.Headers.Set("session-id", "different-session")
	_, _, err = p.reserveSmartWSDial(context.Background(), req)
	require.ErrorIs(t, err, errSmartWSCapacity)
	release()
	release()
	leave, err := p.enterSmartWSAcquire(req)
	require.NoError(t, err)
	req.Headers.Set("session-id", "third-session")
	_, err = p.enterSmartWSAcquire(req)
	require.ErrorIs(t, err, errSmartWSCapacity)
	leave()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = p.reserveSmartWSDial(ctx, req)
	require.Error(t, err)
	snapshot := p.SmartWSSnapshot()
	require.Zero(t, snapshot.Connections)
	require.Zero(t, snapshot.Handshaking)
	require.Zero(t, snapshot.Acquiring)
}

func TestSmartWS_IdleAndAgeCleanupNeverInterruptsLeasedStream(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConnAgeSeconds = 60
	cfg.Gateway.OpenAIWS.IdleTimeoutSeconds = 60
	p := newOpenAIWSConnPool(cfg)
	defer p.Close()
	conn := newOpenAIWSConn("long-stream", 1, newOpenAIWSReaderLoopFakeConn(), nil)
	conn.createdAtNano.Store(time.Now().Add(-20 * time.Minute).UnixNano())
	conn.lastUsedNano.Store(time.Now().Add(-20 * time.Minute).UnixNano())
	require.True(t, conn.tryAcquire())
	ap := p.getOrCreateAccountPool(1)
	ap.mu.Lock()
	ap.conns[conn.id] = conn
	require.Empty(t, p.cleanupAccountLocked(ap, time.Now(), 2))
	ap.mu.Unlock()
	require.False(t, conn.isClosed())
	conn.release()
	ap.mu.Lock()
	evicted := p.cleanupAccountLocked(ap, time.Now(), 2)
	ap.mu.Unlock()
	require.Len(t, evicted, 1)
	closeOpenAIWSConns(evicted)
}

func TestSmartWS_MetricsDistinguishOutputErrorsAndFinalDelivery(t *testing.T) {
	s, _, _ := forcedWSService(t, func(http.ResponseWriter, *http.Request) {})
	o := s.beginSmartWSObservation(nil)
	o.dispatched = true
	o.event([]byte(forcedWSCreated))
	require.EqualValues(t, -1, o.firstMS)
	o.event([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	require.GreaterOrEqual(t, o.firstMS, int64(0))
	o.event([]byte(forcedWSCompleted))
	o.finish(context.Background(), &OpenAIForwardResult{ClientDisconnect: true}, nil)
	o = s.beginSmartWSObservation(nil)
	o.dispatched = true
	o.event([]byte(`{"type":"response.failed"}`))
	o.finish(context.Background(), &OpenAIForwardResult{}, nil)
	snapshot := s.getOpenAIWSConnPool().SmartWSSnapshot()
	require.EqualValues(t, 0, snapshot.Counters["succeeded"])
	require.EqualValues(t, 1, snapshot.Counters["canceled"])
	require.EqualValues(t, 1, snapshot.Counters["failed"])
	require.EqualValues(t, 1, snapshot.Counters["upstream_terminal_error"])
}

func TestSmartWS_MetricsCompletedDeliverySurvivesLateClientClose(t *testing.T) {
	s, _, _ := forcedWSService(t, func(http.ResponseWriter, *http.Request) {})
	o := s.beginSmartWSObservation(nil)
	o.dispatched = true
	o.event([]byte(forcedWSCreated))
	o.event([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	o.event([]byte(forcedWSCompleted))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // The client has consumed the formatter's successful terminal frame.
	o.finish(ctx, &OpenAIForwardResult{}, nil)
	o.finish(ctx, &OpenAIForwardResult{}, nil)
	snapshot := s.getOpenAIWSConnPool().SmartWSSnapshot()
	require.EqualValues(t, 1, snapshot.Counters["succeeded"])
	require.Zero(t, snapshot.Counters["canceled"])
	require.Equal(t, 1, snapshot.Cold.Samples)
}

func TestSmartWS_NonJSONHandshakeErrorIsPreserved(t *testing.T) {
	const payload = "upstream temporarily unavailable"
	s, account, transport := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, payload)
	})
	c, recorder := forcedWSContext(t, "/v1/responses", 1, "non-json-error")
	_, err := s.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
	var failure *UpstreamFailoverError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, http.StatusServiceUnavailable, failure.StatusCode)
	require.Equal(t, payload, string(failure.ResponseBody))
	require.Equal(t, "7", failure.ResponseHeaders.Get("Retry-After"))
	require.True(t, failure.RetryableOnSameAccount)
	require.True(t, failure.PreserveUpstreamError)
	require.Empty(t, recorder.Body.String(), "the handler must exhaust the retry budget before writing the error")
	require.Zero(t, transport.callCount)
}

func TestSmartWS_NativeLifetimeEndsBeforeNextDispatch(t *testing.T) {
	s, _, _ := forcedWSService(t, func(http.ResponseWriter, *http.Request) {})
	s.cfg.Gateway.OpenAIWS.MaxConnAgeSeconds = 60
	connection := &openAIWSCaptureConn{}
	dispatch := &smartWSDispatch{}
	observer := &smartWSNativeObserver{FrameConn: connection, s: s, dispatch: dispatch}
	ctx := context.Background()
	payload := []byte(`{"type":"response.create","model":"gpt-5.1","input":"hello"}`)
	require.NoError(t, observer.WriteFrame(ctx, coderws.MessageText, payload))
	observer.openedAt = time.Now().Add(-61 * time.Second)
	observer.current.event([]byte(forcedWSCompleted))
	observer.delivered(ctx, []byte(forcedWSCompleted), nil)
	require.Len(t, connection.writes, 1, "lifetime expiry must not interrupt the active turn")
	err := observer.WriteFrame(ctx, coderws.MessageText, payload)
	var closeErr *OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Contains(t, err.Error(), "not dispatched")
	require.Len(t, connection.writes, 1, "the next turn must not be dispatched on the aged connection")
	require.EqualValues(t, 1, s.getOpenAIWSConnPool().SmartWSSnapshot().Counters["succeeded"])
}

func TestSmartWS_HandshakeRateWaitHonorsCancellation(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.MaxConcurrentHandshakes = 1
	cfg.Gateway.OpenAIWS.MaxConcurrentHandshakesPerIdentity = 1
	cfg.Gateway.OpenAIWS.HandshakesPerSecond = 1
	cfg.Gateway.OpenAIWS.HandshakesPerSecondPerIdentity = 1
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	req := openAIWSAcquireRequest{Account: &Account{ID: 1}, WSURL: "wss://example.test/responses", Headers: make(http.Header)}
	req.Headers.Set("Authorization", "Bearer bounded-rate")
	release, done, err := pool.reserveSmartWSDial(context.Background(), req)
	require.NoError(t, err)
	_, _, err = pool.reserveSmartWSDial(context.Background(), req)
	require.ErrorIs(t, err, errSmartWSCapacity, "handshake concurrency must be bounded")
	done()
	release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err = pool.reserveSmartWSDial(ctx, req)
	require.Error(t, err, "a second immediate dial cannot bypass the handshake rate")
	require.Less(t, time.Since(started), 300*time.Millisecond)
	snapshot := pool.SmartWSSnapshot()
	require.Zero(t, snapshot.Connections)
	require.Zero(t, snapshot.Handshaking)
}
