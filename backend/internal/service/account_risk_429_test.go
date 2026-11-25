//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

type risk429TestCounter struct{ counts map[string]map[string]bool }

func (c *risk429TestCounter) Increment(_ context.Context, key, token string, _, _ int) (int64, error) {
	if c.counts == nil {
		c.counts = map[string]map[string]bool{}
	}
	if c.counts[key] == nil {
		c.counts[key] = map[string]bool{}
	}
	c.counts[key][token] = true
	return int64(len(c.counts[key])), nil
}

func TestAccountRisk429ThresholdAndResumeEpoch(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	s.counter429 = &risk429TestCounter{}
	for i := 1; i <= 20; i++ {
		stopped, err := s.Handle429(context.Background(), &accounts.pool[0], fmt.Sprint(i), now)
		require.NoError(t, err)
		require.False(t, stopped)
	}
	require.Empty(t, store.stopped)
	stopped, err := s.Handle429(context.Background(), &accounts.pool[0], "21", now)
	require.NoError(t, err)
	require.True(t, stopped)
	require.Equal(t, []int64{1}, store.stopped)
	require.Equal(t, "rate_limit_429", store.state.Rules[7].Events[0].Kind)
	require.Equal(t, 21, store.state.Rules[7].Events[0].Occurrences)
	stopped, err = s.Handle429(context.Background(), &accounts.pool[0], "after-resume", now)
	require.NoError(t, err)
	require.False(t, stopped, "a later manual resume starts fresh")
}

func TestAccountRisk429CustomRuleAndStaleAccounts(t *testing.T) {
	for _, scenario := range []string{"disabled", "rule disabled", "identity changed", "removed", "group inactive", "CAS changed", "valid"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			s.counter429 = &risk429TestCounter{}
			store.state.Rules[7].Config.RateLimit429Threshold = 1
			a := accounts.pool[0]
			switch scenario {
			case "disabled":
				store.state.Rules[7].Config.Enabled = false
			case "rule disabled":
				store.state.Rules[7].Config.RateLimit429Enabled = false
			case "identity changed":
				accounts.pool[0].Credentials = map[string]any{"access_token": "changed"}
			case "removed":
				accounts.pool[0].GroupIDs = nil
			case "group inactive":
				s.groups = &riskTestGroups{inactive: true}
			case "CAS changed":
				store.cas = false
			}
			_, err := s.Handle429(context.Background(), &a, "1", now)
			require.NoError(t, err)
			stopped, err := s.Handle429(context.Background(), &a, "2", now)
			if scenario == "CAS changed" {
				require.ErrorIs(t, err, errAccountRiskModelAccountChanged)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, scenario == "valid", stopped)
			if scenario != "valid" {
				require.Empty(t, store.stopped)
			}
		})
	}
}

func TestAccountRisk429PersistenceRetryDoesNotDoubleCount(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	s.counter429 = &risk429TestCounter{}
	store.state.Rules[7].Config.RateLimit429Threshold = 1
	_, err := s.Handle429(context.Background(), &accounts.pool[0], "one", now)
	require.NoError(t, err)
	store.busy = 1
	_, err = s.Handle429(context.Background(), &accounts.pool[0], "two", now)
	require.ErrorIs(t, err, ErrAccountRiskBusy)
	stopped, err := s.Handle429(context.Background(), &accounts.pool[0], "two", now)
	require.NoError(t, err)
	require.True(t, stopped)
	require.Equal(t, 2, store.state.Rules[7].Events[0].Occurrences)
}

