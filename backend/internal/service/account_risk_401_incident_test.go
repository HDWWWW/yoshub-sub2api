//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type test401Incident struct {
	first           string
	count           int
	last, recovered time.Time
}

func (s *riskTestStore) record401(groupID, ownerID int64, id string, at time.Time) (string, int, error) {
	if ownerID <= 0 {
		return "", 0, errors.New("missing credential owner")
	}
	if s.incidents == nil {
		s.incidents = map[[2]int64]test401Incident{}
	}
	k := [2]int64{groupID, ownerID}
	e := s.incidents[k]
	switch {
	case e.first == "" || (!e.last.After(e.recovered) && at.After(e.recovered)):
		e.first, e.count = id, 1
	case e.recovered.IsZero() || !e.last.After(e.recovered) || at.After(e.recovered):
		e.count++
	}
	if at.After(e.last) {
		e.last = at
	}
	s.incidents[k] = e
	return e.first, e.count, nil
}

func (s *riskTestStore) Recover401(_ context.Context, id int64, startedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, e := range s.incidents {
		if k[1] == id && e.last.Before(startedAt) && e.recovered.Before(e.last) {
			e.recovered = startedAt
			s.incidents[k] = e
		}
	}
	return nil
}

// A gateway failure, a probe action and maintenance traffic share one claim
// across workers and an independent claim in each subscribed group.
func TestAccountRisk401IncidentConcurrentSources(t *testing.T) {
	s, store, accounts, _, _ := riskHarness()
	ctx := context.Background()
	rule := store.state.Rules[7]
	rule.Config.Notify401Enabled = true
	store.state.Rules[8] = &AccountRiskRule{GroupID: 8, Config: rule.Config}
	a := &accounts.pool[0]
	a.GroupIDs = []int64{7, 8}
	shadow := &accounts.pool[1]
	shadow.ParentAccountID = &a.ID
	const reps = 12
	var wg sync.WaitGroup
	errs := make(chan error, reps)
	for i := range reps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := NewAccount401Reporter(store)
			r.record(ctx, 7, shadow, accountRiskBusiness401, "gateway", "握手 401")
			r.observe(ctx, 0, a, 401, "获取模型列表")
			errs <- store.Update(ctx, func(_ *AccountRiskState, tx AccountRiskScheduling) error {
				e := &riskProbe401Evidence{ownerID: a.ID, ownerName: a.Name, alertTarget: &Account401AlertTarget{AccountID: a.ID}, receivedAt: time.Now().UTC()}
				job := &accountRiskJob{pending: AccountRiskPending{ID: fmt.Sprintf("probe-%d", i)}}
				return appendProbe401Event(rule, tx, job, e, e.receivedAt, "cooldown", "自动探测 401", false)
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	n := &riskNoticeStub{}
	s.notifier = n
	now := time.Now().UTC()
	for _, g := range []int64{7, 8} {
		events := store.state.Rules[g].Events
		pending := 0
		for _, e := range events {
			if e.Notification == "pending" {
				pending++
				require.Equal(t, len(events), e.Occurrences)
			} else {
				require.Equal(t, "merged", e.Notification)
			}
		}
		require.Equal(t, 1, pending)
	}
	for i := range 3 {
		require.NoError(t, s.deliverNotification(ctx, now.Add(time.Duration(i)*4*time.Second)))
	}
	require.Len(t, n.messages, 2)
	require.Equal(t, 3*reps, store.incidents[[2]int64{7, a.ID}].count)
	require.Equal(t, reps, store.incidents[[2]int64{8, a.ID}].count)
	// Simulate an evicted sent row and a worker restart. Neither rearms the notice.
	rule.Events = nil
	NewAccount401Reporter(store).observe(ctx, 7, a, 401, "获取模型列表")
	require.Equal(t, "merged", rule.Events[0].Notification)
	require.Equal(t, 3*reps+1, rule.Events[0].Occurrences)
}

func TestAccountRisk401IncidentRearmsOnVerifiedProbe(t *testing.T) {
	s, store, accounts, _, _ := riskHarness()
	ctx := context.Background()
	rule := store.state.Rules[7]
	rule.Config.Notify401Enabled = true
	a := &accounts.pool[0]
	a.Credentials = map[string]any{"access_token": "fixture"}
	r := s.reporter401()
	observe := func(want string) {
		r.observe(ctx, 7, a, 401, "获取模型列表")
		require.Equal(t, want, rule.Events[0].Notification)
	}
	observe("pending")
	// A stale check, a transport 200, and reusing a cached access token are insufficient.
	r.recovered(ctx, a, rule.Events[0].At.Add(-time.Second))
	ObserveAccountHTTPStatus(r.withScope(ctx, a.ID, "凭据刷新"), 200)
	oauth := &OpenAIOAuthService{alerts401: r}
	_, err := oauth.RefreshAccountToken(ctx, a)
	require.NoError(t, err)
	observe("merged")
	gw := &OpenAIGatewayService{accountRisk: s}
	run := func(body string) (AccountRiskProbeResult, error) {
		tester := &AccountTestService{openaiGatewayService: gw, accountRepo: &openAIAccountTestRepo{}, httpUpstream: &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(200, body)}}}
		return tester.RunRiskProbe(ctx, a, rule.Config)
	}
	_, err = run("data: {\"type\":\"error\",\"error\":{\"status\":401}}\n\n")
	require.Error(t, err)
	observe("merged")
	result, err := run("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	require.NoError(t, err)
	require.True(t, result.Completed)
	observe("pending")
	require.Equal(t, 1, rule.Events[0].Occurrences)
	observe("merged")
}

func TestAccountRisk401UsageOnlyLeavesFirstNotificationAvailable(t *testing.T) {
	s, store, accounts, _, _ := riskHarness()
	ctx := context.Background()
	rule := store.state.Rules[7]
	rule.Config.Notify401Enabled = true
	a := &accounts.pool[0]
	r := s.reporter401()
	usage := withUsage401Audit(ctx)
	r.observe(usage, 7, a, 401, "用量探测")
	ObserveAccountHTTPStatus(r.withScope(usage, a.ID, "查询账号用量"), 401)
	ObserveAccountHTTPStatus(r.withScope(usage, a.ID, "注册账号任务"), 401)
	require.Len(t, rule.Events, 3)
	for _, event := range rule.Events {
		require.Equal(t, risk401HistoryOnly, event.Notification)
		require.Zero(t, event.Occurrences)
	}
	require.Empty(t, store.incidents)
	r.record(ctx, 7, a, accountRiskBusiness401, "gateway", "业务请求收到上游 401")
	require.Equal(t, "pending", rule.Events[0].Notification)
	require.Equal(t, 1, rule.Events[0].Occurrences)
	ObserveAccountHTTPStatus(r.withScope(usage, a.ID, "查询账号用量"), 401)
	ObserveAccountHTTPStatus(r.withScope(ctx, a.ID, "账号凭据刷新"), 401)
	require.Equal(t, "merged", rule.Events[0].Notification)
	require.Equal(t, 2, rule.Events[0].Occurrences)
}

func TestAccountRisk401PendingLegacyUsageIsHistoryOnly(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	rule := store.state.Rules[7]
	rule.Config.Notify401Enabled = true
	s.notifier = &riskNoticeStub{}
	for _, reason := range []string{"用量探测收到上游 401", "查询账号用量收到上游 401"} {
		rule.Events = append(rule.Events, AccountRiskEvent{ID: reason, At: now, AccountID: accounts.pool[0].ID,
			Kind: accountRiskAccount401, Action: "observed", Reason: reason, Notification: "pending"})
	}
	require.NoError(t, s.deliverNotification(context.Background(), now))
	for _, e := range rule.Events {
		require.Equal(t, risk401HistoryOnly, e.Notification)
	}
	require.Empty(t, s.notifier.(*riskNoticeStub).messages)
}
