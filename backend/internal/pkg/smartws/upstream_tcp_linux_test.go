//go:build linux

package smartws

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

const (
	testProxyOrigin = "origin.invalid" // only the test proxy knows its loopback address
	tcpUserTimeout  = 0x12             // Linux TCP_USER_TIMEOUT, in milliseconds
)

// Probe a separate socket, not the WS socket being tested. This also lets us run
// the same real handshakes under a kernel policy that refuses just this option.
func wantTCPUserTimeout(t *testing.T) int {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
	require.NoError(t, err)
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, tcpUserTimeout, 30000); err != nil {
		unchanged, getErr := syscall.GetsockoptInt(fd, syscall.IPPROTO_TCP, tcpUserTimeout)
		require.NoError(t, getErr)
		t.Logf("TCP_USER_TIMEOUT refused (%v); expecting unchanged value %d", err, unchanged)
		return unchanged
	}
	return 30000
}

func TestNewPoolWSSFirstHopTCPOptions(t *testing.T) {
	wantTimeout := wantTCPUserTimeout(t)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close()
		var create struct{ Type string }
		if err := ws.ReadJSON(&create); err != nil || create.Type != "response.create" {
			t.Errorf("upstream got %q, err=%v", create.Type, err)
			return
		}
		for _, frame := range []string{
			`{"type":"response.created","response":{"id":"resp_socket"}}`,
			`{"type":"response.completed","response":{"id":"resp_socket","status":"completed","usage":{"output_tokens":1}}}`,
		} {
			if err := ws.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				t.Error(err)
				return
			}
		}
		_, _, _ = ws.ReadMessage() // keep the socket open until the client finishes inspecting it
	}))
	t.Cleanup(origin.Close)

	for _, route := range []string{"direct", "http", "socks5", "socks5h"} {
		t.Run(route, func(t *testing.T) {
			peer, target, proxyURL := origin.Listener.Addr().String(), origin.URL, ""
			if route != "direct" {
				peer = startWSTestTunnel(t, route, peer, 10*time.Second)
				target, proxyURL = "https://"+testProxyOrigin+":443", route+"://"+peer
			}
			p := testPool(t) // NewPool(), with its real NetDialContext and proxy selection
			require.Equal(t, 5*time.Second, p.config.handshake)
			require.Equal(t, 5*time.Second, p.config.dialer.HandshakeTimeout)
			tlsConfig := origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			tlsConfig.ServerName = origin.Listener.Addr().(*net.TCPAddr).IP.String()
			p.config.dialer.TLSClientConfig = tlsConfig

			var firstHop net.Conn
			// Gorilla may wrap SOCKS tunnels in a proxy connection. Inspect
			// the actual first-hop socket returned by our production dialer.
			dial := p.config.dialer.NetDialContext
			p.config.dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err == nil {
					firstHop = conn
				}
				return conn, err
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(`{"stream":true,"input":"test"}`))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer loopback-test")
			resp, handled, err := p.TryWithOptions(req, proxyURL, "", Options{AllowTarget: true, ReadTimeout: 5 * time.Second})
			require.NoError(t, err)
			require.True(t, handled)
			require.NotNil(t, resp)
			defer resp.Body.Close()
			require.Equal(t, "websocket", resp.Header.Get("X-Sub2-Upstream-Transport"))
			tcp, ok := firstHop.(*net.TCPConn)
			require.True(t, ok, "Gorilla first hop: %T", firstHop)
			require.Equal(t, peer, tcp.RemoteAddr().String())
			raw, err := tcp.SyscallConn()
			require.NoError(t, err)
			for _, opt := range []struct{ level, name, want int }{
				{syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1},
				{syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 15},
				{syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, 5},
				{syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, 3},
				{syscall.IPPROTO_TCP, tcpUserTimeout, wantTimeout},
			} {
				var got int
				var getErr error
				require.NoError(t, raw.Control(func(fd uintptr) { got, getErr = syscall.GetsockoptInt(int(fd), opt.level, opt.name) }))
				require.NoError(t, getErr)
				require.Equal(t, opt.want, got, "getsockopt(%d, %d) on %s", opt.level, opt.name, peer)
			}
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Contains(t, string(body), "event: response.completed")
			t.Logf("WSS peer %s: keepalive=1 idle=15s interval=5s count=3 user_timeout=%dms; response.completed", peer, wantTimeout)
		})
	}
}

// A single loopback tunnel, limited to one fixed origin. Rejecting any other
// authority (including a SOCKS IP address) also checks who resolves the name.
func startWSTestTunnel(t *testing.T, route, origin string, timeout time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan struct{})
	t.Cleanup(func() { _ = ln.Close(); <-done })
	go func() {
		defer close(done)
		client, err := ln.Accept()
		if err != nil {
			return // cleanup when the dialer failed to reach the proxy
		}
		defer client.Close()
		deadline := time.Now().Add(timeout)
		_ = client.SetDeadline(deadline)
		server, err := net.DialTimeout("tcp", origin, 5*time.Second)
		if err != nil {
			t.Error(err)
			return
		}
		defer server.Close()
		_ = server.SetDeadline(deadline)
		in := bufio.NewReader(client)
		if err := negotiateWSTestTunnel(route, in, client); err != nil {
			t.Error(err)
			return
		}
		go func() { _, _ = io.Copy(server, in); _ = server.(*net.TCPConn).CloseWrite() }()
		_, _ = io.Copy(client, server)
	}()
	return ln.Addr().String()
}

func negotiateWSTestTunnel(route string, in *bufio.Reader, out io.Writer) error {
	if route == "http" {
		req, err := http.ReadRequest(in)
		if err != nil {
			return err
		}
		defer req.Body.Close()
		if req.Method != http.MethodConnect || req.Host != testProxyOrigin+":443" {
			return fmt.Errorf("unexpected proxy request: %s %s", req.Method, req.Host)
		}
		_, err = io.WriteString(out, "HTTP/1.1 200 Connection Established\r\n\r\n")
		return err
	}
	// SOCKS5 no-auth greeting, then CONNECT with a domain name and port 443.
	connect := append([]byte{5, 1, 0, 3, byte(len(testProxyOrigin))}, testProxyOrigin...)
	for _, step := range []struct{ want, reply []byte }{
		{[]byte{5, 1, 0}, []byte{5, 0}},
		{binary.BigEndian.AppendUint16(connect, 443), []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}},
	} {
		got := make([]byte, len(step.want))
		if _, err := io.ReadFull(in, got); err != nil {
			return err
		}
		if !bytes.Equal(step.want, got) {
			return fmt.Errorf("unexpected SOCKS5 request %x, want %x", got, step.want)
		}
		if _, err := out.Write(step.reply); err != nil {
			return err
		}
	}
	return nil
}
