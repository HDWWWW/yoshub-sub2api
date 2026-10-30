//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type happySettingsRepo struct{ settingRepoStub }

func (r *happySettingsRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if r.values == nil {
		r.values = make(map[string]string)
	}
	r.values[key] = value
	return nil
}

func TestHappyFirstTokenSettingsPersistenceAndCache(t *testing.T) {
	repo := &happySettingsRepo{}
	s := &SettingService{settingRepo: repo}
	ctx := context.Background()
	require.Equal(t, DefaultHappyFirstTokenSettings(), s.happyFirstTokenSettings(ctx))
	want := HappyFirstTokenSettings{Enabled: true, MinDelayMS: 100, MaxDelayMS: 1000}
	require.NoError(t, s.UpdateHappyFirstTokenSettings(ctx, want))
	require.Equal(t, want, s.happyFirstTokenSettings(ctx), "save must invalidate the cached disabled value immediately")

	reloaded := &SettingService{settingRepo: repo}
	got, err := reloaded.GetHappyFirstTokenSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, want, got, "settings must survive a new service instance")
	for _, invalid := range []HappyFirstTokenSettings{
		{MinDelayMS: -1}, {MaxDelayMS: 60001}, {MinDelayMS: 1001, MaxDelayMS: 1000},
	} {
		require.Error(t, s.UpdateHappyFirstTokenSettings(ctx, invalid))
	}
	require.Equal(t, want, s.happyFirstTokenSettings(ctx))
	repo.err = errors.New("storage unavailable")
	require.Error(t, s.UpdateHappyFirstTokenSettings(ctx, DefaultHappyFirstTokenSettings()))
	require.Equal(t, want, s.happyFirstTokenSettings(ctx), "failed saves must not change the active setting")
	require.False(t, (&SettingService{settingRepo: repo}).happyFirstTokenSettings(ctx).Enabled)
}

type happySignalRecorder struct {
	*httptest.ResponseRecorder
	flushed chan time.Time
}

func (r *happySignalRecorder) Flush() {
	r.ResponseRecorder.Flush()
	if strings.Contains(r.Body.String(), `"type":"response.metadata"`) {
		select {
		case r.flushed <- time.Now():
		default:
		}
	}
}

func happyTestContext(t *testing.T) (*gin.Context, *happySignalRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := &happySignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan time.Time, 1)}
	c, _ := gin.CreateTestContext(r)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, r
}

func happyTestService(t *testing.T, delayMS int) *OpenAIGatewayService {
	t.Helper()
	raw, err := json.Marshal(HappyFirstTokenSettings{Enabled: true, MinDelayMS: delayMS, MaxDelayMS: delayMS})
	require.NoError(t, err)
	return &OpenAIGatewayService{
		cfg:            &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		settingService: &SettingService{settingRepo: &happySettingsRepo{settingRepoStub{values: map[string]string{SettingKeyHappyFirstToken: string(raw)}}}},
	}
}

func happyAccount() *Account {
	return &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
}

func happyRunStream(s *OpenAIGatewayService, c *gin.Context, resp *http.Response, passthrough bool, started time.Time) (*openaiStreamingResult, error) {
	if passthrough {
		r, err := s.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, happyAccount(), started, "test-model", "test-model")
		if r == nil {
			return nil, err
		}
		return &openaiStreamingResult{usage: r.usage, firstTokenMs: r.firstTokenMs}, err
	}
	return s.handleStreamingResponse(c.Request.Context(), resp, c, happyAccount(), started, "test-model", "test-model")
}

const happySuccessStream = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n"

// The fake upstream cannot produce a token until the metadata is flushed. This
// exercises the real forwarding loops without relying on goroutine scheduling.
func happyBodyAfterSignal(t *testing.T, r *happySignalRecorder, body string, outputDelay time.Duration) io.ReadCloser {
	t.Helper()
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	go func() {
		defer writer.Close()
		select {
		case <-r.flushed:
		case <-time.After(2 * time.Second):
			_ = writer.CloseWithError(errors.New("metadata was not flushed before model output"))
			return
		}
		time.Sleep(outputDelay)
		_, _ = io.WriteString(writer, body)
	}()
	return reader
}

func TestHappyFirstTokenPrecedesOutputWithoutChangingTTFT(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			c, rec := happyTestContext(t)
			originalWriter := c.Writer
			s := happyTestService(t, 0)
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: happyBodyAfterSignal(t, rec, happySuccessStream, 30*time.Millisecond)}
			result, err := happyRunStream(s, c, resp, passthrough, time.Now())
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(rec.Body.String(), openAIHappyFirstTokenEvent))
			require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.metadata"`))
			require.NotNil(t, result.firstTokenMs)
			require.GreaterOrEqual(t, *result.firstTokenMs, 30, "metadata must not become Sub2's TTFT")
			require.EqualValues(t, 5, result.usage.InputTokens)
			require.EqualValues(t, 2, result.usage.OutputTokens)
			require.Same(t, originalWriter, c.Writer, "restore the middleware writer before returning")
			require.NotContains(t, openAIHappyFirstTokenEvent, "rate_limits")
			require.NotContains(t, openAIHappyFirstTokenEvent, "account")
		})
	}
}

