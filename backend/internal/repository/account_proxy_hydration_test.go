//go:build unit

package repository

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/stretchr/testify/require"
)

func proxyHydrationRepository(t *testing.T) (*accountRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = client.Close() })
	return newAccountRepositoryWithSQL(client, db, nil), mock
}

func TestAccountProxyHydrationFailsClosedDuringRotation(t *testing.T) {
	for _, method := range []string{"GetByID", "GetByIDs"} {
		t.Run(method, func(t *testing.T) {
			repo, mock := proxyHydrationRepository(t)
			// Deterministic interleaving: the account SELECT sees its old proxy,
			// then rotation commits and soft-deletes that proxy before its SELECT.
			mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE .*`).WithArgs(int64(41)).
				WillReturnRows(sqlmock.NewRows([]string{"id", "proxy_id", "name", "platform", "type"}).
					AddRow(int64(41), int64(7), "account", "openai", "oauth"))
			mock.ExpectQuery(`(?s)SELECT .* FROM "proxies" WHERE .*deleted_at.*`).WithArgs(int64(7)).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
			if method == "GetByID" {
				account, err := repo.GetByID(context.Background(), 41)
				require.ErrorContains(t, err, "proxy 7 unavailable; direct egress refused")
				require.Nil(t, account, "must not return a usable account with a missing proxy")
			} else {
				accounts, err := repo.GetByIDs(context.Background(), []int64{41})
				require.ErrorContains(t, err, "proxy 7 unavailable; direct egress refused")
				require.Nil(t, accounts, "the eager-proxy path must also reject incomplete identities")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestAccountProxyHydrationRejectsEntireMixedSnapshot(t *testing.T) {
	repo, mock := proxyHydrationRepository(t)
	proxyID := int64(7)
	accounts := []*dbent.Account{{ID: 40}, {ID: 41, ProxyID: &proxyID}}
	mock.ExpectQuery(`(?s)SELECT .* FROM "proxies" WHERE .*deleted_at.*`).WithArgs(proxyID).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	loaded, err := repo.accountsToService(context.Background(), accounts)
	require.ErrorContains(t, err, "direct egress refused")
	require.Nil(t, loaded, "list and scheduler snapshots must not publish a partially hydrated account")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountProxyHydrationPreservesExplicitDirectAndResolvedProxy(t *testing.T) {
	for _, method := range []string{"GetByID", "GetByIDs"} {
		t.Run(method, func(t *testing.T) {
			repo, mock := proxyHydrationRepository(t)
			if method == "GetByID" {
				mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE .*`).WithArgs(int64(40)).
					WillReturnRows(sqlmock.NewRows([]string{"id", "proxy_id"}).AddRow(int64(40), nil))
				mock.ExpectQuery(`(?s)account_groups`).WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id", "priority", "created_at"}))
				account, err := repo.GetByID(context.Background(), 40)
				require.NoError(t, err)
				require.Nil(t, account.ProxyID)
				require.Nil(t, account.Proxy, "an explicitly direct account is still supported")
			} else {
				mock.ExpectQuery(`(?s)SELECT .* FROM "accounts" WHERE .*`).WithArgs(int64(41), int64(40)).
					WillReturnRows(sqlmock.NewRows([]string{"id", "proxy_id"}).AddRow(int64(40), nil).AddRow(int64(41), int64(8)))
				mock.ExpectQuery(`(?s)SELECT .* FROM "proxies" WHERE .*deleted_at.*`).WithArgs(int64(8)).
					WillReturnRows(sqlmock.NewRows([]string{"id", "protocol", "host", "port", "username", "password", "status"}).
						AddRow(int64(8), "http", "192.0.2.1", 11880, "ovh6_8", "test-only", "active"))
				mock.ExpectQuery(`(?s)account_groups`).WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id", "priority", "created_at"}))
				accounts, err := repo.GetByIDs(context.Background(), []int64{41, 40})
				require.NoError(t, err)
				require.Len(t, accounts, 2)
				require.EqualValues(t, 41, accounts[0].ID)
				require.NotNil(t, accounts[0].Proxy)
				require.EqualValues(t, 8, accounts[0].Proxy.ID)
				require.Contains(t, accounts[0].Proxy.URL(), "ovh6_8")
				require.Nil(t, accounts[1].ProxyID)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
