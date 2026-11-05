//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise the public forwarding boundary: adapter-only tests cannot catch
// a later HTTP error handler poisoning the scheduler after a rejected upgrade.
func TestSmartWSOpaqueHandshake403DoesNotDisableSharedAccount(t *testing.T) {
	s, account, httpUpstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	repo := &rateLimitAccountRepoStub{}
	blocker := &runtimeBlockRecorder{}
	s.rateLimitService = NewRateLimitService(repo, nil, s.cfg, nil, nil)
	s.rateLimitService.SetAccountRuntimeBlocker(blocker)
	counter := &openAI403CounterCacheStub{counts: []int64{1, 2, 3}}
	s.rateLimitService.SetOpenAI403CounterCache(counter)
	account.GroupIDs = []int64{2, 5, 13}
	c, rec := forcedWSContext(t, "/v1/responses", 1, "shared-account")
	_, err := s.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","stream":true,"input":"hello"}`))
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Zero(t, httpUpstream.callCount, "the rejected request must not be replayed")
	require.Zero(t, repo.tempCalls, "an upgrade rejection must not cool down every group sharing this account")
	require.Zero(t, repo.setErrorCalls)
	require.Empty(t, blocker.accounts)
	var stop *UpstreamFailoverError
	require.ErrorAs(t, err, &stop)
	require.Equal(t, NextAccountStop, stop.NextAccountAction)
}

func TestSmartWSHandshakeAccountFailureClassification(t *testing.T) {
	for _, test := range []struct {
		name           string
		status         int
		body           string
		accountFailure bool
	}{
		{"opaque403", 403, "edge forbidden", false},
		{"permission403", 403, `{"error":{"type":"permission_error","message":"forbidden"}}`, false},
		{"real401", 401, `{"error":{"code":"invalid_api_key","message":"invalid credential"}}`, true},
		{"revoked403", 403, `{"error":{"code":"account_deactivated","message":"account deactivated"}}`, true},
		{"temporary503", 503, "unavailable", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			})
			repo := &rateLimitAccountRepoStub{}
			s.rateLimitService = NewRateLimitService(repo, nil, s.cfg, nil, nil)
			c, rec := forcedWSContext(t, "/v1/responses", 1, "classification")
			_, err := s.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","input":"hello","stream":true}`))
			require.Error(t, err)
			require.Equal(t, test.status, rec.Code)
			require.Equal(t, test.body, rec.Body.String())
			require.Equal(t, "7", rec.Header().Get("Retry-After"))
			require.Zero(t, upstream.callCount)
			require.Equal(t, test.accountFailure, repo.tempCalls+repo.setErrorCalls > 0)
			snapshot := s.getOpenAIWSConnPool().SmartWSSnapshot()
			require.EqualValues(t, 1, snapshot.Counters["failed"])
			require.Zero(t, snapshot.Counters["succeeded"])
			require.Zero(t, snapshot.Counters["dispatch_uncertain"])
		})
	}
}

func TestSmartWSHTTPFallbackKeepsHTTPAccountPolicy(t *testing.T) {
	s, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
	const payload = `{"error":{"message":"HTTP credential forbidden"}}`
	upstream.responses = []*http.Response{{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}}
	repo := &rateLimitAccountRepoStub{}
	s.rateLimitService = NewRateLimitService(repo, nil, s.cfg, nil, nil)
	s.rateLimitService.SetOpenAI403CounterCache(&openAI403CounterCacheStub{counts: []int64{1}})
	c, rec := forcedWSContext(t, "/v1/responses", 1, "http-policy")
	_, err := s.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.1","input":"hello","stream":true}`))
	require.Error(t, err)
	require.Equal(t, 403, rec.Code)
	require.Equal(t, payload, rec.Body.String())
	require.Equal(t, 1, upstream.callCount)
	require.Equal(t, 1, repo.tempCalls, "only WS-specific 403 penalties should change")
}