func TestHappyFirstTokenPreservesFailuresAndRetry(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			name, body string
			retry      bool
		}{
			{"rate_limit", `data: {"type":"response.failed","response":{"status":"failed","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Too many requests"}}}` + "\n\n", true},
			{"overload", `data: {"type":"error","error":{"code":"server_is_overloaded","message":"Server overloaded"}}` + "\n\n", true},
			{"eof", "", true},
			{"invalid_request", `data: {"type":"response.failed","response":{"status":"failed","error":{"type":"invalid_request_error","code":"invalid_request_error","message":"Invalid input"}}}` + "\n\n", false},
		} {
			name := "native/" + tc.name
			if passthrough {
				name = "passthrough/" + tc.name
			}
			t.Run(name, func(t *testing.T) {
				c, rec := happyTestContext(t)
				s := happyTestService(t, 0)
				resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: happyBodyAfterSignal(t, rec, tc.body, 0)}
				_, err := happyRunStream(s, c, resp, passthrough, time.Now())
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.Equal(t, tc.retry, errors.As(err, &failover))
				if !tc.retry {
					require.Contains(t, rec.Body.String(), `"type":"response.failed"`)
					require.Contains(t, rec.Body.String(), "invalid_request_error")
					return
				}
				require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c), "preamble must not prevent account failover")
				require.Equal(t, openAIHappyFirstTokenEvent, rec.Body.String(), "failed attempt output must remain staged")
				resp.Body = io.NopCloser(strings.NewReader(happySuccessStream))
				_, err = happyRunStream(s, c, resp, passthrough, time.Now())
				require.NoError(t, err)
				require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.metadata"`))
				require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.completed"`))
				require.NotContains(t, rec.Body.String(), `"type":"response.failed"`)
				require.Greater(t, OpenAICompactKeepaliveAdjustedWrittenSize(c), 0)
			})
		}
	}
}

func TestHappyFirstTokenNeverDelaysRealOutputOrDropsTurnState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay int
		state string
	}{
		{"output_first", 60000, ""}, {"turn_state", 0, "opaque-session-state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := happyTestContext(t)
			s := happyTestService(t, tc.delay)
			resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(happySuccessStream))}
			if tc.state != "" {
				resp.Header.Set(openAICodexTurnStateHeader, tc.state)
			}
			_, err := happyRunStream(s, c, resp, false, time.Now())
			require.NoError(t, err)
			require.NotContains(t, rec.Body.String(), "response.metadata")
			require.Contains(t, rec.Body.String(), "response.completed")
			require.Equal(t, tc.state, rec.Result().Header.Get(openAICodexTurnStateHeader))
		})
	}
}

func TestHappyFirstTokenRetryKeepsOriginalDeadline(t *testing.T) {
	c, rec := happyTestContext(t)
	s := happyTestService(t, 300)
	stop := s.startOpenAIResponseSignals(c.Request.Context(), c, happyAccount(), nil, time.Now(), 0)
	stop() // The first account fails before the notification is due.
	require.Empty(t, rec.Body.String())
	time.Sleep(350 * time.Millisecond) // Retry backoff outlasts the original delay.

	stop = s.startOpenAIResponseSignals(c.Request.Context(), c, happyAccount(), nil, time.Now(), 0)
	defer stop()
	select {
	case <-rec.flushed:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("account retry restarted the notification delay")
	}
	stop()
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.metadata"`))
}

func TestHappyFirstTokenTimingStopAndScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
	}{
		{"wait_remaining", 0}, {"upstream_connection_exceeded_delay", time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := happyTestContext(t)
			s := happyTestService(t, 40)
			started := time.Now().Add(-tc.elapsed)
			stop := s.startOpenAIResponseSignals(c.Request.Context(), c, happyAccount(), nil, started, 0)
			defer stop()
			select {
			case emitted := <-rec.flushed:
				require.GreaterOrEqual(t, emitted.Sub(started), 40*time.Millisecond)
			case <-time.After(time.Second):
				t.Fatal("early response timer did not fire")
			}
			stop()
			require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
		})
	}
	for _, mode := range []string{"disabled", "api_key", "compact", "cancel", "stop"} {
		t.Run(mode, func(t *testing.T) {
			c, rec := happyTestContext(t)
			s, account := happyTestService(t, 20), happyAccount()
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			switch mode {
			case "disabled":
				s.settingService = nil
			case "api_key":
				account.Type = AccountTypeAPIKey
			case "compact":
				c.Request.URL.Path = "/v1/responses/compact"
			}
			stop := s.startOpenAIResponseSignals(ctx, c, account, nil, time.Now(), 0)
			defer stop()
			if mode == "cancel" {
				cancel()
			}
			if mode == "stop" {
				stop()
			}
			time.Sleep(50 * time.Millisecond)
			stop()
			require.Empty(t, rec.Body.String())
		})
	}
}
