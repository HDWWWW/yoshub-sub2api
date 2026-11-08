//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type autoFastHandlerGroups struct{ service.GroupRepository }

func (*autoFastHandlerGroups) GetByID(_ context.Context, id int64) (*service.Group, error) {
	return &service.Group{ID: id, Platform: service.PlatformOpenAI, Status: service.StatusActive}, nil
}

type autoFastHandlerStore struct {
	state *service.AccountAutoFastState
	err   error
}

func (s *autoFastHandlerStore) Read(context.Context) (*service.AccountAutoFastState, error) {
	return s.state, s.err
}
func (s *autoFastHandlerStore) Update(_ context.Context, fn func(*service.AccountAutoFastState, service.AccountAutoFastApply) error) error {
	if s.err != nil {
		return s.err
	}
	return fn(s.state, nil)
}

func TestAccountAutoFastHandlerAuthorizationValidationAndSafeErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	valid, _ := json.Marshal(map[string]any{"config": service.DefaultAccountAutoFastConfig(), "revision": 0})
	multiple := service.DefaultAccountAutoFastConfig()
	multiple.RequireMultipleAccounts = true
	withMultiple, _ := json.Marshal(map[string]any{"config": multiple, "revision": 0})
	for _, tc := range []struct {
		name, method, query, body string
		restricted, unavailable   bool
		err                       error
		status                    int
	}{
		{name: "view", method: "GET", status: 200},
		{name: "save", body: string(valid), status: 200},
		{name: "save multiple accounts", body: string(withMultiple), status: 200},
		{name: "restricted view", method: "GET", restricted: true, status: 403},
		{name: "restricted save", body: string(valid), restricted: true, status: 403},
		{name: "bad group", query: "x", status: 400},
		{name: "zero group", query: "0", status: 400},
		{name: "missing config", body: `{}`, status: 400},
		{name: "invalid config", body: `{"config":{},"revision":0}`, status: 400},
		{name: "negative revision", body: `{"config":{},"revision":-1}`, status: 400},
		{name: "malformed", body: `{`, status: 400},
		{name: "too large", body: strings.Repeat(" ", 16385) + string(valid), status: 400},
		{name: "conflict", body: string(valid), err: service.ErrAccountAutoFastConflict, status: 409},
		{name: "busy", body: string(valid), err: service.ErrAccountAutoFastBusy, status: 409},
		{name: "db save error", body: string(valid), err: errors.New("private db credential"), status: 503},
		{name: "db view error", method: "GET", err: errors.New("private db credential"), status: 503},
		{name: "no service", unavailable: true, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &autoFastHandlerStore{state: &service.AccountAutoFastState{Rules: map[int64]*service.AccountAutoFastRule{}, Managed: map[int64]service.AccountAutoFastManaged{}}, err: tc.err}
			h := &AccountHandler{accountAutoFast: service.NewAccountAutoFastService(store, nil, &autoFastHandlerGroups{})}
			if tc.unavailable {
				h.accountAutoFast = nil
			}
			r := gin.New()
			r.GET("/auto-fast", h.GetAccountAutoFast)
			r.PUT("/auto-fast", h.UpdateAccountAutoFast)
			if tc.method == "" {
				tc.method = "PUT"
			}
			if tc.query == "" {
				tc.query = "7"
			}
			req := httptest.NewRequest(tc.method, "/auto-fast?group_id="+tc.query, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			if tc.restricted {
				req = req.WithContext(adminaccess.WithPrincipal(req.Context(), &adminaccess.Principal{}))
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "private db credential")
			if tc.status == http.StatusOK {
				require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			}
			if tc.name == "save multiple accounts" {
				require.True(t, store.state.Rules[7].Config.RequireMultipleAccounts)
				require.Contains(t, rec.Body.String(), `"require_multiple_accounts":true`)
			}
		})
	}
}
