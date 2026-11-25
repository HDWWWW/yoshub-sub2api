//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type refill558669Fixture struct {
	*refillTestProvider
	quoteVersion int64
	quotePrice   float64
	available    float64
	disabled     bool
	ready        bool
	version      int64
	entitlement  string
	deliveryErr  error
	statusErr    error
	statusIDs    []string
	tokens       []string
	statusJob    *RefillProviderJob
}

func (p *refill558669Fixture) Catalog(_ context.Context, token string) (*RefillCatalog, error) {
	p.tokens = append(p.tokens, token)
	return &RefillCatalog{Available: p.available, Reserved: 20, Items: []RefillQuote{
		{Service: "refill", Plan: "pro5x", Price: p.quotePrice, PriceVersion: p.quoteVersion, Enabled: !p.disabled},
		{Service: "refill", Plan: "pro20x", Price: p.quotePrice, PriceVersion: p.quoteVersion, Enabled: !p.disabled},
	}}, nil
}
func (p *refill558669Fixture) result(j AccountRefillJob) *RefillProviderJob {
	n := 1
	lease := time.Now().Add(time.Hour).Unix()
	return &RefillProviderJob{ID: "558-task", Service: "refill", Plan: j.Plan, Format: "sub2api", Quantity: &n,
		Price: j.Price, PriceVersion: j.PriceVersion, Status: p.status, ResultReady: p.ready,
		ExpiresAt: &lease, DeliveryVersion: p.version, EntitlementStatus: p.entitlement, Email: "fixture@example.test"}
}
func (p *refill558669Fixture) Submit(_ context.Context, token string, j AccountRefillJob) (*RefillProviderJob, error) {
	p.tokens = append(p.tokens, token)
	state, err := decodeAccountRefillState(p.store.raw)
	require.NoError(p.t, err)
	require.NotNil(p.t, state.Jobs[0].SubmissionStartedAt, "POST must follow a committed immutable request")
	require.Equal(p.t, j.PriceVersion, state.Jobs[0].PriceVersion)
	p.submitted = append(p.submitted, j)
	p.statusJob = p.result(j)
	return p.statusJob, p.submitError
}
func (p *refill558669Fixture) Status(_ context.Context, token, id string) (*RefillProviderJob, error) {
	p.tokens = append(p.tokens, token)
	p.statusIDs = append(p.statusIDs, id)
	j := *p.statusJob
	j.Status, j.ResultReady, j.DeliveryVersion, j.EntitlementStatus = p.status, p.ready, p.version, p.entitlement
	return &j, p.statusErr
}
func (p *refill558669Fixture) Delivery(ctx context.Context, token, id string) (*RefillDelivery, error) {
	p.tokens = append(p.tokens, token)
	if p.deliveryErr != nil {
		p.deliveryCalls++
		return nil, p.deliveryErr
	}
	d, err := p.refillTestProvider.Delivery(ctx, token, id)
	d.Proxies = []json.RawMessage{}
	d.Accounts[0].Credentials["email"] = "fixture@example.test"
	d.Accounts[0].Credentials["access_token"] = fmt.Sprintf("fixture-token-%d", p.version)
	return d, err
}

type refill558669Accounts struct {
	*refillTestAccounts
	syncs int
}

type refill558669Cipher struct{ refillTestEncryptor }

func (refill558669Cipher) Decrypt(value string) (string, error) { return value, nil }

func (a *refill558669Accounts) Sync558669Delivery(_ context.Context, j AccountRefillJob, d *RefillDelivery) error {
	for i := range a.accounts {
		if a.accounts[i].ID != j.AccountID {
			continue
		}
		credentials, err := refreshed558669Credentials(&a.accounts[i], j, d)
		if err != nil {
			return err
		}
		if credentials != nil {
			a.accounts[i].Credentials = credentials
			a.accounts[i].Extra["refill_delivery_version"] = j.DeliveryVersion
			a.syncs++
		}
		return nil
	}
	return ErrAccountNotFound
}

