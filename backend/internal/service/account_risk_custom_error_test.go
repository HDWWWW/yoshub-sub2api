//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountRiskCustomErrorHTTPAndStreamShareOneBurst(t *testing.T) {
	risk, store, accounts, _, _ := riskHarness()
	c := &store.state.Rules[7].Config
	c.CustomErrorEnabled, c.CustomErrorThreshold = true, 2
	c.CustomErrorRules = []AccountRiskErrorMatch{{Status: 503, Keyword: "OVERload"}, {Keyword: "busy"}, {Status: 529}}
	risk.counter429 = &risk429TestCounter{}
	gw := &OpenAIGatewayService{accountRisk: risk}
	a := &accounts.pool[0]
	ctx := context.Background()
	wire := func(code int, content string) {
		gw.httpUpstream = &httpUpstreamSequenceRecorder{responses: []*http.Response{newJSONResponse(code, content)}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/v1/responses", nil)
		require.NoError(t, err)
		resp, err := gw.doOpenAIUpstream(req, "", a)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, content, string(body))
		require.NoError(t, resp.Body.Close())
	}
	wire(200, `{"message":"busy"}`)
	wire(503, `{"error":{"message":"maintenance"}}`)
	wire(502, `{"error":"overloaded"}`)
	wire(503, `{"error":{"message":"overloaded and busy"},"extra":"private details"}`)
	observer := gw.newAccountRiskModelObserver(ctx, nil, a, "demo")
	observer.ObserveOpenAI([]byte(`{"response":{"error":{"message":"other"},"output":"busy"}}`), "response.failed")
	observer.ObserveOpenAI([]byte(`{"error":{"message":"BUSY"}}`), "error")
	observer.ObserveOpenAI([]byte(`{"response":{"error":{"message":"busy"}}}`), "response.failed")
	require.Empty(t, store.stopped)
	wire(529, "upstream unavailable")
	require.Equal(t, []int64{a.ID}, store.stopped)
	event := store.state.Rules[7].Events[0]
	require.Equal(t, "custom_error", event.Kind)
	require.Equal(t, 3, event.Occurrences)
	require.NotContains(t, event.Reason, "private")
}

func TestAccountRiskCustomMatchingValidationAndDisabledDefault(t *testing.T) {
	c := DefaultAccountRiskConfig()
	require.False(t, c.CustomErrorEnabled)
	c.CustomErrorEnabled = true
	require.Error(t, c.Validate())
	for _, r := range []AccountRiskErrorMatch{{}, {Status: 200}, {Status: 700}, {Keyword: strings.Repeat("字", 129)}} {
		c.CustomErrorRules = []AccountRiskErrorMatch{r}
		require.Error(t, c.Validate())
	}
	c.CustomErrorRules = []AccountRiskErrorMatch{{Status: 503}, {Keyword: "overload"}}
	require.NoError(t, c.Validate())
}

func TestAccountRisk401LiveRetryWithRiskAndProbeDisabled(t *testing.T) {
	risk, store, accounts, _, now := riskHarness()
	r := store.state.Rules[7]
	r.Config.Enabled = false
	r.Config.ProbeEnabled = false
	r.Config.Probe401Enabled = false
	r.Config.Notify401Enabled = true
	n := &riskNoticeStub{}
	risk.notifier = n
	gw := &OpenAIGatewayService{accountRisk: risk, httpUpstream: &httpUpstreamSequenceRecorder{responses: []*http.Response{newJSONResponse(401, "secret-error"), newJSONResponse(401, "secret-error"), newJSONResponse(200, "ok")}}}
	ctx := WithHeaderOverrideRequestScope(context.Background(), 1, 1, &r.GroupID)
	for _, code := range []int{401, 401, 200} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/v1/responses", nil)
		require.NoError(t, err)
		resp, err := gw.doOpenAIUpstream(req, "", &accounts.pool[0])
		require.NoError(t, err)
		require.Equal(t, code, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	}
	obs := gw.newAccountRiskModelObserver(ctx, nil, &accounts.pool[0], "demo")
	obs.ObserveOpenAI([]byte(`{"error":{"type":"authentication_error"}}`), "error")
	obs.ObserveOpenAI([]byte(`{"response":{"error":{"status":401}}}`), "response.failed")
	obs.ObserveOpenAI([]byte(`{"error":{"status":403,"type":"authentication_error"}}`), "error")
	require.Len(t, r.Events, 3)
	for i := 0; i < 3; i++ {
		require.NoError(t, risk.deliverNotification(ctx, now.Add(time.Minute*time.Duration(i))))
	}
	require.Len(t, n.messages, 1)
	require.Contains(t, n.messages[0], "合并异常次数：3")
	require.Equal(t, "merged", r.Events[0].Notification)
	require.NotContains(t, n.messages[0], "secret-error")
	require.Empty(t, store.stopped)
}