func TestSmartWSRejectionOnlyBypassesNewRequests(t *testing.T) {
	s, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	upstream.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"http_result"}`))}}
	request := func(body, session string) *http.Response {
		t.Helper()
		c, _ := forcedWSContext(t, "/v1/responses", 1, session)
		req := httptest.NewRequest("POST", "https://api.openai.com/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fixture")
		req.Header.Set("session_id", session)
		resp, err := s.doOpenAIHTTPOverWS(c, req, "", account)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}
	require.Equal(t, 403, request(`{"model":"gpt-5.1","input":"hello"}`, "one").StatusCode)
	require.Zero(t, upstream.callCount)
	require.Equal(t, 409, request(`{"model":"gpt-5.1","input":[],"previous_response_id":"resp_ws"}`, "two").StatusCode)
	require.Zero(t, upstream.callCount, "a connection-dependent continuation must not be dispatched via HTTP")
	const fullBody = `{"model":"gpt-5.1","input":"new request","reasoning":{"effort":"high"},"service_tier":"priority","tools":[],"instructions":"keep"}`
	require.Equal(t, 200, request(fullBody, "three").StatusCode)
	require.Equal(t, 1, upstream.callCount)
	require.Equal(t, fullBody, string(upstream.bodies[0]))
	snapshot := s.getOpenAIWSConnPool().SmartWSSnapshot()
	require.EqualValues(t, 1, snapshot.Counters["handshakes"])
	require.EqualValues(t, 1, snapshot.Counters["handshake_403"])
	require.EqualValues(t, 1, snapshot.Counters["safe_http_fallback"])
	require.EqualValues(t, 1, snapshot.Counters["continuation_unavailable"])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := forcedWSContext(t, "/v1/responses", 1, "cancel")
	req := httptest.NewRequest("POST", "https://api.openai.com/v1/responses", strings.NewReader(fullBody)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer fixture")
	_, err := s.doOpenAIHTTPOverWS(c, req, "", account)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, upstream.callCount, "cancellation must prevent a cached HTTP fallback")
}

func TestSmartWSRejectionCacheExpiryBoundsAndIsolation(t *testing.T) {
	pool := newOpenAIWSConnPool(nil)
	defer pool.Close()
	now := time.Now()
	req := openAIWSAcquireRequest{Account: &Account{ID: 1, Type: AccountTypeOAuth}, WSURL: "wss://example.test/responses", ProxyURL: "http://proxy.test", Headers: http.Header{"Authorization": {"Bearer first"}, "Openai-Beta": {"responses_websockets=2026-02-06"}}}
	key := smartWSRejectionDigest(req)
	pool.rejections.put(key, now, 30*time.Second, 2)
	require.True(t, pool.rejections.active(key, now.Add(time.Second)))
	pool.rejections.put(key, now.Add(20*time.Second), 30*time.Second, 2)
	require.False(t, pool.rejections.active(key, now.Add(31*time.Second)), "repeat failures must not extend an existing pause")
	for _, mutate := range []func(*openAIWSAcquireRequest){
		func(r *openAIWSAcquireRequest) { r.Account = &Account{ID: 2, Type: AccountTypeOAuth} },
		func(r *openAIWSAcquireRequest) { r.Headers.Set("Authorization", "Bearer refreshed") },
		func(r *openAIWSAcquireRequest) { r.Headers.Set("OpenAI-Beta", "different_protocol") },
		func(r *openAIWSAcquireRequest) { r.Headers.Set("Chatgpt-Account-Id", "different_tenant") },
		func(r *openAIWSAcquireRequest) { r.WSURL = "wss://other.test/responses" },
		func(r *openAIWSAcquireRequest) { r.ProxyURL = "http://other-proxy.test" },
	} {
		other := req
		other.Headers = req.Headers.Clone()
		mutate(&other)
		require.NotEqual(t, key, smartWSRejectionDigest(other))
	}
	for i := 0; i < 20; i++ {
		var k [32]byte
		k[0] = byte(i)
		pool.rejections.put(k, now.Add(time.Duration(i)*time.Millisecond), time.Minute, 2)
	}
	require.Len(t, pool.rejections.entries, 2)
	// Neither authentication failures nor temporary load/network errors may
	// turn into silently successful HTTP retries through this cache.
	for _, status := range []int{401, 429, 503, 0} {
		pool.rememberSmartWSRejection(req, &openAIWSDialError{StatusCode: status})
		require.False(t, pool.rejections.active(key, now))
	}
}

func TestSmartWSOversizedContinuationAtEndCannotFallback(t *testing.T) {
	s, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { t.Error("oversized continuation must not dial") })
	s.cfg.Gateway.OpenAIWS.MaxRequestBytes = 1024
	body := `{"model":"gpt-5.1","input":"` + strings.Repeat("x", 2048) + `","previous_response_id":"resp_ws_local"}`
	c, rec := forcedWSContext(t, "/v1/responses", 1, "large-continuation")
	_, err := s.Forward(c.Request.Context(), c, account, []byte(body))
	require.Error(t, err)
	require.Equal(t, 409, rec.Code)
	require.Contains(t, rec.Body.String(), "smart_ws_continuation_unavailable")
	require.Zero(t, upstream.callCount)
}
