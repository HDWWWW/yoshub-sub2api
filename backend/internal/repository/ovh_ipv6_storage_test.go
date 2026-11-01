//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ovhipv6"
	"github.com/stretchr/testify/require"
)

func storageRepoFixture(t *testing.T) (*proxyRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	repo := newProxyRepositoryWithSQL(nil, db)
	repo.ipv6Supported = true
	return repo, mock
}

func TestOVHIPv6StorageCombinesRealFilesControlAndDatabase(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"counter.json", "counter.guard.json", "leases.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("opaque-private-data"), 0600))
	}
	t.Setenv("OVH_IPV6_STATE_STATS_DIR", dir)
	repo, mock := storageRepoFixture(t)
	mock.ExpectQuery(regexp.QuoteMeta(ovhIPv6StorageQuery)).WillReturnRows(sqlmock.NewRows([]string{"rows", "reserved", "bound", "retiring", "table_bytes", "index_bytes", "total_bytes"}).AddRow(6, 1, 3, 2, 8192, 32768, 40960))
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/status" || r.Header.Get("Authorization") != "Bearer private-control" {
			t.Error("statistics must use the authenticated read-only control status request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(ovhipv6.Status{Available: true, Prefix: "2604:2dc0:205:b900::/56", Issued: "18446744073709551617", Bound: 999, Pending: 999})
	}))
	defer server.Close()
	repo.ipv6 = &ovhipv6.Client{BaseURL: server.URL, Token: "private-control", HTTP: server.Client()}
	out, err := repo.GetOVHIPv6Storage(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, requests.Load())
	require.Equal(t, "18446744073709551617", *out.Issued, "must not truncate the host sequence to uint64 or float")
	require.True(t, out.ControlAvailable)
	require.True(t, out.Files.Available)
	require.True(t, out.Database.Available)
	require.EqualValues(t, 6, *out.Database.Rows)
	require.EqualValues(t, 3, *out.Database.Bound, "database counts are separate from the daemon's active count")
	require.EqualValues(t, 40960, *out.Database.TotalBytes)
	require.EqualValues(t, 2*len("opaque-private-data"), *out.Files.CounterBytes)
	require.EqualValues(t, 3*len("opaque-private-data"), *out.Files.TotalBytes)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "opaque-private-data")
	require.NotContains(t, string(encoded), "private-control")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOVHIPv6StorageFailuresRetainUnknownInsteadOfZero(t *testing.T) {
	t.Setenv("OVH_IPV6_STATE_STATS_DIR", "")
	repo, mock := storageRepoFixture(t)
	mock.ExpectQuery(regexp.QuoteMeta(ovhIPv6StorageQuery)).WillReturnError(errors.New("private SQL diagnostic"))
	out, err := repo.GetOVHIPv6Storage(context.Background())
	require.NoError(t, err)
	require.Nil(t, out.Issued)
	require.False(t, out.ControlAvailable)
	require.False(t, out.Files.Configured)
	require.False(t, out.Files.Available)
	require.False(t, out.Database.Available)
	require.Nil(t, out.Database.Rows)
	require.Nil(t, out.Database.TotalBytes)
	require.Nil(t, out.Files.CounterBytes)
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"issued":null`)
	require.Contains(t, string(encoded), `"total_bytes":null`)
	require.NotContains(t, string(encoded), "private SQL diagnostic")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOVHIPv6StorageControlErrorPreservesOtherSources(t *testing.T) {
	t.Setenv("OVH_IPV6_STATE_STATS_DIR", "")
	for _, issued := range []string{"", "-1", "001", "NaN"} {
		t.Run("invalid_"+issued, func(t *testing.T) {
			repo := &proxyRepository{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(ovhipv6.Status{Available: true, Issued: issued})
			}))
			defer server.Close()
			repo.ipv6 = &ovhipv6.Client{BaseURL: server.URL, HTTP: server.Client()}
			out, err := repo.GetOVHIPv6Storage(context.Background())
			require.NoError(t, err)
			require.Nil(t, out.Issued)
			require.False(t, out.ControlAvailable)
			require.NotEmpty(t, out.ControlError)
		})
	}
	repo, mock := storageRepoFixture(t)
	mock.ExpectQuery(regexp.QuoteMeta(ovhIPv6StorageQuery)).WillReturnRows(sqlmock.NewRows([]string{"rows", "reserved", "bound", "retiring", "table_bytes", "index_bytes", "total_bytes"}).AddRow(0, 0, 0, 0, 0, 16384, 16384))
	out, err := repo.GetOVHIPv6Storage(context.Background())
	require.NoError(t, err)
	require.Nil(t, out.Issued)
	require.True(t, out.Database.Available)
	require.EqualValues(t, 0, *out.Database.Rows, "observed zero is distinct from an unavailable count")
	require.EqualValues(t, 16384, *out.Database.TotalBytes, "an empty relation can still consume disk")
	require.NoError(t, mock.ExpectationsWereMet())
}
