//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fenduduTestProvider struct {
	t             *testing.T
	store         *refillTestStore
	available     int
	orders        int
	orderError    error
	failAfterPost bool
	cycle         *fenduduCycle
}

func (p *fenduduTestProvider) Products(context.Context, string) ([]FenduduProduct, error) {
	return []FenduduProduct{{Product: "pro20x", AvailableNow: p.available}}, nil
}
func (p *fenduduTestProvider) Order(_ context.Context, _ string, j AccountRefillJob) (*fenduduOrder, error) {
	state, err := decodeAccountRefillState(p.store.raw)
	require.NoError(p.t, err)
	require.NotNil(p.t, state.Jobs[0].SubmissionStartedAt, "persist before the non-idempotent POST")
	require.Equal(p.t, j.ID, state.Jobs[0].ID)
	p.orders++
	p.store.failSave = p.failAfterPost
	p.cycle = &fenduduCycle{ID: "rvc_fixture", Product: j.Plan, PushProvider: "sub2api", PushConfigID: "s2e_fixture", Status: "paid", CreatedAt: j.SubmissionStartedAt.Format(time.RFC3339Nano)}
	return &fenduduOrder{Success: true, Created: 1, CycleIDs: []string{"rvc_fixture"}}, p.orderError
}
func (p *fenduduTestProvider) Cycle(context.Context, string, string) (*fenduduCycle, error) {
	return p.cycle, nil
}

type fenduduTestAdmin struct {
	*refillTestAccounts
	input  *CreateAccountInput
	adopt  int
	closed int
}

func (a *fenduduTestAdmin) AdoptRefillAccount(_ context.Context, id int64, in *CreateAccountInput) (*Account, error) {
	a.input, a.adopt = in, a.adopt+1
	v, err := buildAccountForCreate(in, in.Extra)
	if err != nil {
		return nil, err
	}
	v.ID = id
	for i := range a.accounts {
		if a.accounts[i].ID == id {
			v.CreatedAt = a.accounts[i].CreatedAt
			a.accounts[i] = *v
			return &a.accounts[i], nil
		}
	}
	return nil, ErrAccountNotFound
}

func (a *fenduduTestAdmin) RecordExpiredRefillAccount(_ context.Context, id int64, in *CreateAccountInput) error {
	for i := range a.accounts {
		v := &a.accounts[i]
		if v.ID == id {
			a.closed++
			old := v.ExpiresAt
			ApplyAccountExpiryExtra(v, in.Extra)
			if old != nil && old.Before(*v.ExpiresAt) {
				ApplyAccountExpiryWrite(context.Background(), v, old.Unix())
			}
			v.Status, v.Schedulable = "inactive", false
			return nil
		}
	}
	return ErrAccountNotFound
}

func fenduduHarness(t *testing.T) (*AccountRefillService, *refillTestStore, *fenduduTestProvider, *fenduduTestAdmin) {
	s, store, _, accounts := refillHarness(t)
	p := &fenduduTestProvider{t: t, store: store, available: 5}
	admin := &fenduduTestAdmin{refillTestAccounts: accounts}
	s.fendudu, s.admin = p, admin
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled, state.Config.MonitorEnabled = false, false
		state.EncryptedToken = "" // Backup orders work without the primary key.
		state.Fendudu = fenduduRefillState{EncryptedToken: "ciphertext", TokenHash: "same-wallet", ConnectionID: "s2e_fixture"}
	})
	return s, store, p, admin
}
func fenduduRequest() ManualAccountRefillRequest {
	r := manualRequest()
	r.Provider = refillProviderFendudu
	return r
}

