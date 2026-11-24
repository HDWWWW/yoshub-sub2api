//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func muzeStudioCatalog() []MuzeProduct {
	return []MuzeProduct{
		{Product: "pro20x", Plan: "pro20x", Mode: "standard", Closed: true, Remaining: 20},
		{Product: "pro5x", Plan: "pro5x", Mode: "standard", Closed: true},
		{Product: "pro20xv2", Plan: "pro20x", Mode: "studio", AvailableNow: 5, Remaining: 20, Stock: 5},
		{Product: "pro5xv2", Plan: "pro5x", Mode: "studio", Remaining: 0, Stock: 3},
	}
}

func TestMuzeStockPerSKUQuotaPerTier(t *testing.T) {
	products := muzeStudioCatalog()
	held := newMuzeHolds()
	// Previous release reserved an unversioned standard SKU. It consumes the
	// shared quota but no V2 stock. Prefer standard when that SKU becomes available.
	legacy := AccountRefillJob{Plan: "pro20x"}
	held.add(legacy.Plan, legacy.muzeSKU())
	products[0].Closed = false
	products[0].Stock, products[0].AvailableNow = 17, 17
	plans := slices.Repeat([]string{"pro20x"}, 19)
	_, ok := allocateMuzeProducts(products, append(slices.Clone(plans), "pro20x"), false, held)
	require.False(t, ok, "two SKUs must not double a shared 20-unit quota")
	held = newMuzeHolds()
	held.add(legacy.Plan, legacy.muzeSKU())
	batch, ok := allocateMuzeProducts(products, plans, false, held)
	require.True(t, ok)
	require.Len(t, batch, 19)
	for i, q := range batch {
		if i < 16 {
			require.Equal(t, "pro20x", q.muzeProductID)
		} else {
			require.Equal(t, "pro20xv2", q.muzeProductID)
			require.True(t, q.muzeStudio)
		}
	}
}

func TestMuzeManualPinsSKUAcrossReceiptsAndGroups(t *testing.T) {
	s, base, p, _ := muzeHarness(t)
	ctx := context.Background()
	p.products = muzeStudioCatalog()
	p.products[0].Closed = false
	p.products[0].Stock, p.products[0].Remaining, p.products[0].AvailableNow = 5, 1, 1
	p.products[2].Remaining, p.products[2].AvailableNow = 1, 1
	s.store = &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	child, err := s.ForGroup(ctx, 3)
	require.NoError(t, err)
	v, err := child.View(ctx)
	require.NoError(t, err)
	require.NoError(t, child.Configure(ctx, v.Config, "", 0))
	r := muzeRequest()
	r.Plan = "pro20xv2"
	require.NoError(t, s.Manual(ctx, r))
	require.NoError(t, s.Manual(ctx, r))
	r.Plan = "pro20x"
	require.ErrorContains(t, s.Manual(ctx, r), "同一请求")
	other := muzeRequest()
	other.GroupID = 3
	require.ErrorContains(t, child.Manual(ctx, other), "本地预占")
	state, err := decodeAccountRefillState(base.raw)
	require.NoError(t, err)
	j := state.Jobs[0]
	require.Equal(t, "pro20xv2", j.MuzeProductID)
	require.Equal(t, "pro20x", j.Plan)
	require.True(t, j.MuzeStudio)
	require.Zero(t, p.orders)
	// Even with a sibling SKU in stock, an explicit manual order cannot substitute it.
	p.products[0].Closed = true
	other = muzeRequest()
	other.GroupID = 3
	require.ErrorContains(t, child.Manual(ctx, other), "库存或次数不足")
}

func TestMuzePrimaryBackupAndDailyPinStudioSKU(t *testing.T) {
	ctx := context.Background()
	for _, route := range []string{"primary", "backup", "daily"} {
		t.Run(route, func(t *testing.T) {
			s, store, p, wallet, _ := reverseRefillHarness(t)
			p.products = muzeStudioCatalog()
			if route == "backup" {
				store.mutate(t, func(v *accountRefillState) {
					v.Config.PrimaryProvider = "1208"
					v.Jobs = []AccountRefillJob{fallbackPrimaryJob("primary", "failed", time.Now().Add(-time.Minute))}
				})
			}
			if route == "daily" {
				seedDailyRefill(t, store, time.Now().Truncate(time.Minute), 1)
			}
			require.NoError(t, s.run(ctx, false))
			v, err := s.View(ctx)
			require.NoError(t, err)
			require.NotEmpty(t, v.Jobs)
			j := v.Jobs[0]
			require.Equal(t, "pro20x", j.Plan)
			require.Equal(t, "pro20xv2", j.MuzeProductID)
			require.True(t, j.MuzeStudio)
			require.Empty(t, wallet.orders)
			if route == "backup" {
				require.Equal(t, "primary", j.FallbackForJobID)
			}
			if route == "daily" {
				require.NotEmpty(t, j.DailyBatchID)
			}
		})
	}
}

