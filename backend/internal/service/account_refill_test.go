//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func refillTestConfig() AccountRefillConfig {
	c := DefaultAccountRefillConfig()
	c.GroupID = 2
	c.DeliveryGroupIDs = []int64{2}
	c.Enabled = true
	return c
}
func refillTestAccount(expiry time.Time) Account {
	return Account{ID: 1, Name: "fixture", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, GroupIDs: []int64{2}, Concurrency: 60, ExpiresAt: &expiry}
}

func TestImportedAccountExpiry(t *testing.T) {
	original := time.Now().Add(time.Hour).Unix()
	extra := map[string]any{"unrelated": true}
	deadline, marked := ImportedAccountExpiry(&original, extra)
	require.Equal(t, original+3600, *deadline)
	require.Len(t, extra, 1)
	require.Equal(t, 3600, marked[AccountImportExpiryGraceExtraKey])
	again, againExtra := ImportedAccountExpiry(deadline, marked)
	require.Equal(t, *deadline, *again)
	require.Equal(t, marked, againExtra)
	repeated, _ := ImportedAccountExpiry(&original, nil)
	require.Equal(t, *deadline, *repeated)
	later := original + 900
	changed, _ := ImportedAccountExpiry(&later, marked)
	require.Equal(t, later+3600, *changed)
	legacyExpiry := original + 3600
	legacy, legacyExtra := ImportedAccountExpiry(&legacyExpiry, map[string]any{"import_original_expires_at": original, "import_expiry_offset_seconds": 3600})
	require.Equal(t, original+3600, *legacy)
	require.NotContains(t, legacyExtra, "import_original_expires_at")
	// An already materialized timestamp is idempotent; a different import is extended once.
	renewedExpiry := original + 7200
	renewed, _ := ImportedAccountExpiry(&renewedExpiry, marked)
	require.Equal(t, renewedExpiry+3600, *renewed)
	missing, _ := ImportedAccountExpiry(nil, nil)
	require.Nil(t, missing)
}

func TestAccountRefillEvaluation(t *testing.T) {
	now := time.Now()
	c := refillTestConfig()
	a := refillTestAccount(now.Add(20 * time.Minute))
	// A single account triggers on source expiry even with the local grace period.
	original := a.ExpiresAt.Unix()
	deadline, extra := ImportedAccountExpiry(&original, nil)
	local := time.Unix(*deadline, 0)
	a.ExpiresAt = &local
	a.Extra = extra
	e := evaluateAccountRefill(c, []Account{a}, nil, nil, now)
	require.Equal(t, 1, e.Needed)
	require.Equal(t, a.ExpiresAt.Unix(), e.Accounts[0].ExpiresAt.Unix())
	require.Equal(t, original, e.Accounts[0].SourceExpiresAt.Unix())
	c.MaxReady = 1
	require.Equal(t, 1, evaluateAccountRefill(c, []Account{a}, nil, nil, now).Needed)
	c.MaxReady = 20
	require.Equal(t, 1, e.Expiring)
	require.Zero(t, e.Ready)
	e = evaluateAccountRefill(c, []Account{a}, nil, []AccountRefillJob{{Status: "queued"}}, now)
	require.Zero(t, e.Needed)
	require.Equal(t, 1, e.Pending)
	a = refillTestAccount(now.Add(time.Hour))
	e = evaluateAccountRefill(c, []Account{a}, nil, nil, now)
	require.Zero(t, e.Needed)
	loads := map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 60}}
	e = evaluateAccountRefill(c, []Account{a}, loads, nil, now)
	require.Equal(t, 1, e.Needed)
	require.Equal(t, "并发利用率超过目标", e.Reason)
	c.Mode = "fixed"
	e = evaluateAccountRefill(c, []Account{a}, loads, nil, now)
	require.Zero(t, e.Needed)
	a.Extra = map[string]any{"codex_5h_used_percent": 95.0, "codex_5h_reset_at": now.Add(time.Hour).Format(time.RFC3339)}
	e = evaluateAccountRefill(c, []Account{a}, nil, nil, now)
	require.Equal(t, 1, e.Needed)
	require.Equal(t, "quota_low", e.Accounts[0].State)
	c.Mode = "interval"
	e = evaluateAccountRefill(c, []Account{a}, nil, nil, now)
	require.Zero(t, e.Needed)
	c.Mode = "capacity"
	for _, status := range []string{"review", "blocked", "import_failed"} {
		e = evaluateAccountRefill(c, nil, nil, []AccountRefillJob{{Status: status}}, now)
		require.Zero(t, e.Needed)
	}
}

