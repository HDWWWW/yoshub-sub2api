//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type refillContractProvider struct {
	*refillTestProvider
	response         *RefillProviderJob
	statusErr        error
	deliveryErr      error
	statusCalls      int
	deliveryOverride *RefillDelivery
	overrideDelivery bool
}

func (p *refillContractProvider) Status(context.Context, string, string) (*RefillProviderJob, error) {
	p.statusCalls++
	return p.response, p.statusErr
}

func (p *refillContractProvider) Delivery(ctx context.Context, token, id string) (*RefillDelivery, error) {
	if p.overrideDelivery {
		p.deliveryCalls++
		return p.deliveryOverride, p.deliveryErr
	}
	if p.deliveryErr != nil {
		p.deliveryCalls++
		return nil, p.deliveryErr
	}
	return p.refillTestProvider.Delivery(ctx, token, id)
}

func refillContractHarness(t *testing.T) (*AccountRefillService, *refillTestStore, *refillContractProvider, *refillTestAccounts, accountRefillState) {
	t.Helper()
	s, store, base, accounts := refillHarness(t)
	require.NoError(t, s.run(context.Background(), false))
	p := &refillContractProvider{refillTestProvider: base, response: &RefillProviderJob{ID: "supplier-1", Status: "processing"}}
	s.provider = p
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	return s, store, p, accounts, state
}

func advanceRefillContract(t *testing.T, s *AccountRefillService, state *accountRefillState) {
	t.Helper()
	require.NoError(t, s.advance(context.Background(), "rst_fixture", state, &state.Jobs[0], func() error { return nil }))
}

func TestRefillDocumentedStatesAndUnknownTypes(t *testing.T) {
	for _, status := range []string{"queued", "processing", "succeeded", "failed", "review", "cancelled", "banned", "held", "charged", "released", "future_status", ""} {
		t.Run(status, func(t *testing.T) {
			s, _, p, _, state := refillContractHarness(t)
			p.response.Status = status
			p.response.ResultReady = true // Never trust this flag without succeeded.
			p.response.Error = "公开状态说明"
			advanceRefillContract(t, s, &state)
			j := state.Jobs[0]
			require.Equal(t, status, j.SupplierStatus)
			require.Equal(t, "公开状态说明", j.SupplierError)
			want := status
			if status == "succeeded" {
				want = "awaiting_delivery"
			}
			if !refillProviderStatusKnown(status) {
				want = "supplier_unknown"
			}
			require.Equal(t, want, j.Status)
			require.Equal(t, status == "failed", j.BudgetReleased)
			require.Equal(t, status == "failed", j.ReplacementPending)
			require.Equal(t, status == "failed", j.FinishedAt != nil)
			require.Zero(t, p.deliveryCalls)
			require.Len(t, p.submitted, 1)
			if status != "failed" {
				require.Equal(t, 60.0, state.Usage[j.CreatedAt.In(refillTimezone).Format("2006-01-02")].Points)
			}
			if want == "supplier_unknown" || status == "review" {
				config := state.Config
				config.MinReady, config.MaxInFlight = 5, 5
				require.Zero(t, evaluateAccountRefill(config, nil, nil, state.Jobs, time.Now()).Needed)
				p.response.Status = "processing"
				advanceRefillContract(t, s, &state)
				require.Equal(t, "processing", state.Jobs[0].Status)
				require.Equal(t, 2, p.statusCalls)
				require.Len(t, p.submitted, 1)
			}
		})
	}
}

func TestRefillRejectsMismatchedJobTypesWithoutReleasingBudget(t *testing.T) {
	for _, mutate := range []func(*RefillProviderJob){
		func(r *RefillProviderJob) { r.ID = "another-order" },
		func(r *RefillProviderJob) { r.Service = "recharge" },
		func(r *RefillProviderJob) { r.Service = "refund" },
		func(r *RefillProviderJob) { r.Plan = "pro20x" },
		func(r *RefillProviderJob) { r.Format = "cpa" },
		func(r *RefillProviderJob) { n := 2; r.Quantity = &n },
		func(r *RefillProviderJob) { n := int64(-1); r.ExpiresAt = &n },
	} {
		s, _, p, _, state := refillContractHarness(t)
		p.response.Status = "failed"
		mutate(p.response)
		advanceRefillContract(t, s, &state)
		require.Equal(t, "supplier_unknown", state.Jobs[0].Status)
		require.Equal(t, "supplier-1", state.Jobs[0].SupplierID)
		require.False(t, state.Jobs[0].BudgetReleased)
		require.False(t, state.Jobs[0].ReplacementPending)
	}
}

