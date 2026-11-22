//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type contextBoundBlockingReadCloser struct {
	data       []byte
	offset     int
	ctx        context.Context
	forceClose chan struct{}
	closeOnce  sync.Once
}

func newContextBoundBlockingReadCloser(data []byte) *contextBoundBlockingReadCloser {
	return &contextBoundBlockingReadCloser{
		data:       data,
		forceClose: make(chan struct{}),
	}
}

func (r *contextBoundBlockingReadCloser) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := copy(p, r.data[r.offset:])
		r.offset += n
		return n, nil
	}
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.forceClose:
		return 0, io.EOF
	}
}

func (r *contextBoundBlockingReadCloser) Close() error {
	select {
	case <-r.ctx.Done():
	case <-r.forceClose:
	}
	return nil
}

func (r *contextBoundBlockingReadCloser) forceUnblock() {
	r.closeOnce.Do(func() { close(r.forceClose) })
}

type contextBoundHTTPUpstream struct {
	body *contextBoundBlockingReadCloser
}

func (u *contextBoundHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.body.ctx = req.Context()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       u.body,
	}, nil
}

func (u *contextBoundHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestForwardAsChatCompletions_CancelsUpstreamBeforeClosingBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"model\":\"gpt-5.4\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":17,\"output_tokens\":8,\"total_tokens\":25}}}\n\n")
	stream := newContextBoundBlockingReadCloser(upstreamBody)
	t.Cleanup(stream.forceUnblock)

	cfg := rawChatCompletionsTestConfig()
	cfg.Gateway.StreamKeepaliveInterval = 1
	svc := &OpenAIGatewayService{
		cfg:          cfg,
		httpUpstream: &contextBoundHTTPUpstream{body: stream},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsChatCompletions(context.Background(), c, rawChatCompletionsTestAccount(), body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 17, got.result.Usage.InputTokens)
	case <-time.After(time.Second):
		t.Fatal("ForwardAsChatCompletions did not cancel upstream before closing the body")
	}
}

type cancellationNetworkUpstream struct {
	client *http.Client
	url    *url.URL
	calls  atomic.Int32
}

func (u *cancellationNetworkUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	request := req.Clone(req.Context())
	request.URL.Scheme, request.URL.Host = u.url.Scheme, u.url.Host
	request.Host = u.url.Host
	return u.client.Do(request)
}

func (u *cancellationNetworkUpstream) DoWithTLS(req *http.Request, proxy string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, accountID, concurrency)
}