func harness558669(t *testing.T) (*AccountRefillService, *refillTestStore, *refill558669Fixture, *refill558669Accounts) {
	t.Helper()
	s, store, legacy, accounts := refillHarness(t)
	p := &refill558669Fixture{refillTestProvider: &refillTestProvider{store: store, t: t, status: "processing"}, quoteVersion: 1280, quotePrice: 12, available: 1000, version: 1, entitlement: "pending"}
	s.provider558669 = p
	s.encryptor = refill558669Cipher{}
	a := &refill558669Accounts{refillTestAccounts: accounts}
	s.admin = a
	store.mutate(t, func(v *accountRefillState) {
		v.Supplier558669.EncryptedToken = "558-fixture-token"
		v.Config.PrimaryProvider = refillProvider558669
		v.Config.Enabled, v.Config.MonitorEnabled = false, false
	})
	t.Cleanup(func() { require.Empty(t, legacy.submitted, "558669 must never use the 1208 client") })
	return s, store, p, a
}
func reserve558669(t *testing.T, s *AccountRefillService) ManualAccountRefillRequest {
	t.Helper()
	r := manualRequest()
	r.Provider = refillProvider558669
	require.NoError(t, s.Manual(context.Background(), r))
	return r
}
func read558669(t *testing.T, store *refillTestStore) accountRefillState {
	t.Helper()
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	return state
}

func TestRefill558669HTTPContractAndNativePoints(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer fixture-secret", r.Header.Get("Authorization"))
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/catalog":
			fmt.Fprint(w, `{"available":123.5,"reserved":17,"items":[{"service":"refill","plan":"pro20x","price":12.5,"price_version":1280,"enabled":true,"terms":"fixture"}]}`)
		case "/refills":
			require.Equal(t, "POST", r.Method)
			require.Equal(t, "sub2-refill-local-job", r.Header.Get("Idempotency-Key"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, map[string]any{"plan": "pro20x", "format": "sub2api", "price_version": float64(1280)}, body)
			fmt.Fprint(w, `{"id":"task-1","service":"refill","plan":"pro20x","quantity":1,"price":12.5,"price_version":1280,"format":"sub2api","status":"processing","result_ready":true,"entitlement_status":"pending","delivery_version":1,"duplicate":true,"expires_at":"2026-12-01T12:30:00+08:00"}`)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	p := newHTTP558669Provider()
	p.base = server.URL
	wallet, key, err := p.Identity(context.Background(), "fixture-secret")
	require.NoError(t, err)
	require.Nil(t, key)
	require.Equal(t, 123.5, wallet.Available)
	require.Equal(t, 140.5, wallet.Balance)
	job := AccountRefillJob{ID: "local-job", Plan: "pro20x", Price: 12.5, PriceVersion: 1280}
	remote, err := p.Submit(context.Background(), "fixture-secret", job)
	require.NoError(t, err)
	require.NoError(t, validate558669Job(remote, job))
	require.True(t, remote.Duplicate)
	want, _ := time.Parse(time.RFC3339, "2026-12-01T12:30:00+08:00")
	require.Equal(t, want.Unix(), *remote.ExpiresAt)
	require.Equal(t, []string{"GET /catalog", "POST /refills"}, paths)
}

func TestRefill558669ManualReservationAndImmutableRetry(t *testing.T) {
	s, store, p, _ := harness558669(t)
	r := reserve558669(t, s)
	require.NoError(t, s.Manual(context.Background(), r))
	require.Empty(t, p.submitted)
	p.quoteVersion, p.quotePrice = 1281, 13
	p.submitError = &refillProviderTemporaryError{"uncertain POST"}
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.submitted, 1)
	require.Equal(t, int64(1281), p.submitted[0].PriceVersion)
	p.quoteVersion, p.quotePrice = 9999, 100
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Len(t, p.submitted, 2)
	require.Equal(t, p.submitted[0].ID, p.submitted[1].ID)
	require.Equal(t, p.submitted[0].PriceVersion, p.submitted[1].PriceVersion)
	state := read558669(t, store)
	day := time.Now().In(refillTimezone).Format("2006-01-02")
	require.Len(t, state.Jobs, 1)
	require.Equal(t, 13.0, state.Supplier558669.Usage[day].Points)
	require.Zero(t, state.Usage[day].Points)
	require.Equal(t, 1, state.Usage[day].Count)
	for _, token := range p.tokens {
		require.Equal(t, "558-fixture-token", token)
	}
}

