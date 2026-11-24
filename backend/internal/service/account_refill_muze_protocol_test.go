//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMuzeHTTPProtocol(t *testing.T) {
	ctx := context.Background()
	j := AccountRefillJob{ID: "d8bd88cd-b4a3-5637-92b3-171cf37587c3", Plan: "pro20x", DeliveryGroupIDs: []int64{2, 5}}
	posts := 0
	list := []map[string]any{{"id": "unrelated", "requestId": "other", "product": j.Plan}, {"id": "order-1", "requestId": j.ID, "product": j.Plan, "requestIndex": 1, "requestCount": 1}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+muzeFixtureToken, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/products":
			fmt.Fprint(w, `{"products":[{"id":"pro20x","mode":"standard","remaining":20,"cardStock":0},{"id":"pro20xv2","mode":"studio","remaining":20,"cardStock":4},{"id":"pro5x","remaining":3,"cardStock":10}]}`)
		case "/connections":
			fmt.Fprint(w, `{"connections":[{"id":"c1","isDefault":true,"enabled":true,"provider":"sub2api"}]}`)
		case "/orders":
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"orders": list})
				return
			}
			require.Equal(t, "POST", r.Method)
			posts++
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, map[string]any{"requestId": j.ID, "connectionId": "c1", "product": j.Plan, "count": float64(1), "groups": []any{float64(2), float64(5)}}, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"requestId": j.ID, "orders": []any{list[1]}, "order": list[1]})
		case "/orders/order-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"order": map[string]any{"id": "order-1", "product": j.Plan, "requestId": j.ID, "refundDueAt": "2026-11-24T16:15:00Z", "displayStage": "using", "pushStatus": "pushed", "oauth": map[string]string{"accountId": "upstream-id", "accessToken": "DO-NOT-PERSIST"}}})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p := newHTTPMuzeProvider()
	p.base = server.URL
	products, err := p.Products(ctx, muzeFixtureToken)
	require.NoError(t, err)
	require.Len(t, products, 2)
	require.Zero(t, products[0].AvailableNow)
	require.Equal(t, 3, products[1].AvailableNow)
	j.MuzeConnectionID, err = p.ResolveConnection(ctx, muzeFixtureToken, "")
	require.NoError(t, err)
	require.Equal(t, "c1", j.MuzeConnectionID)
	order, err := p.Order(ctx, muzeFixtureToken, j)
	require.NoError(t, err)
	require.Equal(t, "order-1", order.ID)
	detail, err := p.FindOrder(ctx, muzeFixtureToken, j)
	require.NoError(t, err)
	require.Equal(t, "upstream-id", detail.Subject)
	require.Empty(t, detail.AccountID)
	require.Equal(t, "2026-11-25 00:15", time.Unix(*detail.expiresAt(), 0).In(refillTimezone).Format("2006-01-02 15:04"))
	raw, err := json.Marshal(detail)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "DO-NOT-PERSIST")
	list = append(list, list[1])
	_, err = p.FindOrder(ctx, muzeFixtureToken, j)
	require.Error(t, err)
	require.Equal(t, 1, posts)
}

func TestMuzeHTTPUncertainOrderUsesOnlyReads(t *testing.T) {
	for _, tc := range []struct {
		code     int
		business string
		want     string
	}{{400, "quota_unavailable", "failed"}, {409, "stock_unavailable", "blocked"}, {409, "request_id_conflict", "blocked"}, {429, "rate_limited", "blocked"}, {503, "order_result_pending", "blocked"}, {200, "", "blocked"}} {
		t.Run(fmt.Sprintf("%d-%s", tc.code, tc.business), func(t *testing.T) {
			s, store, _, _ := muzeHarness(t)
			ctx := context.Background()
			require.NoError(t, s.Manual(ctx, muzeRequest()))
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "POST":
					posts++
					w.WriteHeader(tc.code)
					_ = json.NewEncoder(w).Encode(map[string]string{"code": tc.business, "error": "test error"})
				case r.URL.Path == "/products":
					fmt.Fprint(w, `{"products":[{"id":"pro20x","remaining":1,"cardStock":1}]}`)
				default:
					fmt.Fprint(w, `{"orders":[]}`)
				}
			}))
			defer srv.Close()
			p := newHTTPMuzeProvider()
			p.base = srv.URL
			s.muze = p
			require.NoError(t, s.run(ctx, false))
			refillDue(t, store)
			require.NoError(t, s.run(ctx, false))
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Equal(t, tc.want, state.Jobs[0].Status)
			require.Equal(t, 1, posts)
		})
	}
}