func TestOpenAICancellationPropagatesThroughNewAPIStyleRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, route := range []string{"chat", "responses", "messages", "passthrough", "raw_chat"} {
		for _, h2 := range []bool{false, true} {
			for _, phase := range []string{"before_headers", "client_disconnect", "relay_timeout", "error_body", "write_failure"} {
				t.Run(fmt.Sprintf("%s/h2=%v/%s", route, h2, phase), func(t *testing.T) {
					upstreamStarted := make(chan struct{})
					upstreamCanceled := make(chan error, 1)
					upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if h2 && r.ProtoMajor != 2 {
							upstreamCanceled <- fmt.Errorf("expected HTTP/2, got %s", r.Proto)
							return
						}
						if phase != "before_headers" {
							w.Header().Set("Content-Type", "text/event-stream")
							payload := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cancel\",\"model\":\"gpt-5.4\",\"status\":\"in_progress\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"
							if route == "raw_chat" {
								payload = "data: {\"id\":\"chatcmpl_cancel\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"
							}
							if phase == "error_body" {
								w.WriteHeader(http.StatusServiceUnavailable)
								payload = `{"error":{"message":"`
							}
							_, _ = io.WriteString(w, payload)
							w.(http.Flusher).Flush()
						}
						close(upstreamStarted)
						<-r.Context().Done()
						upstreamCanceled <- r.Context().Err()
					}))
					upstream.EnableHTTP2 = h2
					upstream.StartTLS()
					t.Cleanup(upstream.Close)
					upstreamURL, err := url.Parse(upstream.URL)
					require.NoError(t, err)
					transport := &cancellationNetworkUpstream{client: upstream.Client(), url: upstreamURL}
					svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: transport}
					account := rawChatCompletionsTestAccount()
					if route != "raw_chat" {
						account.Type = AccountTypeOAuth
						account.Credentials = map[string]any{"access_token": "test", "chatgpt_account_id": "test"}
					}
					if route == "passthrough" {
						account.Extra = map[string]any{"openai_passthrough": true}
					}
					forwardDone := make(chan error, 1)
					sub2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, _ := io.ReadAll(r.Body)
						c, _ := gin.CreateTestContext(w)
						c.Request = r
						if phase == "write_failure" {
							c.Writer = &openAIRawStreamDisconnectedWriter{ResponseWriter: c.Writer}
						}
						var err error
						switch route {
						case "responses", "passthrough":
							_, err = svc.Forward(r.Context(), c, account, body)
						case "messages":
							_, err = svc.ForwardAsAnthropic(r.Context(), c, account, body, "", "")
						case "raw_chat":
							_, err = svc.forwardAsRawChatCompletions(r.Context(), c, account, body, "")
						default:
							_, err = svc.ForwardAsChatCompletions(r.Context(), c, account, body, "", "")
						}
						forwardDone <- err
					}))
					t.Cleanup(sub2.Close)
					closeRelayBody := make(chan struct{})
					relayHeaders := make(chan struct{})
					newapi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						// Mirrors NewAPI newRelayRequest and StreamScannerHandler cleanup.
						request, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, sub2.URL, r.Body)
						resp, err := http.DefaultClient.Do(request)
						if err != nil {
							return
						}
						defer resp.Body.Close()
						close(relayHeaders)
						stop := context.AfterFunc(r.Context(), func() { _ = resp.Body.Close() })
						defer stop()
						if phase == "relay_timeout" {
							<-closeRelayBody
							_ = resp.Body.Close()
							return
						}
						_, _ = io.Copy(w, resp.Body)
					}))
					t.Cleanup(newapi.Close)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					payload := `{"model":"gpt-5.4","messages":[{"role":"user","content":"test"}],"max_tokens":16,"stream":true}`
					if route == "responses" || route == "passthrough" {
						payload = `{"model":"gpt-5.4","input":[{"role":"user","content":"test"}],"instructions":"test","stream":true}`
					}
					request, err := http.NewRequestWithContext(ctx, http.MethodPost, newapi.URL, strings.NewReader(payload))
					require.NoError(t, err)
					clientDone := make(chan struct{})
					go func() {
						defer close(clientDone)
						resp, err := http.DefaultClient.Do(request)
						if err == nil {
							_, _ = io.Copy(io.Discard, resp.Body)
							_ = resp.Body.Close()
						}
					}()
					select {
					case <-upstreamStarted:
					case <-time.After(3 * time.Second):
						t.Fatal("upstream did not start")
					}
					if phase == "relay_timeout" {
						select {
						case <-relayHeaders:
						case <-time.After(3 * time.Second):
							t.Fatal("relay did not receive output")
						}
						close(closeRelayBody)
					} else if phase != "write_failure" {
						cancel()
					}
					select {
					case err := <-upstreamCanceled:
						require.ErrorIs(t, err, context.Canceled)
					case <-time.After(3 * time.Second):
						t.Fatal("upstream did not observe cancellation")
					}
					select {
					case err := <-forwardDone:
						require.ErrorIs(t, err, context.Canceled)
						var failover *UpstreamFailoverError
						require.NotErrorAs(t, err, &failover)
					case <-time.After(3 * time.Second):
						t.Fatal("Sub2 kept draining")
					}
					require.Equal(t, int32(1), transport.calls.Load(), "cancellation must not retry")
					select {
					case <-clientDone:
					case <-time.After(3 * time.Second):
						t.Fatal("relay did not stop")
					}
				})
			}
		}
	}
}

func TestOpenAIResponsesCancellationPreservesObservedUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%v", passthrough), func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Writer = &openAIRawStreamDisconnectedWriter{ResponseWriter: c.Writer}
			body := []byte(`{"model":"gpt-5.4","instructions":"test","input":[{"role":"user","content":"test"}],"stream":true}`)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
			stream := newContextBoundBlockingReadCloser([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_partial_usage\",\"usage\":{\"input_tokens\":17,\"output_tokens\":3}}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
			t.Cleanup(stream.forceUnblock)
			svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: &contextBoundHTTPUpstream{body: stream}}
			account := rawChatCompletionsTestAccount()
			account.Type = AccountTypeOAuth
			account.Credentials = map[string]any{"access_token": "test", "chatgpt_account_id": "test"}
			account.Extra = map[string]any{"openai_passthrough": passthrough}
			result, err := svc.Forward(c.Request.Context(), c, account, body)
			require.ErrorIs(t, err, context.Canceled)
			require.NotNil(t, result)
			require.True(t, result.ClientDisconnect)
			require.Equal(t, 17, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			require.NoError(t, c.Request.Context().Err(), "closing an attempt must not cancel its parent")
		})
	}
}

