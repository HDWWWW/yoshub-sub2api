//go:build unit

package service

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountRefillViewReadsLiveLocalQuotaWithoutProviderOrLock(t *testing.T) {
	s, store, _, accounts := refillHarness(t)
	// A nil provider makes accidental supplier calls fail the test immediately.
	s.provider = nil
	accounts.accounts = []Account{refillTestAccount(time.Now().Add(time.Hour))}
	accounts.accounts[0].Extra = map[string]any{"codex_7d_used_percent": 97.0, "codex_7d_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}
	old := 9.0
	store.mutate(t, func(v *accountRefillState) {
		v.Config.Enabled, v.Config.MonitorEnabled = false, false
		v.ProviderRetryAt = time.Now().Add(time.Hour)
		v.Evaluation = AccountRefillEvaluation{Accounts: []AccountRefillAccount{{ID: 1, Remaining7d: &old}}}
	})
	before := store.raw
	for _, used := range []float64{97, 98} {
		accounts.accounts[0].Extra["codex_7d_used_percent"] = used
		v, err := s.View(context.Background())
		require.NoError(t, err)
		require.Equal(t, 100-used, *v.Evaluation.Accounts[0].Remaining7d)
		require.Nil(t, v.Evaluation.Accounts[0].Remaining5h)
		require.WithinDuration(t, time.Now(), v.Evaluation.CheckedAt, time.Second)
	}
	require.Equal(t, before, store.raw, "reading the view must not write settings or need the worker lock")
}
func TestAccountRefillInvalidOrExpiredQuotaIsUnknown(t *testing.T) {
	now := time.Now()
	for _, value := range []any{nil, "bad", -1.0, 101.0, math.NaN(), math.Inf(1)} {
		a := refillTestAccount(now.Add(time.Hour))
		a.Extra = map[string]any{"codex_7d_used_percent": value}
		e := evaluateAccountRefill(refillTestConfig(), []Account{a}, nil, nil, now)
		require.Nil(t, e.Accounts[0].Remaining7d)
	}
	a := refillTestAccount(now.Add(time.Hour))
	a.Extra = map[string]any{"codex_7d_used_percent": 97.0, "codex_7d_reset_at": now.Add(-time.Minute).Format(time.RFC3339)}
	e := evaluateAccountRefill(refillTestConfig(), []Account{a}, nil, nil, now)
	require.Nil(t, e.Accounts[0].Remaining7d, "past-window usage does not imply fresh 100 percent remaining")
}

type refillDemandChangeProvider struct {
	*refillTestProvider
	accounts *refillTestAccounts
}

func (p *refillDemandChangeProvider) Catalog(ctx context.Context, token string) (*RefillCatalog, error) {
	// Simulate new local quota/account data arriving during a slow supplier call.
	p.accounts.accounts = []Account{refillTestAccount(time.Now().Add(2 * time.Hour))}
	return p.refillTestProvider.Catalog(ctx, token)
}
func TestAccountRefillRechecksDemandAfterSlowProviderCall(t *testing.T) {
	s, _, p, a := refillHarness(t)
	s.provider = &refillDemandChangeProvider{p, a}
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, p.submitted, "no purchase after local shortage resolves while the supplier responds")
}
