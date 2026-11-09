package smartws

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func testPool(t *testing.T) *Pool {
	p := NewPool()
	t.Cleanup(p.Close)
	return p
}

func TestStableAccountAdmissionAndProxyIsolation(t *testing.T) {
	u, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	h := http.Header{"Authorization": {"Bearer first"}, "Chatgpt-Account-Id": {"account-a"}}
	before := accountAdmissionKey(u, h)
	h.Set("Authorization", "Bearer refreshed")
	require.Equal(t, before, accountAdmissionKey(u, h))
	h.Set("Chatgpt-Account-Id", "account-b")
	require.NotEqual(t, before, accountAdmissionKey(u, h))
	require.NotEqual(t, digest(u.String(), "proxy-a", "scope", headersKey(h, false)), digest(u.String(), "proxy-b", "scope", headersKey(h, false)))
}

func TestFullHistoryReusesSocketAndCancelCloses(t *testing.T) {
	var dials, turns atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		dials.Add(1)
		for {
			_, _, err := ws.ReadMessage()
			if err != nil {
				return
			}
			turn := turns.Add(1)
			if turn == 3 {
				_, _, _ = ws.ReadMessage()
				return
			}
			_ = ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%d"}}`, turn)))
			_ = ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_%d","status":"completed","usage":{"output_tokens":1}}}`, turn)))
		}
	}))
	defer server.Close()
	p := testPool(t)
	invoke := func(ctx context.Context, prev string) *http.Response {
		body := `{"model":"test","stream":true,"input":"hello"}`
		if prev != "" {
			body = fmt.Sprintf(`{"model":"test","stream":true,"input":"next","previous_response_id":%q}`, prev)
		}
		req, _ := http.NewRequestWithContext(ctx, "POST", server.URL, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test")
		resp, handled, err := p.TryWithOptions(req, "", "scope", Options{AllowTarget: true})
		require.NoError(t, err)
		require.True(t, handled)
		return resp
	}
	for _, prev := range []string{"", ""} {
		resp := invoke(context.Background(), prev)
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Contains(t, string(data), "response.completed")
		require.NoError(t, resp.Body.Close())
	}
	require.EqualValues(t, 1, dials.Load())
	ctx, cancel := context.WithCancel(context.Background())
	resp := invoke(ctx, "")
	cancel()
	_, err := io.ReadAll(resp.Body)
	require.Error(t, err)
	_ = resp.Body.Close()
	require.Eventually(t, func() bool { return p.Stats()["active"] == 0 }, time.Second, time.Millisecond)
}

func TestMissingContinuationNeverDials(t *testing.T) {
	p := testPool(t)
	req, _ := http.NewRequest("POST", "http://127.0.0.1:1/responses", strings.NewReader(`{"stream":true,"previous_response_id":"missing","input":"next"}`))
	req.Header.Set("Authorization", "Bearer test")
	_, handled, err := p.TryWithOptions(req, "", "scope", Options{AllowTarget: true})
	require.False(t, handled)
	require.NoError(t, err)
	require.Zero(t, p.Stats()["connecting"])
}

func TestAdaptiveCapacityFairnessAndIdleVictim(t *testing.T) {
	p := testPool(t)
	now := time.Now()
	p.accountLocked("a", now)
	capacity, _ := p.capacityLocked(now)
	require.Equal(t, 256, capacity)
	p.accountLocked("b", now)
	capacity, _ = p.capacityLocked(now)
	require.Equal(t, 512, capacity)
	st := p.accounts["a"]
	st.waiters = []*capacityWaiter{{key: "busy"}, {key: "hot"}}
	require.False(t, p.reuseTurnLocked(st, nil, "hot"))
	require.True(t, p.reuseTurnLocked(st, nil, "other"))
	old := &connection{accountKey: "a", used: now.Add(-time.Minute), active: newLease()}
	free := &connection{accountKey: "b", used: now}
	p.connections[old] = struct{}{}
	p.connections[free] = struct{}{}
	require.Equal(t, free, p.idleVictimLocked("a", false))
	// Synthetic records have no actual sockets and must not enter Close.
	delete(p.connections, old)
	delete(p.connections, free)
}

func TestHandshakeBackoffSingleProbeAndEpoch(t *testing.T) {
	p := testPool(t)
	now := time.Now()
	one, _ := p.admitDialLocked("a", now)
	two, _ := p.admitDialLocked("a", now)
	require.NotNil(t, one)
	require.NotNil(t, two)
	p.finishDialLocked(two, "blank_403", now)
	p.finishDialLocked(one, "success", now)
	require.Equal(t, uint(1), p.dials["a"].failures)
	permit, reason := p.admitDialLocked("a", now.Add(time.Second))
	require.Nil(t, permit)
	require.Equal(t, "dial_backoff", reason)
	probe, _ := p.admitDialLocked("a", now.Add(6*time.Second))
	require.NotNil(t, probe)
	require.True(t, probe.probe)
	_, reason = p.admitDialLocked("a", now.Add(6*time.Second))
	require.Equal(t, "dial_half_open_busy", reason)
	p.finishDialLocked(probe, "success", now.Add(6*time.Second))
	require.Zero(t, p.dials["a"].failures)
}

func TestQueueBytesBoundAndCancellation(t *testing.T) {
	p := testPool(t)
	p.config.maxWaitingBytes = 4
	// A matching handshake provides a real reason to wait instead of cold fallback.
	p.openingKeys["key"] = 1
	p.dials["a"] = &dialState{inFlight: 2, used: time.Now()}
	l := newLease()
	l.requestBytes = 5
	c, permit, err := p.acquire(context.Background(), "key", "cap", "a", true, l)
	require.NoError(t, err)
	require.Nil(t, c)
	require.Nil(t, permit)
	require.EqualValues(t, 1, p.counters["wait_bytes_full"])
	require.Zero(t, p.waitingBytes)
	ctx, cancel := context.WithCancel(context.Background())
	l.requestBytes = 3
	done := make(chan error, 1)
	go func() { _, _, err := p.acquire(ctx, "key", "cap", "a", true, l); done <- err }()
	require.Eventually(t, func() bool { return p.Stats()["waiting"] == 1 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, 0, p.Stats()["waiting_bytes"])
}