func TestOpenAIAccountCancellationDoesNotAffectSchedulerScore(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	svc := &OpenAIGatewayService{rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true")}
	account := &Account{ID: 101, Platform: PlatformOpenAI}
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.4", true, nil)
	require.NotNil(t, svc.openaiAccountStats)
	initialRate, _, _ := svc.openaiAccountStats.snapshot(account.ID)
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.4", false, nil, fmt.Errorf("partial response: %w", context.Canceled))
	canceledRate, _, _ := svc.openaiAccountStats.snapshot(account.ID)
	require.Equal(t, initialRate, canceledRate)
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.4", false, nil, fmt.Errorf("upstream timeout: %w", context.DeadlineExceeded))
	timeoutRate, _, _ := svc.openaiAccountStats.snapshot(account.ID)
	require.Greater(t, timeoutRate, canceledRate, "an actual upstream timeout still affects scheduling")
}

func TestOpenAIOutputFormatValidationDoesNotAffectSchedulerScore(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	svc := &OpenAIGatewayService{rateLimitService: newOpenAIAdvancedSchedulerRateLimitService("true")}
	account := &Account{ID: 102, Platform: PlatformOpenAI}
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.4", true, nil)
	require.NotNil(t, svc.openaiAccountStats)
	initialRate, _, _ := svc.openaiAccountStats.snapshot(account.ID)
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.4", false, nil,
		fmt.Errorf("invalid request: %w", outputFormatError("text.format.schema.required", "missing property")))
	currentRate, _, _ := svc.openaiAccountStats.snapshot(account.ID)
	require.Equal(t, initialRate, currentRate)
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-5.4", false, nil, context.DeadlineExceeded)
	timeoutRate, _, _ := svc.openaiAccountStats.snapshot(account.ID)
	require.Greater(t, timeoutRate, currentRate)
}

func TestOpenAIHTTP2CancellationOnlyClosesCanceledStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	firstCanceled := make(chan error, 1)
	secondStarted := make(chan struct{})
	finishSecond := make(chan struct{})
	type observedConnection struct {
		remote string
		proto  int
	}
	connections := make(chan observedConnection, 2)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections <- observedConnection{r.RemoteAddr, r.ProtoMajor}
		if r.URL.Path == "/cancel" {
			_, _ = io.WriteString(w, "partial")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			firstCanceled <- r.Context().Err()
			return
		}
		close(secondStarted)
		select {
		case <-finishSecond:
			_, _ = io.WriteString(w, "healthy")
		case <-r.Context().Done():
		}
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	require.NoError(t, err)
	svc := &OpenAIGatewayService{httpUpstream: &cancellationNetworkUpstream{client: upstream.Client(), url: upstreamURL}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/cancel", nil)
	require.NoError(t, err)
	response, err := svc.doOpenAIUpstream(request, "", rawChatCompletionsTestAccount())
	require.NoError(t, err)
	defer response.Body.Close()
	secondDone := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/healthy", nil)
		response, err := svc.doOpenAIUpstream(request, "", rawChatCompletionsTestAccount())
		if err == nil {
			defer response.Body.Close()
			var body []byte
			body, err = io.ReadAll(response.Body)
			if err == nil && string(body) != "healthy" {
				err = fmt.Errorf("unexpected healthy-stream body: %q", body)
			}
		}
		secondDone <- err
	}()
	select {
	case <-secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("second HTTP/2 stream did not start")
	}
	require.NoError(t, response.Body.Close())
	select {
	case err := <-firstCanceled:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("first HTTP/2 stream was not canceled")
	}
	close(finishSecond)
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("healthy stream did not complete")
	}
	first, second := <-connections, <-connections
	require.Equal(t, 2, first.proto)
	require.Equal(t, 2, second.proto)
	require.Equal(t, first.remote, second.remote, "both streams must share the same HTTP/2 connection")
	require.NoError(t, ctx.Err(), "closing one response must not cancel the caller or its other stream")
}