func TestAccountRefillWindowsAndValidation(t *testing.T) {
	c := refillTestConfig()
	require.NoError(t, c.Validate())
	c.WindowEnabled = true
	c.WindowStart = "23:00"
	c.WindowEnd = "06:00"
	for _, tc := range []struct {
		hour int
		want bool
	}{{23, true}, {0, true}, {5, true}, {6, false}, {22, false}} {
		require.Equal(t, tc.want, refillInWindow(c, time.Date(2026, 10, 31, tc.hour, 0, 0, 0, refillTimezone)))
	}
	c.DeliveryGroupIDs = []int64{3}
	require.Error(t, c.Validate())
	c = refillTestConfig()
	c.Delivery.WSMode = "invalid"
	require.Error(t, c.Validate())
	c = refillTestConfig()
	c.DailyMaxPoints = 0
	require.Error(t, c.Validate())
}

type refillTestStore struct {
	raw      string
	failSave bool
}

func (s *refillTestStore) Read(context.Context) (string, error) { return s.raw, nil }
func (s *refillTestStore) WithLock(_ context.Context, fn func(string, func(string) error) error) error {
	return fn(s.raw, func(v string) error {
		if s.failSave {
			return errors.New("save failed")
		}
		s.raw = v
		return nil
	})
}
func (s *refillTestStore) mutate(t *testing.T, fn func(*accountRefillState)) {
	t.Helper()
	v, e := decodeAccountRefillState(s.raw)
	require.NoError(t, e)
	fn(&v)
	b, e := json.Marshal(v)
	require.NoError(t, e)
	s.raw = string(b)
}

type refillTestEncryptor struct{}

func (refillTestEncryptor) Encrypt(v string) (string, error) { return "ciphertext", nil }
func (refillTestEncryptor) Decrypt(v string) (string, error) { return "rst_fixture", nil }

type refillTestAccounts struct {
	accounts        []Account
	creates         int
	failAfterCreate bool
	selectionCalls  int
	selectionError  error
	selectedProxyID int64
}

func (a *refillTestAccounts) CreateAccountWithProxySelection(ctx context.Context, in *CreateAccountInput) (*Account, error) {
	a.selectionCalls++
	if in.ProxyMode != "random" || in.ProxyID != nil {
		return nil, errors.New("expected transactional random selection without a preselected proxy")
	}
	if a.selectionError != nil {
		return nil, a.selectionError
	}
	in.ProxyID = &a.selectedProxyID
	return a.CreateAccount(ctx, in)
}

func (a *refillTestAccounts) ListByGroup(context.Context, int64) ([]Account, error) {
	var out []Account
	for _, v := range a.accounts {
		if v.Status == StatusActive {
			out = append(out, v)
		}
	}
	return out, nil
}
func (a *refillTestAccounts) FindByExtraField(_ context.Context, k string, v any) ([]Account, error) {
	var out []Account
	for _, x := range a.accounts {
		if x.Extra[k] == v {
			out = append(out, x)
		}
	}
	return out, nil
}
func (a *refillTestAccounts) BindGroups(_ context.Context, id int64, ids []int64) error {
	for i := range a.accounts {
		if a.accounts[i].ID == id {
			a.accounts[i].GroupIDs = ids
		}
	}
	return nil
}
func (a *refillTestAccounts) SetSchedulable(_ context.Context, id int64, v bool) error {
	for i := range a.accounts {
		if a.accounts[i].ID == id {
			a.accounts[i].Schedulable = v
		}
	}
	return nil
}