func TestRefillDeliveryErrorsKeepOriginalOrderAndBilling(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{401, "import_failed"}, {402, "import_failed"}, {403, "import_failed"}, {404, "import_failed"},
		{409, "awaiting_delivery"}, {410, "delivery_expired"}, {413, "import_failed"}, {422, "import_failed"},
		{429, "awaiting_delivery"}, {500, "awaiting_delivery"}, {503, "awaiting_delivery"}, {0, "awaiting_delivery"},
	} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			s, _, p, accounts, state := refillContractHarness(t)
			j := &state.Jobs[0]
			j.SupplierStatus, j.ResultReady = "succeeded", true
			p.deliveryErr = &refillProviderError{code: tc.code, retryAfter: 120 * time.Second}
			if tc.code == 0 {
				p.deliveryErr = &refillProviderTemporaryError{"connection interrupted"}
			}
			advanceRefillContract(t, s, &state)
			require.Equal(t, tc.want, j.Status)
			require.False(t, j.BudgetReleased)
			require.False(t, j.ReplacementPending)
			require.Zero(t, accounts.creates)
			require.Len(t, p.submitted, 1)
			if tc.code == 429 {
				require.Greater(t, time.Until(state.ProviderRetryAt), 119*time.Second)
			}
			if tc.want == "awaiting_delivery" {
				require.False(t, j.ResultReady)
				p.deliveryErr = nil
				p.response.Status, p.response.ResultReady = "succeeded", true
				advanceRefillContract(t, s, &state)
				advanceRefillContract(t, s, &state)
				require.Equal(t, "imported", j.Status)
				require.Equal(t, 1, accounts.creates)
				require.Len(t, p.submitted, 1)
			}
		})
	}
}

func TestRefillSucceededButExpiredIsChargedAndNotImported(t *testing.T) {
	for _, ready := range []bool{false, true} {
		s, _, p, _, state := refillContractHarness(t)
		expiry := time.Now().Add(-time.Second).Unix()
		p.response.Status, p.response.ResultReady, p.response.ExpiresAt = "succeeded", ready, &expiry
		advanceRefillContract(t, s, &state)
		require.Equal(t, "delivery_expired", state.Jobs[0].Status)
		require.False(t, state.Jobs[0].BudgetReleased)
		require.False(t, state.Jobs[0].ReplacementPending)
		require.NotNil(t, state.Jobs[0].FinishedAt)
		require.Zero(t, p.deliveryCalls)
	}
}