func TestMuzeStudioReceiptAndReadOnlyRecovery(t *testing.T) {
	ctx := context.Background()
	const sku = "pro20xv2"
	j := AccountRefillJob{ID: "d8bd88cd-b4a3-5637-92b3-171cf37587c3", Plan: "pro20x", MuzeProductID: sku, MuzeStudio: true, MuzeConnectionID: "c1"}
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`{"order":{"id":"o1","refundDueAt":"2026-11-24T16:15:00Z"},"product":{"id":"pro20xv2"}}`, true},
		{`{"order":{"id":"o1","product":"pro20x"},"product":{"id":"pro20xv2"}}`, false},
		{`{"order":{"id":"o1","product":"pro20xv2"},"product":{"id":"pro5xv2"}}`, false},
		{`{"order":{"id":"o1"}}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/orders", r.URL.Path)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, sku, body["product"])
			require.Equal(t, 1., body["count"])
			require.Equal(t, "c1", body["connectionId"])
			require.NotContains(t, body, "requestId")
			fmt.Fprint(w, tc.body)
		}))
		p := newHTTPMuzeProvider()
		p.base = srv.URL
		o, err := p.Order(ctx, muzeFixtureToken, j)
		srv.Close()
		if tc.valid {
			require.NoError(t, err)
			require.NotNil(t, o.ExpiresAt)
		} else {
			require.Error(t, err)
		}
	}

	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprintf("persist-failure-%t", crash), func(t *testing.T) {
			s, store, p, _ := muzeHarness(t)
			p.products = muzeStudioCatalog()
			p.orderError = errors.New("receipt lost")
			p.failAfterPost = crash
			r := muzeRequest()
			r.Plan = sku
			require.NoError(t, s.Manual(ctx, r))
			err := s.run(ctx, false)
			if crash {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			store.failSave = false
			for i := 0; i < 2; i++ {
				refillDue(t, store)
				require.NoError(t, s.run(ctx, false))
			}
			require.Equal(t, 1, p.orders)
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			task := state.Jobs[0]
			require.Equal(t, "blocked", task.Status)
			require.False(t, refillJobTimedOut(state.Config, task, time.Now().Add(2*time.Hour)))
			require.Error(t, s.Retry(ctx, task.ID))
			original := *p.cycle
			require.Empty(t, original.RequestID)
			for _, bad := range []func(*muzeCycle){func(c *muzeCycle) { c.Product = "pro20x" }, func(c *muzeCycle) { c.PushConfigID = "other" }, func(c *muzeCycle) { c.PushConfigID = "" }, func(c *muzeCycle) { c.CreatedAt = task.SubmissionStartedAt.Add(-time.Hour).Format(time.RFC3339) }, func(c *muzeCycle) { c.CreatedAt = task.SubmissionStartedAt.Add(time.Hour).Format(time.RFC3339) }} {
				mismatch := original
				bad(&mismatch)
				p.cycle = &mismatch
				require.Error(t, s.ReconcileMuze(ctx, task.ID, original.ID))
			}
			p.cycle = &original
			require.NoError(t, s.ReconcileMuze(ctx, task.ID, original.ID))
			v, err := s.View(ctx)
			require.NoError(t, err)
			require.Equal(t, original.ID, v.Jobs[0].SupplierID)
			require.Equal(t, 1, p.orders)
		})
	}
}

func TestMuzeCatalogReadsOnlyAndPreservesEmptyList(t *testing.T) {
	s, store, p, _ := muzeHarness(t)
	p.products = muzeStudioCatalog()
	ctx := context.Background()
	cat, err := s.MuzeCatalog(ctx)
	require.NoError(t, err)
	require.Len(t, cat.Products, 4)
	require.Equal(t, 5, cat.Products[2].Stock)
	p.products = []MuzeProduct{}
	cat, err = s.MuzeCatalog(ctx)
	require.NoError(t, err)
	require.Len(t, cat.Products, 4, "coalesces concurrent picker loads")
	store.mutate(t, func(v *accountRefillState) { v.Muze.CheckedAt = nil })
	cat, err = s.MuzeCatalog(ctx)
	require.NoError(t, err)
	require.NotNil(t, cat.Products)
	require.Empty(t, cat.Products)
	require.Zero(t, p.orders)
}