func TestFenduduManualSnapshotAndPushedAccountRecovery(t *testing.T) {
	s, store, p, accounts := fenduduHarness(t)
	store.mutate(t, func(state *accountRefillState) {
		state.Config.DeliveryGroupIDs = []int64{2, 3}
		state.Config.PlanImportConcurrency = map[string]int{"pro20x": 77}
		state.Config.ImportExpiryGraceMinutes = 30
		state.Config.Delivery.Priority = 8
		state.Config.Delivery.RateMultiplier = 1.5
		state.Config.Delivery.LoadFactor = 99
		state.Config.Delivery.NameTemplate = "backup-{plan_type}"
		state.Config.Delivery.ModelMapping = map[string]string{"a": "b"}
	})
	ctx := context.Background()
	r := fenduduRequest()
	require.NoError(t, s.Manual(ctx, r))
	require.NoError(t, s.Manual(ctx, r))
	require.Zero(t, p.orders)
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Len(t, v.Jobs, 1)
	require.Equal(t, 1, v.Daily.Count)
	require.Zero(t, v.Daily.Points)
	require.Zero(t, v.RotationCursor)
	r.Provider = ""
	require.ErrorContains(t, s.Manual(ctx, r), "同一请求")
	store.mutate(t, func(state *accountRefillState) {
		state.Revision++
		state.Config.ImportConcurrency = 3
		state.Config.Delivery.Priority = 99
	})
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, p.orders)
	lease := time.Now().Add(time.Hour).Truncate(time.Second)
	p.cycle.Pushed, p.cycle.AccountID, p.cycle.Email, p.cycle.RevokeAt = true, "344", "fixture@example.test", lease.Format(time.RFC3339)
	accounts.accounts = []Account{{ID: 344, Name: "supplier name", CreatedAt: time.Now(), Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"email": "fixture@example.test", "access_token": "fixture", "refresh_token": "refresh"}, Extra: map[string]any{}}}
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Zero(t, accounts.creates, "must adopt the pushed account, never create another")
	require.Equal(t, 1, accounts.adopt)
	a := accounts.accounts[0]
	require.Equal(t, "backup-pro20x", a.Name)
	require.Equal(t, 77, a.Concurrency)
	require.Equal(t, 8, a.Priority)
	require.Equal(t, []int64{2, 3}, a.GroupIDs)
	require.Equal(t, "fendudu_api", a.Extra["import_source"])
	require.Equal(t, map[string]string{"a": "b"}, a.Credentials["model_mapping"])
	require.Equal(t, lease.Add(30*time.Minute).Unix(), a.ExpiresAt.Unix())
	require.Equal(t, lease.Unix(), a.Extra[AccountSourceExpiryExtraKey])
	require.True(t, a.Schedulable)
	require.Equal(t, StatusActive, a.Status)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, int64(344), state.Jobs[0].AccountID)
	require.Equal(t, "imported", state.Jobs[0].Status)
	// Crash after committing account settings, before recording the job result.
	store.mutate(t, func(state *accountRefillState) { state.Jobs[0].Status = "importing"; state.Jobs[0].AccountID = 0 })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, accounts.adopt)
	require.Equal(t, 1, p.orders)
	_, err = s.DownloadDelivery(ctx, state.Jobs[0].ID)
	require.ErrorContains(t, err, "不提供交付 JSON")
}

func TestFenduduUncertainSubmissionNeverRepeats(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "crash-after-post"}[crash], func(t *testing.T) {
			s, store, p, _ := fenduduHarness(t)
			p.failAfterPost = crash
			if !crash {
				p.orderError = errors.New("timeout")
			}
			require.NoError(t, s.Manual(context.Background(), fenduduRequest()))
			err := s.run(context.Background(), false)
			if crash {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			store.failSave = false
			for i := 0; i < 3; i++ {
				refillDue(t, store)
				require.NoError(t, s.run(context.Background(), false))
			}
			require.Equal(t, 1, p.orders)
			state, err := decodeAccountRefillState(store.raw)
			require.NoError(t, err)
			require.Equal(t, "blocked", state.Jobs[0].Status)
			require.ErrorContains(t, s.Retry(context.Background(), state.Jobs[0].ID), "不能重复下单")
			require.NoError(t, s.ReconcileFendudu(context.Background(), state.Jobs[0].ID, "rvc_fixture"))
			refillDue(t, store)
			require.NoError(t, s.run(context.Background(), false))
			require.Equal(t, 1, p.orders)
		})
	}
}