func TestRefill558669NoPOSTBeforeDurableSave(t *testing.T) {
	s, store, p, _ := harness558669(t)
	reserve558669(t, s)
	state := read558669(t, store)
	saves := 0
	err := s.advance558669(context.Background(), &state, &state.Jobs[0], func() error {
		saves++
		if saves == 2 {
			return errors.New("disk full")
		}
		return nil
	})
	require.Error(t, err)
	require.Empty(t, p.submitted)
}

func TestRefill558669ReadyBeforeEntitlementAndVersionedImport(t *testing.T) {
	s, store, p, a := harness558669(t)
	reserve558669(t, s)
	p.ready = true
	require.NoError(t, s.run(context.Background(), false))
	state := read558669(t, store)
	require.Equal(t, "imported", state.Jobs[0].Status)
	require.Equal(t, "pending", state.Jobs[0].EntitlementStatus)
	require.Equal(t, int64(1), state.Jobs[0].AppliedDeliveryVersion)
	require.Equal(t, 1, a.creates)
	a.accounts[0].Status = "inactive"
	a.accounts[0].Concurrency = 99
	proxy := int64(77)
	a.accounts[0].ProxyID = &proxy
	a.accounts[0].Credentials["model_mapping"] = map[string]string{"custom": "keep"}
	expires := a.accounts[0].ExpiresAt
	p.version, p.entitlement = 2, "confirmed"
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, a.creates)
	require.Equal(t, 1, a.syncs)
	require.Equal(t, "fixture-token-2", a.accounts[0].Credentials["access_token"])
	require.Equal(t, map[string]string{"custom": "keep"}, a.accounts[0].Credentials["model_mapping"])
	require.Equal(t, "inactive", a.accounts[0].Status)
	require.Equal(t, expires, a.accounts[0].ExpiresAt)
	require.Equal(t, &proxy, a.accounts[0].ProxyID)
	require.Equal(t, 99, a.accounts[0].Concurrency)
	// A state-save crash after credential commit cannot apply the same version twice.
	store.mutate(t, func(v *accountRefillState) { v.Jobs[0].AppliedDeliveryVersion = 1 })
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, a.syncs)
	require.Equal(t, int64(2), read558669(t, store).Jobs[0].AppliedDeliveryVersion)
	require.NotContains(t, store.raw, "fixture-token-")
}

func TestRefill558669DeliveryErrorsAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		code   int
		status string
	}{{409, "awaiting_delivery"}, {410, "delivery_expired"}, {429, "awaiting_delivery"}, {503, "awaiting_delivery"}, {401, "blocked"}} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			s, store, p, a := harness558669(t)
			reserve558669(t, s)
			p.ready = true
			p.deliveryErr = &refillProviderError{code: tc.code, retryAfter: 3 * time.Minute}
			require.NoError(t, s.run(context.Background(), false))
			v := read558669(t, store)
			require.Equal(t, tc.status, v.Jobs[0].Status)
			require.False(t, v.Jobs[0].BudgetReleased)
			require.Zero(t, a.creates)
			if tc.code == 429 {
				require.Greater(t, time.Until(v.Supplier558669.RetryAt), 179*time.Second)
				require.True(t, v.ProviderRetryAt.IsZero())
			}
			if tc.code == 409 {
				p.deliveryErr = nil
				refillDue(t, store)
				require.NoError(t, s.run(context.Background(), false))
				require.Equal(t, "imported", read558669(t, store).Jobs[0].Status)
				require.Len(t, p.submitted, 1)
				require.Equal(t, []string{"558-task"}, p.statusIDs)
			}
		})
	}
}

