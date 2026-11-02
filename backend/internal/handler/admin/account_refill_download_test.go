//go:build unit

package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type refillDownloadStore struct {
	raw    string
	writes int
}

func (s *refillDownloadStore) Read(context.Context) (string, error) { return s.raw, nil }
func (s *refillDownloadStore) WithLock(context.Context, func(string, func(string) error) error) error {
	s.writes++
	return nil
}

func TestRefillDownloadHandlerScopeAndErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, path, job string
		scoped          bool
		status          int
	}{
		{"missing", "/jobs/missing/delivery", `{"id":"local"}`, false, 404},
		{"not ready", "/jobs/local/delivery", `{"id":"local","status":"queued"}`, false, 409},
		{"expired", "/jobs/local/delivery", `{"id":"local","status":"delivery_expired"}`, false, 410},
		{"invalid group", "/jobs/local/delivery?group_id=-1", `{"id":"local"}`, false, 400},
		{"restricted key", "/jobs/local/delivery", `{"id":"local"}`, true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &refillDownloadStore{raw: fmt.Sprintf(`{"jobs":[%s]}`, tc.job)}
			svc := service.NewAccountRefillService(store, nil, nil, nil, nil, nil, nil)
			h := &AccountHandler{accountRefill: svc}
			router := gin.New()
			router.GET("/jobs/:job_id/delivery", h.DownloadAccountRefill)
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.scoped {
				req = req.WithContext(adminaccess.WithPrincipal(req.Context(), &adminaccess.Principal{KeyID: "restricted"}))
			}
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
			require.Empty(t, rec.Header().Get("Content-Disposition"))
			require.Zero(t, store.writes)
		})
	}
}

func TestRefillDownloadHandlerRetryAfter(t *testing.T) {
	store := &refillDownloadStore{raw: fmt.Sprintf(`{"provider_retry_at":%q,"jobs":[{"id":"local","supplier_id":"remote","supplier_status":"succeeded","result_ready":true}]}`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339))}
	h := &AccountHandler{accountRefill: service.NewAccountRefillService(store, nil, nil, nil, nil, nil, nil)}
	router := gin.New()
	router.GET("/jobs/:job_id/delivery", h.DownloadAccountRefill)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jobs/local/delivery", nil))
	require.Equal(t, 429, rec.Code, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Retry-After"))
	require.Zero(t, store.writes)
}