func TestFenduduManualGuardsAndForce(t *testing.T) {
	for _, scenario := range []string{"quota", "push-disabled", "revision", "daily-limit", "pending", "save-failed"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, _ := fenduduHarness(t)
			r := fenduduRequest()
			store.mutate(t, func(v *accountRefillState) {
				switch scenario {
				case "quota":
					p.available = 0
				case "push-disabled":
					v.Config.PushEnabled = false
				case "revision":
					r.Revision++
				case "daily-limit":
					v.Usage[time.Now().In(refillTimezone).Format("2006-01-02")] = AccountRefillDailyUsage{Count: v.Config.DailyMaxCount}
				case "pending":
					v.Jobs = []AccountRefillJob{{ID: "old", Status: "review", CreatedAt: time.Now()}}
				}
			})
			if scenario == "save-failed" {
				store.failSave = true
			}
			require.Error(t, s.Manual(context.Background(), r))
			require.Zero(t, p.orders)
			if scenario == "pending" {
				r.Force = true
				require.NoError(t, s.Manual(context.Background(), r))
			}
		})
	}
}

func TestFenduduRejectsWrongPushedAccountAndOldOrders(t *testing.T) {
	s, store, p, accounts := fenduduHarness(t)
	require.NoError(t, s.Manual(context.Background(), fenduduRequest()))
	require.NoError(t, s.run(context.Background(), false))
	p.cycle.Pushed, p.cycle.AccountID, p.cycle.Email, p.cycle.RevokeAt = true, "344", "expected@example.test", time.Now().Add(time.Hour).Format(time.RFC3339)
	accounts.accounts = []Account{{ID: 344, Name: "keep", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"email": "other@example.test"}}}
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Zero(t, accounts.adopt)
	require.Equal(t, "keep", accounts.accounts[0].Name)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, "import_failed", state.Jobs[0].Status)
	p.cycle.CreatedAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
	require.Error(t, validateFenduduCycle(p.cycle, state.Jobs[0]))
}

func TestFenduduHTTPContract(t *testing.T) {
	var order map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "rvak_fixture", r.Header.Get("X-Api-Key"))
		require.Empty(t, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/products":
			_, _ = w.Write([]byte(`{"success":true,"products":[{"product":"pro20x","available_now":5}]}`))
		case "/order":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&order))
			_, _ = w.Write([]byte(`{"success":true,"created":1,"cycle_ids":["rvc_fixture"]}`))
		case "/cycle-detail":
			require.Equal(t, "rvc_fixture", r.URL.Query().Get("id"))
			_, _ = w.Write([]byte(`{"success":true,"cycle":{"id":"rvc_fixture","product":"pro20x","sub2api_account_id":"344"}}`))
		case "/cycles":
			require.Equal(t, http.MethodGet, r.Method)
			require.Equal(t, "200", r.URL.Query().Get("limit"))
			require.Equal(t, "cursor/+fixture", r.URL.Query().Get("cursor"))
			_, _ = w.Write([]byte(`{"success":true,"items":[{"id":"rvc_fixture"}],"has_more":false,"next_cursor":""}`))
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer server.Close()
	p := newHTTPFenduduProvider()
	p.base = server.URL
	products, err := p.Products(context.Background(), "rvak_fixture")
	require.NoError(t, err)
	require.Equal(t, 5, products[0].AvailableNow)
	_, err = p.Order(context.Background(), "rvak_fixture", AccountRefillJob{Plan: "pro20x", DeliveryGroupIDs: []int64{2, 5}, FenduduConnectionID: "s2e_fixture"})
	require.NoError(t, err)
	require.Equal(t, float64(1), order["count"])
	require.Equal(t, "2,5", order["sub2api_group_ids"])
	require.Equal(t, "s2e_fixture", order["sub2api_config_id"])
	require.Equal(t, float64(0), order["subscription_schedule"])
	require.Empty(t, order["push_fallback_config_id"])
	cycle, err := p.Cycle(context.Background(), "rvak_fixture", "rvc_fixture")
	require.NoError(t, err)
	require.Equal(t, "344", cycle.AccountID)
	page, err := p.Cycles(context.Background(), "rvak_fixture", "cursor/+fixture")
	require.NoError(t, err)
	require.Equal(t, "rvc_fixture", page.Items[0].ID)
}

