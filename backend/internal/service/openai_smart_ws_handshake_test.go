//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the public forwarding boundary: adapter-only tests cannot catch
// a later HTTP error handler poisoning the scheduler after a rejected upgrade.
func TestSmartWSOpaqueHandshake403DoesNotDisableSharedAccount(t *testing.T) {
	s, account, httpUpstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "forbidden by edge")
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
	require.False(t, stop.ShouldReportAccountScheduleFailure(), "a rejected WS upgrade must not lower shared account health")
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
			var stop *UpstreamFailoverError
			require.ErrorAs(t, err, &stop)
			require.Equal(t, test.accountFailure, stop.ShouldReportAccountScheduleFailure())
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

func TestSmartWSBlank403FallsBackAndBacksOff(t *testing.T) {
	s, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	const body = `{"model":"gpt-5.1","input":"hello"}`
	for i := 0; i < 2; i++ {
		upstream.responses = append(upstream.responses, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"http_result","status":"completed"}`))})
	}
	for i := 0; i < 2; i++ {
		c, _ := forcedWSContext(t, "/v1/responses", 1, "session")
		req := httptest.NewRequest("POST", "https://api.openai.com/v1/responses", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer fixture")
		resp, err := s.doOpenAIHTTPOverWS(c, req, "", account)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)
		_ = resp.Body.Close()
	}
	require.Equal(t, 2, upstream.callCount)
	require.Equal(t, []byte(body), upstream.bodies[0])
	stats := s.independentSmartWSStats()
	require.EqualValues(t, 1, stats["blank_403_fallback"])
	require.EqualValues(t, 1, stats["dial_backoff"])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := forcedWSContext(t, "/v1/responses", 1, "cancel")
	req := httptest.NewRequest("POST", "https://api.openai.com/v1/responses", strings.NewReader(body)).WithContext(ctx)
	_, err := s.doOpenAIHTTPOverWS(c, req, "", account)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 2, upstream.callCount)
}

func TestSmartWSOversizedHTTPHistoryPreservesEntireBody(t *testing.T) {
	s, account, upstream := forcedWSService(t, func(w http.ResponseWriter, r *http.Request) { t.Error("history must not dial WS") })
	s.cfg.Gateway.OpenAIWS.MaxRequestBytes = 1024
	body := `{"model":"gpt-5.1","input":"` + strings.Repeat("x", 2048) + `","previous_response_id":"resp_http"}`
	upstream.responses = []*http.Response{{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_next","status":"completed","usage":{"input_tokens":1}}`))}}
	c, _ := forcedWSContext(t, "/v1/responses", 1, "history")
	_, err := s.Forward(c.Request.Context(), c, account, []byte(body))
	require.NoError(t, err)
	require.Equal(t, 1, upstream.callCount)
	require.JSONEq(t, body, string(upstream.bodies[0]))
}
