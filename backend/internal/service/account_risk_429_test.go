//go:build unit

package service

import (
	"context"
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