func TestRefill558669UncertainOrdersNeverTriggerBackupOrTimeoutPurchase(t *testing.T) {
	for _, status := range []string{"queued", "processing", "review", "supplier_unknown", "blocked"} {
		t.Run(status, func(t *testing.T) {
			s, store, p, _ := harness558669(t)
			reserve558669(t, s)
			store.mutate(t, func(v *accountRefillState) {
				v.Config.Enabled, v.Config.MonitorEnabled = true, true
				v.Config.MuzeFallback = &AccountRefillMuzeFallbackConfig{Enabled: true, Provider: "1208", FailureThreshold: 1, TimeoutMinutes: 1}
				v.Jobs[0].ManualRequestID = ""
				v.Jobs[0].Reason = "自动补号"
				v.Jobs[0].Mode = v.Config.Mode
				v.Jobs[0].Status = status
				v.Jobs[0].CreatedAt = time.Now().Add(-time.Hour)
				v.Jobs[0].NextAttemptAt = time.Now().Add(time.Hour)
				v.LastSubmitAt = time.Now().Add(-time.Hour)
			})
			require.NoError(t, s.run(context.Background(), false))
			v := read558669(t, store)
			require.Len(t, v.Jobs, 1)
			require.Empty(t, p.submitted)
			require.False(t, refillJobTimedOut(v.Config, v.Jobs[0], time.Now()))
			require.Nil(t, refillTimeoutCandidate(&v, v.Jobs, time.Now()))
		})
	}
}

func TestRefill558669ConfirmedFailureFallsBackOnce(t *testing.T) {
	s, store, muze, _, _ := reverseRefillHarness(t)
	p := &refill558669Fixture{refillTestProvider: &refillTestProvider{store: store, t: t, status: "failed"}, quoteVersion: 1280, quotePrice: 12, available: 1000}
	s.provider558669 = p
	store.mutate(t, func(v *accountRefillState) {
		v.Config.PrimaryProvider = "558669"
		v.Config.MuzeFallback.Provider = "muze"
		v.Supplier558669.EncryptedToken = "558-fixture-token"
	})
	require.NoError(t, s.run(context.Background(), false))
	require.NoError(t, s.run(context.Background(), false))
	v := read558669(t, store)
	require.Len(t, p.submitted, 1)
	require.Equal(t, 1, muze.orders)
	require.Len(t, v.Jobs, 2)
	require.Equal(t, "muze", v.Jobs[0].Provider)
	require.Equal(t, v.Jobs[1].ID, v.Jobs[0].FallbackForJobID)
	require.True(t, v.Jobs[1].BudgetReleased)
	require.Zero(t, v.Supplier558669.Usage[time.Now().In(refillTimezone).Format("2006-01-02")].Points)
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, muze.orders)
}

func TestRefill558669CredentialIdentityAndVersionGuard(t *testing.T) {
	s, store, p, a := harness558669(t)
	reserve558669(t, s)
	p.ready = true
	require.NoError(t, s.run(context.Background(), false))
	j := read558669(t, store).Jobs[0]
	j.DeliveryVersion = 2
	d, err := p.Delivery(context.Background(), "fixture", "558-task")
	require.NoError(t, err)
	for _, key := range []string{"refill_job_id", "refill_supplier_id", "import_source"} {
		copy := a.accounts[0]
		copy.Extra = maps.Clone(copy.Extra)
		copy.Extra[key] = "other"
		_, err := refreshed558669Credentials(&copy, j, d)
		require.Error(t, err)
	}
	d.Accounts[0].Credentials["email"] = "another@example.test"
	_, err = refreshed558669Credentials(&a.accounts[0], j, d)
	require.Error(t, err)
}

