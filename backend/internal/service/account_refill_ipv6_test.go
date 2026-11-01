//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
	"github.com/stretchr/testify/require"
)

type refillIPv6Proxies struct {
	ovhRefillProxies
	status ovhipv6.Status
	err    error
}

func (p *refillIPv6Proxies) GetOVHIPv6(context.Context) (*ovhipv6.Status, error) {
	return &p.status, p.err
}
func (p *refillIPv6Proxies) SetOVHIPv6(context.Context, bool) (*ovhipv6.Status, error) {
	return &p.status, p.err
}
func TestOVHIPv6RefillPreflightIndependentOfRandomToggle(t *testing.T) {
	p := &refillIPv6Proxies{status: ovhipv6.Status{Configured: true, Available: true, Enabled: false}}
	s := &AccountRefillService{proxies: p}
	id, err := s.selectProxy(context.Background(), AccountRefillDeliveryConfig{ProxyMode: "ovh_ipv6"})
	require.NoError(t, err)
	require.Nil(t, id, "preflight must leave allocation to the account transaction")
	p.status.Available = false
	_, err = s.selectProxy(context.Background(), AccountRefillDeliveryConfig{ProxyMode: "ovh_ipv6"})
	require.ErrorContains(t, err, "未就绪")
	p.err = errors.New("status unavailable")
	_, err = s.selectProxy(context.Background(), AccountRefillDeliveryConfig{ProxyMode: "ovh_ipv6"})
	require.ErrorContains(t, err, "status unavailable")
}

func TestOVHIPv6RefillSpecifiedRejectsManagedAddressBeforePurchaseAndImport(t *testing.T) {
	id := int64(12)
	for _, state := range []string{"reserved", "bound", "retiring", ""} {
		t.Run(state, func(t *testing.T) {
			s, store, provider, accounts := refillHarness(t)
			s.proxies = &refillIPv6Proxies{ovhRefillProxies: ovhRefillProxies{{ID: id, Username: "ovh6_100", ManagedIPv6State: state}}}
			delivery := refillTestConfig().Delivery
			delivery.ProxyMode, delivery.ProxyID = "specified", &id
			store.mutate(t, func(v *accountRefillState) { v.Config.Delivery = delivery })
			ctx := context.Background()
			require.NoError(t, s.run(ctx, false))
			require.Empty(t, provider.submitted, "a reservation must not authorize multiple paid orders")
			v, err := s.View(ctx)
			require.NoError(t, err)
			require.Contains(t, v.Message, "ovh_ipv6")
			job := &AccountRefillJob{ID: "legacy-job", SupplierID: "already-paid", DeliveryConfig: delivery}
			err = s.importDelivery(ctx, "fixture", job)
			require.ErrorContains(t, err, "ovh_ipv6")
			require.Zero(t, accounts.creates)
			require.Empty(t, provider.submitted)
		})
	}
}

type refillIPv6Accounts struct{ *refillTestAccounts }

func (a *refillIPv6Accounts) CreateAccountWithProxySelection(ctx context.Context, in *CreateAccountInput) (*Account, error) {
	a.selectionCalls++
	if in.ProxyMode != "ovh_ipv6" || in.ProxyID != nil {
		return nil, errors.New("expected fresh IPv6 allocation at save")
	}
	if a.selectionError != nil {
		return nil, a.selectionError
	}
	in.ProxyID = &a.selectedProxyID
	return a.CreateAccount(ctx, in)
}

func TestOVHIPv6RefillDeliveryRetriesAllocationWithoutRepurchasing(t *testing.T) {
	s, store, provider, accounts := refillHarness(t)
	s.admin = &refillIPv6Accounts{accounts}
	s.proxies = &refillIPv6Proxies{status: ovhipv6.Status{Configured: true, Available: true, Enabled: false}}
	store.mutate(t, func(v *accountRefillState) { v.Config.Delivery.ProxyMode = "ovh_ipv6" })
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	provider.status = "succeeded"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	accounts.selectionError = errors.New("IPv6 daemon temporarily unavailable")
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "import_failed", v.Jobs[0].Status)
	require.Zero(t, accounts.creates)
	require.Equal(t, 1, accounts.selectionCalls)
	accounts.selectionError = nil
	accounts.selectedProxyID = 74
	require.NoError(t, s.Retry(ctx, v.Jobs[0].ID))
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, accounts.creates)
	require.EqualValues(t, 74, *accounts.accounts[0].ProxyID)
	require.Len(t, provider.submitted, 1, "a failed address allocation must reuse the paid order")
}