func (a *refillTestAccounts) UpdateExtra(_ context.Context, id int64, values map[string]any) error {
	for i := range a.accounts {
		if a.accounts[i].ID == id {
			if a.accounts[i].Extra == nil {
				a.accounts[i].Extra = map[string]any{}
			}
			for k, v := range values {
				a.accounts[i].Extra[k] = v
			}
			return nil
		}
	}
	return ErrAccountNotFound
}
func (a *refillTestAccounts) CreateAccount(_ context.Context, in *CreateAccountInput) (*Account, error) {
	a.creates++
	v, err := buildAccountForCreate(in, in.Extra)
	if err != nil {
		return nil, err
	}
	v.ID = int64(a.creates + 100)
	a.accounts = append(a.accounts, *v)
	if a.failAfterCreate {
		return nil, errors.New("crash after create")
	}
	return v, nil
}
func (a *refillTestAccounts) UpdateAccount(_ context.Context, id int64, in *UpdateAccountInput) (*Account, error) {
	for i := range a.accounts {
		if a.accounts[i].ID == id {
			a.accounts[i].Status = in.Status
			return &a.accounts[i], nil
		}
	}
	return nil, errors.New("missing")
}

type refillTestGroups struct{}

type refillTestProxies struct {
	accountRefillProxies
	free bool
}

func (p *refillTestProxies) HasFreeAccountProxy(context.Context) (bool, error) { return p.free, nil }

func (refillTestGroups) GetByID(context.Context, int64) (*Group, error) {
	return &Group{Platform: PlatformOpenAI, Status: StatusActive}, nil
}

type refillTestLoads struct{ values map[int64]*AccountLoadInfo }

func (l refillTestLoads) GetAccountsLoadBatch(context.Context, []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	return l.values, nil
}

type refillTestProvider struct {
	customerID    string
	store         *refillTestStore
	t             *testing.T
	submitted     []AccountRefillJob
	submitError   error
	status        string
	deliveryCalls int
}