func TestFenduduHTTPBusinessFailureAndRedirect(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
	defer target.Close()
	for _, response := range []string{`{"success":false,"error":"rvak_fixture"}`, `{"success":true,"products":[{"product":"pro20x","available_now":-1}]}`, `{"success":true}`, `{"success":`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(response)) }))
		p := newHTTPFenduduProvider()
		p.base = server.URL
		_, err := p.Products(context.Background(), "rvak_fixture")
		require.Error(t, err)
		require.NotContains(t, err.Error(), "rvak_fixture")
		server.Close()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	p := newHTTPFenduduProvider()
	p.base = server.URL
	_, err := p.Products(context.Background(), "rvak_fixture")
	require.Error(t, err)
	require.Zero(t, targetCalls)
}

func TestFenduduQuotaReservationsAcrossGroups(t *testing.T) {
	s, base, p, _ := fenduduHarness(t)
	p.available = 1
	stores := &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	s.store = stores
	child, err := s.ForGroup(context.Background(), 3)
	require.NoError(t, err)
	v, err := child.View(context.Background())
	require.NoError(t, err)
	require.True(t, v.Fendudu.TokenConfigured)
	require.NoError(t, child.Configure(context.Background(), v.Config, "", 0))
	require.NoError(t, s.Manual(context.Background(), fenduduRequest()))
	r := fenduduRequest()
	r.GroupID = 3
	require.ErrorContains(t, child.Manual(context.Background(), r), "次数不足")
	require.Zero(t, p.orders)
}

func TestFenduduConnectionsInheritIndependently(t *testing.T) {
	for _, backupAtRoot := range []bool{true, false} {
		name := "primary_at_root"
		if backupAtRoot {
			name = "backup_at_root"
		}
		t.Run(name, func(t *testing.T) {
			s, base, _, _ := fenduduHarness(t)
			other := &refillTestStore{raw: base.raw}
			base.mutate(t, func(state *accountRefillState) {
				if !backupAtRoot {
					state.EncryptedToken = "primary-cipher"
					state.Fendudu = fenduduRefillState{}
				}
			})
			other.mutate(t, func(state *accountRefillState) {
				state.Config.GroupID = 3
				state.Jobs = []AccountRefillJob{{ID: "must-not-inherit", Status: "processing"}}
				if backupAtRoot {
					state.EncryptedToken = "primary-cipher"
					state.Fendudu = fenduduRefillState{}
				}
			})
			s.store = &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{3: other}}
			child := s.groupService(4, s.store.(AccountRefillGroupStore))
			raw, err := child.store.Read(context.Background())
			require.NoError(t, err)
			state, err := decodeAccountRefillState(raw)
			require.NoError(t, err)
			require.Equal(t, "primary-cipher", state.EncryptedToken)
			require.Equal(t, "ciphertext", state.Fendudu.EncryptedToken)
			require.Equal(t, "s2e_fixture", state.Fendudu.ConnectionID)
			require.Empty(t, state.Jobs)
			require.False(t, state.Config.Enabled)
			require.Equal(t, []int64{4}, state.Config.DeliveryGroupIDs)
		})
	}
}

func TestFenduduPendingDeliveryMetadata(t *testing.T) {
	s, store, p, _ := fenduduHarness(t)
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, fenduduRequest()))
	require.NoError(t, s.run(ctx, false))
	p.cycle.Status, p.cycle.PushProvider, p.cycle.PushConfigID = "paying", "", ""
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "processing", v.Jobs[0].Status, "missing pending metadata must not block polling")
	cycle := *p.cycle
	cycle.PushProvider = "other"
	require.Error(t, validateFenduduCycle(&cycle, v.Jobs[0]))
	cycle.PushProvider, cycle.PushConfigID = "sub2api", "s2e_other"
	require.Error(t, validateFenduduCycle(&cycle, v.Jobs[0]))
	p.cycle.Pushed = true
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "blocked", v.Jobs[0].Status, "pushed delivery requires complete matching metadata")
	require.Equal(t, 1, p.orders)
}