func TestRefill558669DailyBatchAndSharedReservations(t *testing.T) {
	t.Run("daily budget is all or nothing", func(t *testing.T) {
		s, store, p, _ := harness558669(t)
		seedDailyRefill(t, store, time.Now().Add(-time.Minute).Truncate(time.Minute), 3)
		store.mutate(t, func(v *accountRefillState) {
			v.Config.Supplier558669 = &Refill558669Limits{MaxUnitPrice: 20, DailyMaxPoints: 30}
		})
		require.NoError(t, s.run(context.Background(), false))
		require.Empty(t, read558669(t, store).Jobs)
		require.Empty(t, p.submitted)
		store.mutate(t, func(v *accountRefillState) {
			v.Config.Supplier558669.DailyMaxPoints = 36
			v.DailyScheduleStatus.RetryAt = time.Time{}
		})
		require.NoError(t, s.run(context.Background(), false))
		v := read558669(t, store)
		require.Len(t, v.Jobs, 3)
		require.Empty(t, p.submitted)
		day := time.Now().In(refillTimezone).Format("2006-01-02")
		require.Equal(t, 36.0, v.Supplier558669.Usage[day].Points)
		require.Zero(t, v.Usage[day].Points)
		require.Equal(t, 3, v.Usage[day].Count)
	})
	t.Run("groups inherit only connection and share wallet holds", func(t *testing.T) {
		s, store, p, _ := harness558669(t)
		p.available = 20
		reserve558669(t, s)
		grouped := &refillGroupedTestStore{refillTestStore: store, children: map[int64]*refillTestStore{}}
		s.store = grouped
		child, err := s.ForGroup(context.Background(), 3)
		require.NoError(t, err)
		v, err := child.View(context.Background())
		require.NoError(t, err)
		require.True(t, v.Supplier558669.TokenConfigured)
		require.Zero(t, v.Supplier558669.Daily.Points)
		require.Empty(t, v.Jobs)
		require.Same(t, p, child.provider558669)
		r := manualRequest()
		r.Provider = "558669"
		r.GroupID = 3
		r.Revision = v.Revision
		// An untouched group has revision zero; save its rule before purchasing.
		require.NoError(t, child.Configure(context.Background(), v.Config, "", v.Revision))
		r.Revision++
		require.ErrorContains(t, child.Manual(context.Background(), r), "本地预占")
		require.Empty(t, p.submitted)
	})
}

func TestRefill558669CrashRecoveryDoesNotRecreateOrOverstateAppliedVersion(t *testing.T) {
	s, store, p, a := harness558669(t)
	reserve558669(t, s)
	p.ready = true
	a.failAfterCreate = true
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, 1, a.creates)
	v := read558669(t, store)
	require.Equal(t, "import_failed", v.Jobs[0].Status)
	a.failAfterCreate = false
	p.version = 2
	require.NoError(t, s.Retry(context.Background(), v.Jobs[0].ID))
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	v = read558669(t, store)
	require.Equal(t, "imported", v.Jobs[0].Status)
	require.Equal(t, int64(1), v.Jobs[0].AppliedDeliveryVersion)
	require.Equal(t, 1, a.creates)
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Equal(t, int64(2), read558669(t, store).Jobs[0].AppliedDeliveryVersion)
	require.Equal(t, 1, a.creates)
}

func TestRefill558669HTTPBackoffAndRedaction(t *testing.T) {
	for _, code := range []int{400, 401, 403, 409, 410, 429, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "241")
				w.WriteHeader(code)
				fmt.Fprint(w, `{"error":{"code":"unavailable","message":"fixture-secret-sensitive"}}`)
			}))
			defer server.Close()
			p := newHTTP558669Provider()
			p.base = server.URL
			_, err := p.Status(context.Background(), "fixture-secret-sensitive", "task-1")
			var remote *refillProviderError
			require.ErrorAs(t, err, &remote)
			require.Equal(t, code, remote.code)
			require.Equal(t, 241*time.Second, remote.retryAfter)
			require.NotContains(t, err.Error(), "fixture-secret-sensitive")
		})
	}
}