func (p *refillTestProvider) Catalog(context.Context, string) (*RefillCatalog, error) {
	return &RefillCatalog{Available: 500, Items: []RefillQuote{{Service: "refill", Plan: "pro5x", Price: 60, PriceVersion: 1, Enabled: true}}}, nil
}
func (p *refillTestProvider) Identity(context.Context, string) (*RefillWallet, *RefillKeyInfo, error) {
	id := p.customerID
	if id == "" {
		id = "customer-1"
	}
	return &RefillWallet{CustomerID: id, Available: 500, Balance: 500}, &RefillKeyInfo{ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (p *refillTestProvider) Submit(_ context.Context, _ string, j AccountRefillJob) (*RefillProviderJob, error) {
	state, err := decodeAccountRefillState(p.store.raw)
	require.NoError(p.t, err)
	require.NotEmpty(p.t, state.Jobs)
	require.Equal(p.t, j.ID, state.Jobs[0].ID)
	require.Equal(p.t, 60.0, state.Usage[j.CreatedAt.In(refillTimezone).Format("2006-01-02")].Points)
	p.submitted = append(p.submitted, j)
	return &RefillProviderJob{ID: "supplier-1", Status: "queued", Price: 60}, p.submitError
}
func (p *refillTestProvider) Status(context.Context, string, string) (*RefillProviderJob, error) {
	return &RefillProviderJob{ID: "supplier-1", Status: p.status, ResultReady: p.status == "succeeded", Price: 60}, nil
}
func (p *refillTestProvider) Delivery(context.Context, string, string) (*RefillDelivery, error) {
	p.deliveryCalls++
	var d RefillDelivery
	err := json.Unmarshal([]byte(`{"accounts":[{"name":"fixture","platform":"openai","type":"oauth","credentials":{"access_token":"fixture-access","refresh_token":"fixture-refresh","expires_at":"unchanged","malicious":"ignored"}}]}`), &d)
	deadline := time.Now().Add(time.Hour).Unix()
	d.Accounts[0].ExpiresAt = &deadline
	return &d, err
}
func refillHarness(t *testing.T) (*AccountRefillService, *refillTestStore, *refillTestProvider, *refillTestAccounts) {
	t.Helper()
	store := &refillTestStore{}
	provider := &refillTestProvider{store: store, t: t, status: "processing"}
	accounts := &refillTestAccounts{}
	s := &AccountRefillService{store: store, encryptor: refillTestEncryptor{}, accounts: accounts, admin: accounts, groups: refillTestGroups{}, loads: refillTestLoads{}, provider: provider}
	require.NoError(t, s.Configure(context.Background(), refillTestConfig(), "rst_fixture", 0))
	return s, store, provider, accounts
}
func refillDue(t *testing.T, store *refillTestStore) {
	store.mutate(t, func(v *accountRefillState) {
		v.NextCheckAt = time.Time{}
		v.LastSubmitAt = time.Time{}
		v.ProviderRetryAt = time.Time{}
		for i := range v.Jobs {
			v.Jobs[i].NextAttemptAt = time.Time{}
		}
	})
}

func TestAccountRefillDurableReservationRetryAndRestart(t *testing.T) {
	s, store, p, accounts := refillHarness(t)
	ctx := context.Background()
	accounts.accounts = []Account{refillTestAccount(time.Now().Add(20 * time.Minute))}
	p.submitError = errors.New("uncertain timeout")
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.submitted, 1)
	first := p.submitted[0]
	require.Equal(t, "账号临期，提前补充储备", first.Reason)
	require.Contains(t, first.ReasonDetail, "1 个将在 30 分钟内到期")
	accounts.accounts = nil
	store.mutate(t, func(v *accountRefillState) { v.Config.MinReady = 4 })
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "在途任务已抵扣缺口，等待交付", v.Evaluation.Reason)
	require.Equal(t, first.ReasonDetail, v.Jobs[0].ReasonDetail, "history must not use the current rules or evaluation")
	require.Equal(t, 60.0, v.Daily.ReservedPoints)
	refillDue(t, store)
	p.submitError = nil
	// A new service instance shares only persisted state, not a process-local job map.
	restarted := *s
	require.NoError(t, restarted.run(ctx, false))
	require.Len(t, p.submitted, 2)
	require.Equal(t, first.ID, p.submitted[1].ID)
	require.Equal(t, first.PriceVersion, p.submitted[1].PriceVersion)
	require.Equal(t, first.ReasonDetail, p.submitted[1].ReasonDetail, "retries preserve the durable trigger snapshot")
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 1)
	require.Equal(t, 1, v.Daily.Count)
	p.status = "review"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.submitted, 2)
	p.status = "failed"
	refillDue(t, store)
	store.mutate(t, func(v *accountRefillState) { v.Config.Enabled = false })
	require.NoError(t, s.run(ctx, false))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Zero(t, v.Daily.Points)
	require.Zero(t, v.Daily.ReservedPoints)
}

func TestAccountRefillDoesNotPayWhenReservationCannotPersist(t *testing.T) {
	s, store, p, _ := refillHarness(t)
	store.failSave = true
	require.Error(t, s.run(context.Background(), false))
	require.Empty(t, p.submitted)
}

func TestAccountRefillRandomProxyWaitsForFreeProxyAndRetriesSameOrder(t *testing.T) {
	s, store, p, a := refillHarness(t)
	s.proxies = &refillTestProxies{free: true}
	ctx := context.Background()
	store.mutate(t, func(v *accountRefillState) { v.Config.Delivery.ProxyMode = "random" })
	require.NoError(t, s.run(ctx, false))
	p.status = "succeeded"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	a.selectionError = errors.New("没有未绑定账号的空闲代理")
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "import_failed", v.Jobs[0].Status)
	require.Zero(t, a.creates)
	require.Equal(t, 1, a.selectionCalls)
	jobID := v.Jobs[0].ID
	a.selectionError = nil
	a.selectedProxyID = 73
	require.NoError(t, s.Retry(ctx, jobID))
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, a.creates)
	require.EqualValues(t, 73, *a.accounts[0].ProxyID)
	require.Len(t, p.submitted, 1, "空闲代理耗尽后的重试不得重新购买")
	require.Equal(t, jobID, p.submitted[0].ID)
}

