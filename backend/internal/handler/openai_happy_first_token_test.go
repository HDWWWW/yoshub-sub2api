package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type happyHandlerRecorder struct {
	*httptest.ResponseRecorder
	ready chan struct{}
	once  sync.Once
}

func (r *happyHandlerRecorder) Flush() {
	r.ResponseRecorder.Flush()
	if strings.Contains(r.Body.String(), `"gateway_status":"upstream_connected"`) {
		r.once.Do(func() { close(r.ready) })
	}
}

type happyHandlerUpstream struct {
	service.HTTPUpstream
	ready    <-chan struct{}
	mu       sync.Mutex
	attempts []int64
	status   int
}

func (u *happyHandlerUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.attempts = append(u.attempts, accountID)
	u.mu.Unlock()
	if u.status != http.StatusOK {
		return &http.Response{StatusCode: u.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Too many requests"}}`))}, nil
	}
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		select {
		case <-u.ready:
		case <-time.After(2 * time.Second):
			return
		}
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"Too many requests\"}}}\n\n")
	}()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Turn-State": {"account-bound-state"}}, Body: reader}, nil
}

func (u *happyHandlerUpstream) DoWithTLS(r *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, proxy, id, concurrency)
}

// Exercise the handler's real account switching and final-error fallback after
// Sub2 has already sent a metadata event. Transport 200 must not hide the 429.
func TestHappyFirstTokenHandlerKeepsRateLimitAndAccountFailover(t *testing.T) {
	for _, tc := range []struct {
		status int
		chat   bool
	}{{200, false}, {429, false}, {200, true}, {429, true}} {
		status := tc.status
		name := "stream_error_after_metadata"
		if status != http.StatusOK {
			name = "http_error_before_metadata"
		}
		if tc.chat {
			name = "chat/" + name
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			cfg := &config.Config{RunMode: config.RunModeSimple}
			cfg.Default.RateMultiplier = 1
			cfg.Gateway.MaxAccountSwitches = 1
			accounts := []service.Account{}
			for _, id := range []int64{9901, 9902} {
				accounts = append(accounts, service.Account{ID: id, Name: "happy-first-response-test", Platform: service.PlatformOpenAI,
					Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: int(id),
					Credentials: map[string]any{"access_token": "local-test-token", "chatgpt_account_id": "local-test-account"}})
			}
			repo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
			settings := service.NewSettingService(&contentModerationHandlerSettingRepo{values: map[string]string{}}, cfg)
			require.NoError(t, settings.UpdateHappyFirstTokenSettings(context.Background(), service.HappyFirstTokenSettings{Enabled: true}))
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			rec := &happyHandlerRecorder{ResponseRecorder: httptest.NewRecorder(), ready: make(chan struct{})}
			if tc.chat {
				close(rec.ready) // Chat keeps HTTP error handling and emits no early signal.
			}
			upstream := &happyHandlerUpstream{ready: rec.ready, status: status}
			gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, nil, nil,
				service.NewBillingService(cfg, nil), nil, billing, upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, settings, nil)
			h := NewOpenAIGatewayHandler(gateway, service.NewConcurrencyService(nil), billing,
				service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, nil, cfg)
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`))
			c.Request.Header.Set("Content-Type", "application/json")
			groupID := int64(9991)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: 9981, GroupID: &groupID,
				User:  &service.User{ID: 9982, Status: service.StatusActive},
				Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive}})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9982})
			if tc.chat {
				c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hello"}],"stream":true}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.ChatCompletions(c)
			} else {
				h.Responses(c)
			}
			upstream.mu.Lock()
			attempts := append([]int64(nil), upstream.attempts...)
			upstream.mu.Unlock()
			// Existing OAuth 429 policy retries each account once before switching.
			require.Equal(t, []int64{9901, 9901, 9902, 9902}, attempts, "status=%d body=%s", rec.Code, rec.Body.String())
			if status == http.StatusOK && !tc.chat {
				require.Contains(t, rec.Body.String(), "rate_limit_exceeded")
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, 1, strings.Count(rec.Body.String(), `"gateway_status":"upstream_connected"`))
				if !tc.chat {
					require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
				}
				require.NotContains(t, rec.Body.String(), "response.completed")
			} else {
				require.Equal(t, http.StatusTooManyRequests, rec.Code)
				require.Contains(t, rec.Body.String(), "rate_limit_error")
				require.NotContains(t, rec.Body.String(), "gateway.status")
				require.NotContains(t, rec.Body.String(), "upstream_connected")
			}
		})
	}
}
