//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type leaseTestProvider struct {
	*refillTestProvider
	response     *RefillProviderJob
	err          error
	statusCalls  int
	catalogCalls int
}

func (p *leaseTestProvider) Status(context.Context, string, string) (*RefillProviderJob, error) {
	p.statusCalls++
	return p.response, p.err
}

func (p *leaseTestProvider) Catalog(ctx context.Context, token string) (*RefillCatalog, error) {
	p.catalogCalls++
	return p.refillTestProvider.Catalog(ctx, token)
}

func TestRefillSourceLeaseSyncPreservesNativeAccountAndUpdatesLead(t *testing.T) {
	s, store, base, accounts := refillHarness(t)
	now := time.Now().Truncate(time.Second)
	a := refillTestAccount(now.AddDate(1, 0, 0))
	a.Priority, a.AutoPauseOnExpired = 17, true
	a.Extra = map[string]any{"refill_job_id": "job-1", AccountImportExpiryGraceExtraKey: 3600, "keep": true}
	accounts.accounts = []Account{a}
	expiry := now.Add(3 * time.Minute).Unix()
	p := &leaseTestProvider{refillTestProvider: base, response: &RefillProviderJob{ID: "supplier-1", ExpiresAt: &expiry}}
	s.provider = p
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	state.Config.LeadMinutes = 3
	state.Jobs = []AccountRefillJob{{ID: "job-1", SupplierID: "supplier-1", Status: "imported", AccountID: a.ID}}
	s.syncSourceLeases(context.Background(), "fixture", &state, now, false)
	updated := accounts.accounts[0]
	require.Equal(t, a.ExpiresAt, updated.ExpiresAt)
	require.Equal(t, a.Priority, updated.Priority)
	require.Equal(t, a.Schedulable, updated.Schedulable)
	require.Equal(t, a.GroupIDs, updated.GroupIDs)
	require.Equal(t, expiry+3600, updated.SchedulingExpiresAt().Unix())
	require.Equal(t, "supplier_api", updated.Extra[AccountSourceExpiryOriginExtraKey])
	require.Equal(t, expiry, *state.Jobs[0].ExpiresAt)
	before := evaluateAccountRefill(state.Config, accounts.accounts, nil, nil, now.Add(-time.Second))
	require.Zero(t, before.Needed)
	due := evaluateAccountRefill(state.Config, accounts.accounts, nil, nil, now)
	require.Equal(t, 1, due.Needed, "lead is measured from lease expiry, without grace")
	s.syncSourceLeases(context.Background(), "fixture", &state, now.Add(time.Minute), false)
	require.Equal(t, 1, p.statusCalls, "periodic sync is bounded to five minutes")
}

func TestRefillSourceLeaseSyncRejectsMissingOrUnrelatedOrders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *RefillProviderJob
		err      error
		mismatch bool
	}{
		{name: "website-404", err: &refillProviderError{code: 404}},
		{name: "network", err: errors.New("unavailable")},
		{name: "missing-expiry", response: &RefillProviderJob{ID: "supplier-1"}},
		{name: "wrong-order", response: &RefillProviderJob{ID: "other"}},
		{name: "wrong-account", mismatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, base, accounts := refillHarness(t)
			a := refillTestAccount(time.Now().Add(time.Hour))
			a.Extra = map[string]any{"refill_job_id": "job-1", AccountSourceExpiryExtraKey: a.ExpiresAt.Unix()}
			accounts.accounts = []Account{a}
			p := &leaseTestProvider{refillTestProvider: base, response: tc.response, err: tc.err}
			s.provider = p
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			state.Jobs = []AccountRefillJob{{ID: "job-1", SupplierID: "supplier-1", Status: "imported", AccountID: a.ID}}
			if tc.mismatch {
				state.Jobs[0].AccountID++
			}
			s.syncSourceLeases(context.Background(), "fixture", &state, time.Now(), true)
			require.Equal(t, a.ExpiresAt.Unix(), accounts.accounts[0].Extra[AccountSourceExpiryExtraKey])
			require.NotContains(t, accounts.accounts[0].Extra, AccountSourceExpiryCheckedExtraKey)
			require.Empty(t, base.submitted)
			if tc.mismatch {
				require.Zero(t, p.statusCalls)
			}
		})
	}
}

func TestRefillSourceLeaseRateLimitStopsOtherProviderRequests(t *testing.T) {
	s, store, base, accounts := refillHarness(t)
	a := refillTestAccount(time.Now().Add(time.Hour))
	a.Extra = map[string]any{"refill_job_id": "job-1"}
	accounts.accounts = []Account{a}
	p := &leaseTestProvider{refillTestProvider: base, err: &refillProviderError{code: 429}}
	s.provider = p
	store.mutate(t, func(state *accountRefillState) {
		state.Jobs = []AccountRefillJob{{ID: "job-1", SupplierID: "supplier-1", Status: "imported", AccountID: a.ID}}
	})
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, p.statusCalls)
	require.Zero(t, p.catalogCalls)
	require.Empty(t, base.submitted)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.True(t, state.ProviderRetryAt.After(time.Now()))
}