func TestAccountRefillRandomProxyDoesNotPurchaseWhenPoolOccupied(t *testing.T) {
	s, store, p, _ := refillHarness(t)
	s.proxies = &refillTestProxies{free: false}
	store.mutate(t, func(v *accountRefillState) { v.Config.Delivery.ProxyMode = "random" })
	require.NoError(t, s.run(context.Background(), false))
	require.Empty(t, p.submitted)
	v, err := s.View(context.Background())
	require.NoError(t, err)
	require.Contains(t, v.Message, "空闲代理")
}
func TestAccountRefillPreviewAndLimits(t *testing.T) {
	for _, scenario := range []string{"preview", "disabled", "budget", "unit_price", "window", "monitor_off"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, _ := refillHarness(t)
			store.mutate(t, func(v *accountRefillState) {
				switch scenario {
				case "disabled":
					v.Config.Enabled = false
				case "budget":
					v.Config.DailyMaxPoints = 59
				case "unit_price":
					v.Config.MaxUnitPrice = 59
				case "window":
					v.Config.WindowEnabled = true
					v.Config.WindowStart = time.Now().In(refillTimezone).Add(2 * time.Hour).Format("15:04")
					v.Config.WindowEnd = time.Now().In(refillTimezone).Add(3 * time.Hour).Format("15:04")
				case "monitor_off":
					v.Config.MonitorEnabled = false
				}
			})
			require.NoError(t, s.run(context.Background(), scenario == "preview"))
			require.Empty(t, p.submitted)
		})
	}
}

func TestAccountRefillImportCrashRecoveryAndPause(t *testing.T) {
	s, store, p, a := refillHarness(t)
	ctx := context.Background()
	require.NoError(t, s.run(ctx, false))
	p.status = "succeeded"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	store.mutate(t, func(v *accountRefillState) { v.Config.PushEnabled = false })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Zero(t, a.creates)
	store.mutate(t, func(v *accountRefillState) { v.Config.PushEnabled = true; v.Config.Enabled = false })
	a.failAfterCreate = true
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, a.creates)
	require.False(t, a.accounts[0].Schedulable)
	require.Equal(t, "inactive", a.accounts[0].Status)
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "import_failed", v.Jobs[0].Status)
	require.NoError(t, s.Retry(ctx, v.Jobs[0].ID))
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, a.creates)
	require.Equal(t, 1, p.deliveryCalls)
	require.True(t, a.accounts[0].Schedulable)
	require.Equal(t, StatusActive, a.accounts[0].Status)
	require.Equal(t, []int64{2}, a.accounts[0].GroupIDs)
	require.Equal(t, "unchanged", a.accounts[0].Credentials["expires_at"])
	require.NotContains(t, a.accounts[0].Credentials, "malicious")
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "imported", v.Jobs[0].Status)
	data, err := json.Marshal(v)
	require.NoError(t, err)
	require.NotContains(t, string(data), "fixture-access")
	require.NotContains(t, string(data), "ciphertext")
}

func TestAccountRefillIntervalAndCapacityPersistence(t *testing.T) {
	s, store, p, a := refillHarness(t)
	ctx := context.Background()
	a.accounts = []Account{refillTestAccount(time.Now().Add(time.Hour))}
	s.loads = refillTestLoads{map[int64]*AccountLoadInfo{1: {CurrentConcurrency: 60}}}
	require.NoError(t, s.run(ctx, false))
	require.Empty(t, p.submitted)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	feedRefillCapacity(s, state.Config, state.Revision, time.Now(), 30)
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.submitted, 1)
	require.Contains(t, p.submitted[0].ReasonDetail, "运行中并发 60 / 60（利用率 100.00%）")
	require.Contains(t, p.submitted[0].ReasonDetail, "达到设定 85%，持续观察已满 30 秒")
	s, store, p, _ = refillHarness(t)
	store.mutate(t, func(v *accountRefillState) { v.Config.Mode = "interval"; v.NextIntervalAt = time.Now().Add(time.Hour) })
	require.NoError(t, s.run(ctx, false))
	require.Empty(t, p.submitted)
	store.mutate(t, func(v *accountRefillState) { v.NextIntervalAt = time.Now().Add(-time.Minute) })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Len(t, p.submitted, 1)
	require.Contains(t, p.submitted[0].ReasonDetail, "定时周期已到：每 60 分钟补 1 个")
}

