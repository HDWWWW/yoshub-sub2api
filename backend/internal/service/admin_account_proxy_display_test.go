//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Unimplemented repository methods panic, so reads cannot silently allocate or save.
type accountProxyDisplayRepo struct {
	AccountRepository
	account *Account
	err     error
	readID  int64
}

func (r *accountProxyDisplayRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.readID = id
	return r.account, r.err
}

type accountProxyDisplayMetadata struct {
	ProxyRepository
	details map[int64]ManagedIPv6Binding
	err     error
	readIDs []int64
}

func (r *accountProxyDisplayMetadata) ManagedIPv6Addresses(context.Context, []int64) (map[int64]string, error) {
	panic("use managed binding details when available")
}

func (r *accountProxyDisplayMetadata) ManagedIPv6Details(_ context.Context, ids []int64) (map[int64]ManagedIPv6Binding, error) {
	r.readIDs = append(r.readIDs, ids...)
	return r.details, r.err
}

func TestAdminGetAccountIncludesCurrentManagedIPv6Binding(t *testing.T) {
	proxyID := int64(45)
	accounts := &accountProxyDisplayRepo{account: &Account{
		ID: 86, ProxyID: &proxyID,
		Proxy: &Proxy{ID: proxyID, Name: "newly imported proxy", Host: "egress.internal", Port: 1080},
	}}
	proxies := &accountProxyDisplayMetadata{details: map[int64]ManagedIPv6Binding{
		45: {Address: "2001:db8::45", State: "bound"},
		46: {Address: "2001:db8::46", State: "bound"},
	}}
	s := &adminServiceImpl{accountRepo: accounts, proxyRepo: proxies}

	account, err := s.GetAccount(context.Background(), 86)
	require.NoError(t, err)
	require.Equal(t, int64(86), accounts.readID)
	require.Equal(t, []int64{45}, proxies.readIDs)
	require.Equal(t, proxyID, *account.ProxyID)
	require.Equal(t, proxyID, account.Proxy.ID)
	require.Equal(t, "2001:db8::45", account.Proxy.ManagedIPv6)
	require.Equal(t, "bound", account.Proxy.ManagedIPv6State)
	require.Equal(t, "egress.internal", account.Proxy.Host)
}

func TestAdminGetAccountProxyDisplayPreservesOrdinaryAndDirectAccounts(t *testing.T) {
	proxyID := int64(12)
	for _, tc := range []struct {
		name    string
		account *Account
		readIDs []int64
	}{
		{"manual proxy", &Account{ID: 1, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Host: "manual.example"}}, []int64{12}},
		{"direct", &Account{ID: 2}, nil},
		{"missing proxy details", &Account{ID: 3, ProxyID: &proxyID}, nil},
		{"nil account", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := &accountProxyDisplayRepo{account: tc.account}
			proxies := &accountProxyDisplayMetadata{}
			s := &adminServiceImpl{accountRepo: accounts, proxyRepo: proxies}
			account, err := s.GetAccount(context.Background(), 1)
			require.NoError(t, err)
			require.Equal(t, tc.account, account)
			require.Equal(t, tc.readIDs, proxies.readIDs)
			if account != nil && account.Proxy != nil {
				require.Empty(t, account.Proxy.ManagedIPv6)
				require.Equal(t, "manual.example", account.Proxy.Host)
			}
		})
	}
}

func TestAdminGetAccountProxyDisplayWorksWithoutManagedIPv6Repository(t *testing.T) {
	accounts := &accountProxyDisplayRepo{account: &Account{ID: 1, Proxy: &Proxy{ID: 12}}}
	s := &adminServiceImpl{accountRepo: accounts}
	account, err := s.GetAccount(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, accounts.account, account)
}

func TestAdminGetAccountProxyDisplayPropagatesReadErrors(t *testing.T) {
	readErr := errors.New("account unavailable")
	metadataErr := errors.New("proxy binding unavailable")
	for _, tc := range []struct {
		name                          string
		accountErr, proxyErr, wantErr error
	}{
		{"account read", readErr, nil, readErr},
		{"binding read", nil, metadataErr, metadataErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := &accountProxyDisplayRepo{account: &Account{ID: 1, Proxy: &Proxy{ID: 12}}, err: tc.accountErr}
			proxies := &accountProxyDisplayMetadata{err: tc.proxyErr}
			s := &adminServiceImpl{accountRepo: accounts, proxyRepo: proxies}
			account, err := s.GetAccount(context.Background(), 1)
			require.ErrorIs(t, err, tc.wantErr)
			require.Nil(t, account)
			if tc.accountErr != nil {
				require.Empty(t, proxies.readIDs)
			}
		})
	}
}