func TestFenduduConnectionSecretAndPendingProtection(t *testing.T) {
	s, store, _, _ := fenduduHarness(t)
	ctx := context.Background()
	require.NoError(t, s.ConfigureFendudu(ctx, "rvak_fixture", "s2e_fixture", 1))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, v.Revision)
	body, err := json.Marshal(v)
	require.NoError(t, err)
	require.NotContains(t, string(body), "rvak_fixture")
	require.NotContains(t, string(body), "encrypted_token")
	require.NotContains(t, string(body), "token_hash")
	require.NotContains(t, store.raw, "rvak_fixture")
	r := fenduduRequest()
	r.Revision = v.Revision
	require.NoError(t, s.Manual(ctx, r))
	require.ErrorContains(t, s.ConfigureFendudu(ctx, "rvak_other", "s2e_fixture", v.Revision), "在途订单")
	require.ErrorContains(t, s.ConfigureFendudu(ctx, "rvak_fixture", "s2e_other", v.Revision), "在途订单")
	require.ErrorContains(t, s.ConfigureFendudu(ctx, "rvak_fixture", "s2e_fixture", 1), "其他页面")
}

func TestFenduduQuotaRefreshIgnoresPrimaryBackoff(t *testing.T) {
	s, store, p, _ := fenduduHarness(t)
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, fenduduRequest()))
	require.NoError(t, s.run(ctx, false))
	p.available = 4
	store.mutate(t, func(state *accountRefillState) { state.ProviderRetryAt = time.Now().Add(time.Hour) })
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, v.Fendudu.Products[0].AvailableNow)
	p.available = 3
	require.NoError(t, s.Check(ctx))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, v.Fendudu.Products[0].AvailableNow)
	require.NotNil(t, v.Fendudu.CheckedAt)
	require.Equal(t, 1, p.orders, "refresh never submits another order")
}

func TestFenduduPauseResumesSavedImportAndLeaseSync(t *testing.T) {
	s, store, p, accounts := fenduduHarness(t)
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, fenduduRequest()))
	require.NoError(t, s.run(ctx, false))
	lease := time.Now().Add(time.Hour).Truncate(time.Second)
	p.cycle.Pushed, p.cycle.Status, p.cycle.AccountID, p.cycle.Email = true, "prewarming", "344", "fixture@example.test"
	p.cycle.RevokeAt = lease.Format(time.RFC3339)
	accounts.accounts = []Account{{ID: 344, Name: "supplier", CreatedAt: time.Now(), Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Credentials: map[string]any{"email": p.cycle.Email, "access_token": "fixture"}, Extra: map[string]any{}}}
	store.mutate(t, func(state *accountRefillState) { state.Config.PushEnabled = false })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "awaiting_push", v.Jobs[0].Status)
	require.False(t, accounts.accounts[0].Schedulable)
	require.Equal(t, "inactive", accounts.accounts[0].Status)
	store.mutate(t, func(state *accountRefillState) { state.Config.PushEnabled = true })
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	require.Equal(t, 1, accounts.adopt, "resumption must not allocate another proxy or apply newer settings")
	require.True(t, accounts.accounts[0].Schedulable)
	p.cycle.RevokeAt = lease.Add(time.Hour).Format(time.RFC3339)
	require.NoError(t, s.Check(ctx))
	require.Equal(t, lease.Add(time.Hour).Unix(), accounts.accounts[0].Extra[AccountSourceExpiryExtraKey])
	require.Equal(t, 1, p.orders)
}

func TestFenduduOrderStoppedDoesNotRetryOrAdvanceRotation(t *testing.T) {
	s, store, p, _ := fenduduHarness(t)
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, fenduduRequest()))
	require.NoError(t, s.run(ctx, false))
	p.cycle.Status = "order_stopped"
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "failed", v.Jobs[0].Status)
	require.True(t, v.Jobs[0].BudgetReleased)
	require.False(t, v.Jobs[0].ReplacementPending)
	require.Equal(t, 1, v.Daily.Count)
	require.Zero(t, v.RotationCursor)
	require.Equal(t, 1, p.orders)
}