func TestRefillHTTPContractAndWalletUnits(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer rst_fixture", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/refills":
			keys = append(keys, r.Header.Get("Idempotency-Key"))
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "sub2api", body["format"])
			require.Equal(t, float64(12), body["price_version"])
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"id":"s1","status":"queued"}`))
		case "/me":
			_, _ = w.Write([]byte(`{"balance":482550,"held":6000,"available":476550}`))
		case "/keys":
			_, _ = w.Write([]byte(`{"total":1,"items":[{"label":"fixture","prefix":"rst_fix","scope":"write","expires_at":"2027-01-29T12:59:37Z"}]}`))
		default:
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			_, _ = w.Write([]byte("sensitive-provider-body"))
		}
	}))
	defer server.Close()
	p := newHTTPRefillProvider()
	p.base = server.URL
	p.identityBase = server.URL
	j := AccountRefillJob{ID: "stable", Plan: "pro5x", PriceVersion: 12}
	for range 2 {
		_, err := p.Submit(context.Background(), "rst_fixture", j)
		require.NoError(t, err)
	}
	require.Equal(t, keys[0], keys[1])
	wallet, key, err := p.Identity(context.Background(), "rst_fixture")
	require.NoError(t, err)
	require.Equal(t, 4825.5, wallet.Balance)
	require.Equal(t, 60.0, wallet.Held)
	require.Equal(t, 4765.5, wallet.Available)
	require.Equal(t, "fixture", key.Label)
	_, err = p.Status(context.Background(), "rst_fixture", "error")
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "sensitive-provider-body"))
	require.GreaterOrEqual(t, refillRetryDelay(err, 1), 120*time.Second)
}

func TestAccountRefillKeyRotationAndReconciliation(t *testing.T) {
	s, _, p, _ := refillHarness(t)
	ctx := context.Background()
	p.submitError = &refillProviderError{code: 409}
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "blocked", v.Jobs[0].Status)
	p.customerID = "different-customer"
	require.ErrorContains(t, s.Configure(ctx, v.Config, "rst_replacement", v.Revision), "同一号商账户")
	p.customerID = "customer-1"
	require.NoError(t, s.Configure(ctx, v.Config, "rst_replacement", v.Revision))
	require.NoError(t, s.DismissRejected(ctx, v.Jobs[0].ID))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Zero(t, v.Daily.Points)
	require.Equal(t, "dismissed", v.Jobs[0].Status)
	require.Error(t, s.DismissRejected(ctx, v.Jobs[0].ID))
	s, _, p, _ = refillHarness(t)
	require.NoError(t, s.run(ctx, false))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Error(t, s.DismissRejected(ctx, v.Jobs[0].ID))
}

func TestAccountRefillQueueFairness(t *testing.T) {
	now := time.Now()
	jobs := []AccountRefillJob{{Status: "queued", NextAttemptAt: now.Add(-time.Second)}, {Status: "processing", NextAttemptAt: now.Add(-time.Minute)}, {Status: "awaiting_push", NextAttemptAt: now.Add(-time.Hour)}}
	require.Equal(t, 1, nextAccountRefillJob(jobs, false, now))
	require.Equal(t, 2, nextAccountRefillJob(jobs, true, now))
	jobs[1].Status = "blocked"
	require.Equal(t, 0, nextAccountRefillJob(jobs, false, now))
	jobs[0].NextAttemptAt = now.Add(time.Minute)
	require.Equal(t, -1, nextAccountRefillJob(jobs, false, now))
}
