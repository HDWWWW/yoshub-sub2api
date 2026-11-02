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

func TestRefillReviewRecoversCommittedImportAfterLeaseExpires(t *testing.T) {
	s, _, p, accounts, state := refillContractHarness(t)
	j := &state.Jobs[0]
	j.SupplierStatus, j.ResultReady, j.Status = "succeeded", true, "importing"
	// Account creation and activation committed, but persisting the job failed.
	require.NoError(t, s.importDelivery(context.Background(), "rst_fixture", j))
	accountID := j.AccountID
	j.AccountID = 0
	expired := time.Now().Add(-time.Second).Unix()
	j.ExpiresAt = &expired
	advanceRefillContract(t, s, &state)
	require.Equal(t, "imported", j.Status)
	require.Equal(t, accountID, j.AccountID)
	require.Equal(t, 1, accounts.creates)
	require.Equal(t, 1, p.deliveryCalls, "recovery must use the existing account, not redownload an expired delivery")
	require.False(t, j.BudgetReleased)
}

func TestRefillReviewExpiredReadyDeliveryDoesNotCreateAccount(t *testing.T) {
	s, _, p, accounts, state := refillContractHarness(t)
	j := &state.Jobs[0]
	j.SupplierStatus, j.ResultReady = "succeeded", true
	expired := time.Now().Add(-time.Second).Unix()
	j.ExpiresAt = &expired
	advanceRefillContract(t, s, &state)
	require.Equal(t, "delivery_expired", j.Status)
	require.Zero(t, accounts.creates)
	require.Zero(t, p.deliveryCalls)
	require.False(t, j.BudgetReleased)
}

func TestRefillReviewRejectsIncompatibleDeliveryDocuments(t *testing.T) {
	for _, root := range []string{`"type":"cpa",`, `"version":999,`, `"type":"refund",`} {
		t.Run(root, func(t *testing.T) {
			s, store, _, accounts, state := refillContractHarness(t)
			j := &state.Jobs[0]
			j.SupplierStatus, j.ResultReady = "succeeded", true
			require.NoError(t, saveRefillState(&state, func(v string) error { store.raw = v; return nil }))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/delivery") {
					fmt.Fprintf(w, `{%s"accounts":[{"platform":"openai","type":"oauth","credentials":{"access_token":"fixture"},"expires_at":%d}]}`, root, time.Now().Add(time.Hour).Unix())
				} else {
					fmt.Fprint(w, `{"id":"supplier-1","status":"succeeded","result_ready":true}`)
				}
			}))
			defer server.Close()
			provider := newHTTPRefillProvider()
			provider.base = server.URL
			s.provider = provider
			data, err := s.DownloadDelivery(context.Background(), j.ID)
			require.Error(t, err)
			require.Empty(t, data)
			advanceRefillContract(t, s, &state)
			require.Equal(t, "import_failed", j.Status)
			require.Zero(t, accounts.creates)
			require.False(t, j.BudgetReleased)
		})
	}
}

func TestRefillReviewRejectsNilOrBlankCredentials(t *testing.T) {
	// A provider implementation returning nil must not panic in the importer.
	var delivery RefillDelivery
	require.NoError(t, json.Unmarshal([]byte(`{"accounts":[{"platform":"openai","type":"oauth","credentials":{"access_token":"  "},"expires_at":4000000000}]}`), &delivery))
	s, _, p, _, state := refillContractHarness(t)
	p.deliveryOverride, p.overrideDelivery = &delivery, true
	state.Jobs[0].SupplierStatus, state.Jobs[0].ResultReady = "succeeded", true
	advanceRefillContract(t, s, &state)
	require.Equal(t, "import_failed", state.Jobs[0].Status)
	p.deliveryOverride = nil
	advanceRefillContract(t, s, &state)
	require.Equal(t, "import_failed", state.Jobs[0].Status)
}

func TestRefillReviewDownloadRequiresImporterCompatibleProxyList(t *testing.T) {
	s, store, p, _, state := refillContractHarness(t)
	j := &state.Jobs[0]
	j.SupplierStatus, j.ResultReady = "succeeded", true
	p.response.Status, p.response.ResultReady = "succeeded", true
	require.NoError(t, saveRefillState(&state, func(v string) error { store.raw = v; return nil }))
	_, err := s.DownloadDelivery(context.Background(), j.ID)
	var failure *AccountRefillDownloadError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, 502, failure.StatusCode)
	require.Contains(t, failure.Error(), "代理列表")
}

func TestRefillReviewDownloadRateLimitSurvivesRepeatedClicksAndRestart(t *testing.T) {
	for _, phase := range []string{"status", "delivery"} {
		t.Run(phase, func(t *testing.T) {
			s, store, p, _, state := refillContractHarness(t)
			j := &state.Jobs[0]
			j.SupplierStatus, j.ResultReady = "succeeded", true
			p.response.Status, p.response.ResultReady = "succeeded", true
			rateLimit := &refillProviderError{code: 429, retryAfter: 120 * time.Second}
			if phase == "status" {
				p.statusErr = rateLimit
			} else {
				p.deliveryErr = rateLimit
			}
			require.NoError(t, saveRefillState(&state, func(v string) error { store.raw = v; return nil }))
			_, err := s.DownloadDelivery(context.Background(), j.ID)
			var failure *AccountRefillDownloadError
			require.ErrorAs(t, err, &failure)
			require.Equal(t, 429, failure.StatusCode)
			persisted, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Greater(t, time.Until(persisted.ProviderRetryAt), 119*time.Second)
			require.Equal(t, state.Jobs, persisted.Jobs, "backoff must not change the order or its budget")
			statusCalls, deliveryCalls := p.statusCalls, p.deliveryCalls
			restarted := &AccountRefillService{store: store, provider: p, encryptor: s.encryptor}
			for i := 0; i < 3; i++ {
				_, err = restarted.DownloadDelivery(context.Background(), j.ID)
				require.ErrorAs(t, err, &failure)
				require.Equal(t, 429, failure.StatusCode)
			}
			require.Equal(t, statusCalls, p.statusCalls)
			require.Equal(t, deliveryCalls, p.deliveryCalls)
		})
	}
}
