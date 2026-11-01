//go:build unit

package service

import (
	"context"
	"errors"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
	"testing"
)

type ovhRefillProxies []Proxy

func (p ovhRefillProxies) ListActive(context.Context) ([]Proxy, error) { return p, nil }
func TestOVHIPv6CannotBeSharedByPaidRefill(t *testing.T) {
	id := int64(12)
	for _, p := range []Proxy{{ID: id, Username: "ovh6_100"}, {ID: id, ManagedIPv6: "2604:2dc0:205:b900::64"}} {
		s := AccountRefillService{proxies: ovhRefillProxies{p}}
		_, err := s.selectProxy(context.Background(), AccountRefillDeliveryConfig{ProxyMode: "specified", ProxyID: &id})
		require.ErrorContains(t, err, "独占")
	}
	s := AccountRefillService{proxies: ovhRefillProxies{{ID: id, Username: "manual"}}}
	got, err := s.selectProxy(context.Background(), AccountRefillDeliveryConfig{ProxyMode: "specified", ProxyID: &id})
	require.NoError(t, err)
	require.Equal(t, id, *got)
}

type unavailableOVHProxy struct {
	ProxyRepository
	err error
}

func (p unavailableOVHProxy) GetByID(context.Context, int64) (*Proxy, error) { return nil, p.err }
func TestOVHIPv6RetiredPrivacyBindingNeverGoesDirect(t *testing.T) {
	for _, lookupErr := range []error{nil, errors.New("deleted proxy")} {
		calls := 0
		svc := adminServiceImpl{proxyRepo: unavailableOVHProxy{err: lookupErr}, privacyClientFactory: func(string) (*req.Client, error) { calls++; return nil, errors.New("must not be called") }}
		id := int64(42)
		account := &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ProxyID: &id, Credentials: map[string]any{"access_token": "test-only"}}
		require.Empty(t, svc.EnsureOpenAIPrivacy(context.Background(), account))
		require.Empty(t, svc.ForceOpenAIPrivacy(context.Background(), account))
		require.Zero(t, calls)
	}
}