func TestAccountRisk429GatewayHTTPAndWebSocket(t *testing.T) {
	s, store, accounts, _, _ := riskHarness()
	s.counter429 = &risk429TestCounter{}
	store.state.Rules[7].Config.RateLimit429Threshold = 1
	gateway := &OpenAIGatewayService{accountRisk: s}
	a := &accounts.pool[0]
	require.False(t, gateway.handleOpenAIAccountUpstreamError(context.Background(), a, http.StatusBadRequest, nil, nil))
	require.False(t, gateway.handleOpenAIAccountUpstreamError(context.Background(), a, http.StatusTooManyRequests, nil, []byte(`{"error":{"code":"rate_limit_exceeded"}}`)))
	require.Empty(t, store.stopped)
	status, stopped := gateway.handleOpenAIStreamTerminalAccountSideEffects(nil, a, []byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded"}}}`), "Rate limit exceeded", nil)
	require.Equal(t, http.StatusTooManyRequests, status)
	require.True(t, stopped)
	require.Equal(t, []int64{1}, store.stopped)
}

func TestAccountRisk429ReduceConcurrencyViaGateway(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	s.counter429 = &risk429TestCounter{}
	r := store.state.Rules[7]
	r.Config.RateLimit429Enabled, r.Config.RateLimit429ReduceConcurrency = false, true
	r.Config.RateLimit429Threshold, r.Config.RateLimit429ConcurrencyLimit = 60, 4
	r.Config.NotifyEnabled = true
	gateway := &OpenAIGatewayService{accountRisk: s}
	snapshot := accounts.pool[0]
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		require.False(t, gateway.handleOpenAIAccountUpstreamError(ctx, &snapshot, http.StatusTooManyRequests, nil, nil))
	}
	require.Equal(t, 100, accounts.pool[0].Concurrency)
	require.Empty(t, r.Events)
	status, blocked := gateway.handleOpenAIStreamTerminalAccountSideEffects(nil, &snapshot, []byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Rate limit exceeded"}}}`), "Rate limit exceeded", nil)
	require.Equal(t, http.StatusTooManyRequests, status)
	require.False(t, blocked)
	require.False(t, gateway.isOpenAIAccountRuntimeBlocked(&snapshot))
	require.True(t, accounts.pool[0].Schedulable)
	require.Equal(t, 4, accounts.pool[0].Concurrency)
	require.Equal(t, 100, accounts.pool[1].Concurrency)
	require.Empty(t, store.stopped)
	require.Len(t, r.Events, 1)
	require.Equal(t, "rate_limit_429", r.Events[0].Kind)
	require.Equal(t, riskActionReduceConcurrency, r.Events[0].Action)
	require.Equal(t, 61, r.Events[0].Occurrences)
	require.Contains(t, r.Events[0].Reason, "从 100 降至 4")
	// In-flight responses may still carry the pre-change snapshot.
	for i := 0; i < 62; i++ {
		_, err := s.Handle429(ctx, &snapshot, fmt.Sprint(i), now)
		require.NoError(t, err)
	}
	require.Len(t, r.Events, 1)
	n := &riskNoticeStub{}
	s.notifier = n
	require.NoError(t, s.deliverNotification(ctx, now))
	require.Len(t, n.messages, 1)
	require.Contains(t, n.messages[0], "继续调度")
	require.NotContains(t, n.messages[0], "已关闭")
}

func TestAccountRisk429ReduceOnlyEligibleAccountsAndNeverRaise(t *testing.T) {
	for _, tc := range []struct {
		name              string
		before, after     int
		eligible, changed bool
	}{{"unlimited", 0, 5, true, true}, {"already lower", 2, 2, true, false}, {"already capped", 5, 5, true, false}, {"already paused", 10, 10, false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			s.counter429 = &risk429TestCounter{}
			r := store.state.Rules[7]
			r.Config.RateLimit429Enabled, r.Config.RateLimit429ReduceConcurrency = false, true
			r.Config.RateLimit429Threshold, r.Config.RateLimit429ConcurrencyLimit = 1, 5
			snapshot := accounts.pool[0] // still eligible in the gateway snapshot
			accounts.pool[0].Concurrency, accounts.pool[0].Schedulable = tc.before, tc.eligible
			if !tc.eligible {
				accounts.pool[0].Extra = map[string]any{AccountRiskCapacityPauseExtraKey: "owned"}
			}
			for _, token := range []string{"one", "two"} {
				stopped, err := s.Handle429(context.Background(), &snapshot, token, now)
				require.NoError(t, err)
				require.False(t, stopped)
			}
			require.Equal(t, tc.after, accounts.pool[0].Concurrency)
			require.Equal(t, tc.eligible, accounts.pool[0].Schedulable)
			if tc.changed {
				require.Len(t, r.Events, 1)
				require.Empty(t, r.Events[0].Notification)
			} else {
				require.Empty(t, r.Events)
			}
		})
	}
}

func TestAccountRisk429ReduceConfigAndIndependentStop(t *testing.T) {
	ctx := context.Background()
	s, store, accounts, _, now := riskHarness()
	s.counter429 = &risk429TestCounter{}
	var legacy AccountRiskConfig
	require.NoError(t, json.Unmarshal([]byte(`{"rate_limit_429_enabled":true}`), &legacy))
	require.True(t, legacy.RateLimit429Enabled)
	require.False(t, legacy.RateLimit429ReduceConcurrency)
	require.Equal(t, 1, legacy.RateLimit429ConcurrencyLimit)
	c := store.state.Rules[7].Config
	c.RateLimit429ReduceConcurrency = true
	require.Error(t, s.Configure(ctx, 7, 1, c), "both actions cannot be saved together")
	c.RateLimit429Enabled = false
	c.RateLimit429Threshold, c.RateLimit429ConcurrencyLimit = 1, 0
	require.Error(t, s.Configure(ctx, 7, 1, c), "a zero target would remove the limit")
	c.RateLimit429ConcurrencyLimit = 4
	require.NoError(t, s.Configure(ctx, 7, 1, c))
	// A separate associated group can still enforce its own 429 stop.
	stopConfig := DefaultAccountRiskConfig()
	stopConfig.Enabled, stopConfig.RateLimit429Threshold = true, 2
	require.NoError(t, s.Configure(ctx, 8, 0, stopConfig))
	accounts.pool[0].GroupIDs = []int64{7, 8}
	for i := 1; i <= 3; i++ {
		stopped, err := s.Handle429(ctx, &accounts.pool[0], fmt.Sprint(i), now)
		require.NoError(t, err)
		require.Equal(t, i == 3, stopped)
	}
	require.Equal(t, 4, accounts.pool[0].Concurrency)
	require.Equal(t, riskActionReduceConcurrency, store.state.Rules[7].Events[0].Action)
	require.Equal(t, "stop", store.state.Rules[8].Events[0].Action)
}