func TestRefillDownloadPreservesCompleteSupplierJSONWithoutSideEffects(t *testing.T) {
	s, store, p, accounts, state := refillContractHarness(t)
	expiry := time.Now().Add(time.Hour).Unix()
	j := &state.Jobs[0]
	j.Status, j.SupplierStatus, j.ResultReady, j.ExpiresAt = "imported", "succeeded", true, &expiry
	require.NoError(t, saveRefillState(&state, func(v string) error { store.raw = v; return nil }))
	before := store.raw
	delivery := fmt.Sprintf(`{"type":"sub2api-data","version":1,"exported_at":"2026-11-02T00:00:00Z","proxies":[],"future_field":{"value":1234567890123456789},"accounts":[{"name":"fixture","platform":"openai","type":"oauth","credentials":{"access_token":"fixture-access","refresh_token":"fixture-refresh","expires_at":2000000000},"extra":{"preserved":true},"expires_at":%d,"auto_pause_on_expired":true}]}`, expiry)
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer rst_fixture", r.Header.Get("Authorization"))
		requests = append(requests, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/delivery") {
			_, _ = w.Write([]byte(delivery))
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"supplier-1","status":"succeeded","service":"refill","format":"sub2api","plan":"pro5x","quantity":1,"price":60,"result_ready":true,"expires_at":%d}`, expiry)
	}))
	defer server.Close()
	provider := newHTTPRefillProvider()
	provider.base = server.URL
	s.provider = provider
	data, err := s.DownloadDelivery(context.Background(), j.ID)
	require.NoError(t, err)
	require.JSONEq(t, delivery, string(data))
	require.Contains(t, string(data), "1234567890123456789")
	require.Equal(t, []string{"/refills/supplier-1", "/refills/supplier-1/delivery"}, requests)
	require.Equal(t, before, store.raw)
	require.NotContains(t, store.raw, "fixture-access")
	require.Zero(t, accounts.creates)
	require.Len(t, p.submitted, 1)
}

func TestRefillDownloadEligibilityAndProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		mutate func(*accountRefillState, *refillContractProvider)
	}{
		{"unknown local order", 404, func(st *accountRefillState, _ *refillContractProvider) { st.Jobs[0].ID = "other" }},
		{"not accepted", 409, func(st *accountRefillState, _ *refillContractProvider) { st.Jobs[0].SupplierID = "" }},
		{"queued", 409, func(st *accountRefillState, _ *refillContractProvider) { st.Jobs[0].SupplierStatus = "queued" }},
		{"not ready", 409, func(st *accountRefillState, _ *refillContractProvider) { st.Jobs[0].ResultReady = false }},
		{"expired", 410, func(st *accountRefillState, _ *refillContractProvider) {
			n := time.Now().Add(-time.Second).Unix()
			st.Jobs[0].ExpiresAt = &n
		}},
		{"zero expiry", 410, func(st *accountRefillState, _ *refillContractProvider) { n := int64(0); st.Jobs[0].ExpiresAt = &n }},
		{"backoff", 429, func(st *accountRefillState, _ *refillContractProvider) {
			st.ProviderRetryAt = time.Now().Add(time.Minute)
		}},
		{"no token", 400, func(st *accountRefillState, _ *refillContractProvider) { st.EncryptedToken = "" }},
		{"supplier review", 409, func(_ *accountRefillState, p *refillContractProvider) { p.response.Status = "review" }},
		{"supplier unknown", 409, func(_ *accountRefillState, p *refillContractProvider) { p.response.Status = "cancelled" }},
		{"supplier expired", 410, func(_ *accountRefillState, p *refillContractProvider) {
			n := time.Now().Add(-time.Second).Unix()
			p.response.ExpiresAt = &n
		}},
		{"wrong order", 502, func(_ *accountRefillState, p *refillContractProvider) { p.response.ID = "other" }},
		{"wrong service", 502, func(_ *accountRefillState, p *refillContractProvider) { p.response.Service = "recharge" }},
		{"delivery unavailable", 409, func(_ *accountRefillState, p *refillContractProvider) {
			p.deliveryErr = &refillProviderError{code: 409}
		}},
		{"delivery expired", 410, func(_ *accountRefillState, p *refillContractProvider) {
			p.deliveryErr = &refillProviderError{code: 410}
		}},
		{"supplier auth", 502, func(_ *accountRefillState, p *refillContractProvider) { p.statusErr = &refillProviderError{code: 401} }},
		{"export denied", 502, func(_ *accountRefillState, p *refillContractProvider) {
			p.deliveryErr = &refillProviderError{code: 403}
		}},
		{"rate limited", 429, func(_ *accountRefillState, p *refillContractProvider) {
			p.deliveryErr = &refillProviderError{code: 429, retryAfter: time.Minute}
		}},
		{"unavailable", 502, func(_ *accountRefillState, p *refillContractProvider) {
			p.deliveryErr = &refillProviderError{code: 503}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store, p, _, state := refillContractHarness(t)
			id := state.Jobs[0].ID
			state.Jobs[0].SupplierStatus, state.Jobs[0].ResultReady = "succeeded", true
			p.response.Status, p.response.ResultReady = "succeeded", true
			tc.mutate(&state, p)
			require.NoError(t, saveRefillState(&state, func(v string) error { store.raw = v; return nil }))
			before := store.raw
			data, err := s.DownloadDelivery(context.Background(), id)
			var failure *AccountRefillDownloadError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, tc.code, failure.StatusCode)
			require.Empty(t, data)
			if tc.name == "rate limited" {
				after, err := decodeAccountRefillState(store.raw)
				require.NoError(t, err)
				require.True(t, after.ProviderRetryAt.After(time.Now()))
				require.Equal(t, state.Jobs, after.Jobs)
			} else {
				require.Equal(t, before, store.raw)
			}
			require.Len(t, p.submitted, 1)
		})
	}
}

func TestRefillHTTPErrorClassificationAndMalformedDelivery(t *testing.T) {
	for _, code := range []int{401, 402, 403, 404, 409, 410, 413, 422, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"detail":"private-input-must-not-leak"}`))
			}))
			defer server.Close()
			p := newHTTPRefillProvider()
			p.base = server.URL
			_, err := p.Delivery(context.Background(), "rst_fixture", "supplier-1")
			var remote *refillProviderError
			require.ErrorAs(t, err, &remote)
			require.Equal(t, code, remote.code)
			require.NotContains(t, err.Error(), "private-input")
			require.Equal(t, 120*time.Second, remote.retryAfter)
		})
	}
	for _, invalid := range []string{`{`, `{"accounts":"invalid"}`} {
		var delivery RefillDelivery
		require.Error(t, json.Unmarshal([]byte(invalid), &delivery))
	}
}
