//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Reproduce the November 25 incident through the model picker, including a
// second independently subscribed group and a disabled group on the same account.
func TestAccountRisk401AdminPickerAndTest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"token_revoked","message":"private-account-error"}}`))
	}))
	t.Cleanup(server.Close)
	original := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	t.Cleanup(func() { chatgptCodexModelsURL = original })
	risk, store, _, _, _ := riskHarness()
	r := store.state.Rules[7]
	r.Config.Enabled, r.Config.ProbeEnabled, r.Config.Notify401Enabled = false, false, true
	store.state.Rules[8] = &AccountRiskRule{GroupID: 8, Config: r.Config}
	store.state.Rules[9] = &AccountRiskRule{GroupID: 9}
	gw := newCodexModels401TestService(&codexModelsAccountStateRepo{})
	gw.accountRisk = risk
	up := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(401, "any reason"), newJSONResponse(403, "401 is only text")}}
	tester := &AccountTestService{openaiGatewayService: gw, accountRepo: &openAIAccountTestRepo{}, httpUpstream: up}
	a := newCodexModelsTestAccount()
	a.GroupIDs = []int64{7, 7, 8, 9}
	for _, id := range []int64{885, 886, 887} {
		a.ID = id
		_, err := tester.FetchOpenAIAccountModels(context.Background(), a)
		require.Error(t, err)
	}
	require.Len(t, r.Events, 3)
	require.Len(t, store.state.Rules[8].Events, 3)
	require.Empty(t, store.state.Rules[9].Events)
	for i, id := range []int64{887, 886, 885} {
		require.Equal(t, id, r.Events[i].AccountID)
		require.Contains(t, r.Events[i].Reason, "获取模型列表")
		require.NotContains(t, r.Events[i].Reason, "private-account-error")
		require.Equal(t, "pending", r.Events[i].Notification)
	}
	for range 2 {
		c, _ := newTestContext()
		require.Error(t, tester.testOpenAIAccountConnection(c, a, "gpt-5.4", "", ""))
	}
	require.Len(t, r.Events, 4)
	require.Contains(t, r.Events[0].Reason, "手动测试")
	scoped := WithHeaderOverrideRequestScope(context.Background(), 1, 1, &r.GroupID)
	_, err := tester.FetchOpenAIAccountModels(scoped, a)
	require.Error(t, err)
	require.Len(t, r.Events, 5)
	require.Len(t, store.state.Rules[8].Events, 4)
	r.Config.Notify401Enabled = false
	_, err = tester.FetchOpenAIAccountModels(scoped, a)
	require.Error(t, err)
	require.Len(t, r.Events, 5)
	require.Len(t, store.state.Rules[8].Events, 4)
}

func TestAccountRisk401ManualPreviewUsesSavedRuleAndWireStatus(t *testing.T) {
	risk, store, accounts, _, _ := riskHarness()
	r := store.state.Rules[7]
	accounts.pool[0].Credentials = map[string]any{"access_token": "fixture"}
	gw := &OpenAIGatewayService{accountRisk: risk}
	for _, tc := range []struct {
		enabled bool
		status  int
		body    string
		want    int
	}{
		{false, 401, "unauthorized", 0}, {true, 401, "unknown reason", 1},
		{true, 200, "data: {\"type\":\"error\",\"error\":{\"status\":401}}\n\n", 2},
		{true, 403, "401 in error text", 2},
	} {
		r.Config.Notify401Enabled = tc.enabled
		draft := r.Config
		draft.Notify401Enabled = !tc.enabled
		risk.probe = &AccountTestService{openaiGatewayService: gw, accountRepo: &openAIAccountTestRepo{}, httpUpstream: &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(tc.status, tc.body)}}}
		_, err := risk.TestNow(context.Background(), 7, 1, r.Revision, draft)
		require.NoError(t, err)
		require.Len(t, r.Events, tc.want)
		require.Empty(t, store.stopped)
	}
}

func TestAccountRisk401LateProbeAndMaintenanceResponse(t *testing.T) {
	risk, store, accounts, _, now := riskHarness()
	r := store.state.Rules[7]
	r.Config.Notify401Enabled = true
	job := riskDueJob(t, risk, accounts, now)
	r.Revision++
	evidence := &riskProbe401Evidence{ownerID: job.account.ID, receivedAt: time.Now().UTC()}
	for range 2 {
		require.NoError(t, risk.finishProbe(context.Background(), job, AccountRiskProbeResult{UpstreamStatus: 401, auth401: evidence}, errors.New("upstream"), now))
	}
	require.Len(t, r.Events, 1)
	require.Empty(t, store.stopped)
	ctx := risk.reporter401().withScope(context.Background(), job.account.ID, "账号凭据刷新")
	ObserveAccountHTTPStatus(ctx, 401)
	ObserveAccountHTTPStatus(ctx, 403)
	ObserveAccountHTTPStatus(context.Background(), 401)
	require.Len(t, r.Events, 2)
	require.Contains(t, r.Events[0].Reason, "账号凭据刷新")
}

func TestAccountRisk401IncidentQueueSpacing(t *testing.T) {
	risk, store, _, _, now := riskHarness()
	n := &riskNoticeStub{}
	risk.notifier = n
	r := store.state.Rules[7]
	r.Config.Notify401Enabled = true
	for _, id := range []int64{885, 886, 887} {
		risk.reporter401().observe(context.Background(), 7, &Account{ID: id, Platform: PlatformOpenAI}, 401, "获取模型列表")
	}
	for _, tc := range []struct {
		ms   int
		want int
	}{{0, 1}, {2999, 1}, {3000, 2}, {5999, 2}, {6000, 3}} {
		require.NoError(t, risk.deliverNotification(context.Background(), now.Add(time.Duration(tc.ms)*time.Millisecond)))
		require.Len(t, n.messages, tc.want)
	}
	for i, id := range []string{"885", "886", "887"} {
		require.Contains(t, n.messages[i], id)
		require.Equal(t, "sent", r.Events[i].Notification)
	}
}

func TestAccountRisk401BufferedErrorsAndAgentRegistration(t *testing.T) {
	risk, store, _, _, _ := riskHarness()
	r := store.state.Rules[7]
	r.Config.Notify401Enabled = true
	gw := &OpenAIGatewayService{accountRisk: risk}
	_, key := newTestAgentIdentityKey(t)
	a := &Account{ID: 885, GroupIDs: []int64{7}, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"auth_mode": OpenAIAuthModeAgentIdentity, "agent_runtime_id": "fixture", "agent_private_key": key}}
	ts := &AccountTestService{openaiGatewayService: gw}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"error":{"status_code":"401","message":"arbitrary reason"}}`, 1},
		{"event: error\ndata: {\"status\":401}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"token_revoked\"}}}\n\n", 2},
		{`{"error":{"status":403,"code":"invalid_api_key","message":"401"}}`, 2},
		{`{"type":"response.output_text.delta","delta":"401","status":401}`, 2},
	} {
		ts.observeTest401Body(context.Background(), a, 200, []byte(tc.body), "测试图片")
		require.Len(t, r.Events, tc.want)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(server.Close)
	original := openAIAgentIdentityAuthAPIBaseURL
	openAIAgentIdentityAuthAPIBaseURL = server.URL
	t.Cleanup(func() { openAIAgentIdentityAuthAPIBaseURL = original })
	_, err := registerAgentIdentityTask(context.Background(), a, risk.reporter401())
	require.Error(t, err)
	require.Len(t, r.Events, 3)
	require.Contains(t, r.Events[0].Reason, "注册账号任务")
}
