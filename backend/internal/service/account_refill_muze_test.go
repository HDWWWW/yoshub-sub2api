//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type muzeTestProvider struct {
	t             *testing.T
	store         *refillTestStore
	available     int
	orders        int
	orderError    error
	failAfterPost bool
	cycle         *muzeCycle
}

const muzeFixtureToken = "tk_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func (p *muzeTestProvider) ResolveConnection(_ context.Context, _ string, id string) (string, error) {
	if id == "" {
		return "c_fixture", nil
	}
	return id, nil
}
func (p *muzeTestProvider) Products(context.Context, string) ([]MuzeProduct, error) {
	return []MuzeProduct{{Product: "pro20x", AvailableNow: p.available}}, nil
}
func (p *muzeTestProvider) Order(_ context.Context, _ string, j AccountRefillJob) (*muzeOrder, error) {
	state, err := decodeAccountRefillState(p.store.raw)
	require.NoError(p.t, err)
	require.NotNil(p.t, state.Jobs[0].SubmissionStartedAt, "persist the task ID and parameters before POST")
	require.Equal(p.t, j.ID, state.Jobs[0].ID)
	p.orders++
	p.store.failSave = p.failAfterPost
	p.cycle = &muzeCycle{ID: "rvc_fixture", RequestID: j.ID, Product: j.Plan, PushProvider: "sub2api", PushConfigID: j.MuzeConnectionID, Status: "using", CreatedAt: j.SubmissionStartedAt.Format(time.RFC3339Nano)}
	return &muzeOrder{ID: "rvc_fixture"}, p.orderError
}
func (p *muzeTestProvider) Cycle(context.Context, string, string) (*muzeCycle, error) {
	return p.cycle, nil
}

type muzeTestAdmin struct {
	*refillTestAccounts
	input  *CreateAccountInput
	adopt  int
	closed int
}

func (a *muzeTestAdmin) SyncMuzeAccountLease(ctx context.Context, j AccountRefillJob, cycle *muzeCycle, now time.Time) (*int64, error) {
	for i := range a.accounts {
		account := &a.accounts[i]
		if account.ID != j.AccountID {
			continue
		}
		if err := validateMuzePushedAccount(account, &j, cycle, now); err != nil {
			return nil, err
		}
		lease := muzeEffectiveLease(&j, cycle, account)
		expires := &lease
		updates := muzeLeaseUpdates(&j, account, *expires, now)
		updates["refill_job_id"], updates["refill_supplier_id"] = j.ID, j.SupplierID
		updates["refill_plan"], updates["import_source"] = j.Plan, "muze_api"
		ApplyAccountExpiryExtra(account, updates)
		return expires, nil
	}
	return nil, ErrAccountNotFound
}

func (a *muzeTestAdmin) AdoptRefillAccount(_ context.Context, id int64, in *CreateAccountInput) (*Account, error) {
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

func (a *muzeTestAdmin) RecordExpiredRefillAccount(_ context.Context, id int64, in *CreateAccountInput) error {
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

func muzeHarness(t *testing.T) (*AccountRefillService, *refillTestStore, *muzeTestProvider, *muzeTestAdmin) {
	s, store, _, accounts := refillHarness(t)
	p := &muzeTestProvider{t: t, store: store, available: 5}
	admin := &muzeTestAdmin{refillTestAccounts: accounts}
	s.muze, s.admin = p, admin
	store.mutate(t, func(state *accountRefillState) {
		state.Config.Enabled, state.Config.MonitorEnabled = false, false
		state.EncryptedToken = "" // Backup orders work without the primary key.
		state.Muze = muzeRefillState{EncryptedToken: "ciphertext", TokenHash: "same-wallet", ConnectionID: "s2e_fixture"}
	})
	return s, store, p, admin
}
func muzeRequest() ManualAccountRefillRequest {
	r := manualRequest()
	r.Provider = refillProviderMuze
	return r
}

func TestMuzeManualSnapshotAndPushedAccountRecovery(t *testing.T) {
	s, store, p, accounts := muzeHarness(t)
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
	r := muzeRequest()
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
	p.cycle.Pushed, p.cycle.AccountID, p.cycle.Email, p.cycle.RefundDueAt = true, "344", "fixture@example.test", lease.Format(time.RFC3339)
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
	require.Equal(t, "muze_api", a.Extra["import_source"])
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
	require.ErrorContains(t, err, "在账号列表导出")
}

func TestMuzeUncertainSubmissionNeverRepeats(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "crash-after-post"}[crash], func(t *testing.T) {
			s, store, p, _ := muzeHarness(t)
			p.failAfterPost = crash
			if !crash {
				p.orderError = errors.New("timeout")
			}
			require.NoError(t, s.Manual(context.Background(), muzeRequest()))
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
			require.NoError(t, s.ReconcileMuze(context.Background(), state.Jobs[0].ID, "rvc_fixture"))
			refillDue(t, store)
			require.NoError(t, s.run(context.Background(), false))
			require.Equal(t, 1, p.orders)
		})
	}
}

func TestMuzeManualGuardsAndForce(t *testing.T) {
	for _, scenario := range []string{"quota", "push-disabled", "revision", "daily-limit", "pending", "save-failed"} {
		t.Run(scenario, func(t *testing.T) {
			s, store, p, _ := muzeHarness(t)
			r := muzeRequest()
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

func TestMuzeRejectsWrongPushedAccountAndOldOrders(t *testing.T) {
	s, store, p, accounts := muzeHarness(t)
	require.NoError(t, s.Manual(context.Background(), muzeRequest()))
	require.NoError(t, s.run(context.Background(), false))
	p.cycle.Pushed, p.cycle.AccountID, p.cycle.Email, p.cycle.RefundDueAt = true, "344", "expected@example.test", time.Now().Add(time.Hour).Format(time.RFC3339)
	accounts.accounts = []Account{{ID: 344, Name: "keep", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"email": "other@example.test"}}}
	refillDue(t, store)
	require.NoError(t, s.run(context.Background(), false))
	require.Zero(t, accounts.adopt)
	require.Equal(t, "keep", accounts.accounts[0].Name)
	state, err := decodeAccountRefillState(store.raw)
	require.NoError(t, err)
	require.Equal(t, "import_failed", state.Jobs[0].Status)
	p.cycle.CreatedAt = time.Now().Add(-time.Hour).Format(time.RFC3339)
	require.Error(t, validateMuzeCycle(p.cycle, state.Jobs[0]))
}

func TestMuzeQuotaReservationsAcrossGroups(t *testing.T) {
	s, base, p, _ := muzeHarness(t)
	p.available = 1
	stores := &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{}}
	s.store = stores
	child, err := s.ForGroup(context.Background(), 3)
	require.NoError(t, err)
	v, err := child.View(context.Background())
	require.NoError(t, err)
	require.True(t, v.Muze.TokenConfigured)
	require.NoError(t, child.Configure(context.Background(), v.Config, "", 0))
	require.NoError(t, s.Manual(context.Background(), muzeRequest()))
	r := muzeRequest()
	r.GroupID = 3
	require.ErrorContains(t, child.Manual(context.Background(), r), "次数不足")
	require.Zero(t, p.orders)
}

func TestMuzeConnectionsInheritIndependently(t *testing.T) {
	for _, backupAtRoot := range []bool{true, false} {
		name := "primary_at_root"
		if backupAtRoot {
			name = "backup_at_root"
		}
		t.Run(name, func(t *testing.T) {
			s, base, _, _ := muzeHarness(t)
			other := &refillTestStore{raw: base.raw}
			base.mutate(t, func(state *accountRefillState) {
				if !backupAtRoot {
					state.EncryptedToken = "primary-cipher"
					state.Muze = muzeRefillState{}
				}
			})
			other.mutate(t, func(state *accountRefillState) {
				state.Config.GroupID = 3
				state.Jobs = []AccountRefillJob{{ID: "must-not-inherit", Status: "processing"}}
				if backupAtRoot {
					state.EncryptedToken = "primary-cipher"
					state.Muze = muzeRefillState{}
				}
			})
			s.store = &refillGroupedTestStore{refillTestStore: base, children: map[int64]*refillTestStore{3: other}}
			child := s.groupService(4, s.store.(AccountRefillGroupStore))
			raw, err := child.store.Read(context.Background())
			require.NoError(t, err)
			state, err := decodeAccountRefillState(raw)
			require.NoError(t, err)
			require.Equal(t, "primary-cipher", state.EncryptedToken)
			require.Equal(t, "ciphertext", state.Muze.EncryptedToken)
			require.Equal(t, "s2e_fixture", state.Muze.ConnectionID)
			require.Empty(t, state.Jobs)
			require.False(t, state.Config.Enabled)
			require.Equal(t, []int64{4}, state.Config.DeliveryGroupIDs)
		})
	}
}

func TestMuzePendingDeliveryMetadata(t *testing.T) {
	s, store, p, _ := muzeHarness(t)
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, muzeRequest()))
	require.NoError(t, s.run(ctx, false))
	p.cycle.Status, p.cycle.PushProvider, p.cycle.PushConfigID = "charging", "", ""
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "processing", v.Jobs[0].Status, "missing pending metadata must not block polling")
	cycle := *p.cycle
	cycle.PushProvider = "other"
	require.Error(t, validateMuzeCycle(&cycle, v.Jobs[0]))
	cycle.PushProvider, cycle.PushConfigID = "sub2api", "s2e_other"
	require.Error(t, validateMuzeCycle(&cycle, v.Jobs[0]))
	p.cycle.Pushed = true
	refillDue(t, store)
	require.NoError(t, s.run(ctx, false))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, "import_failed", v.Jobs[0].Status, "published push still needs a local identity")
	require.Equal(t, 1, p.orders)
}

func TestMuzeConnectionSecretAndPendingProtection(t *testing.T) {
	s, store, _, _ := muzeHarness(t)
	ctx := context.Background()
	require.NoError(t, s.ConfigureMuze(ctx, muzeFixtureToken, "s2e_fixture", 1))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 2, v.Revision)
	body, err := json.Marshal(v)
	require.NoError(t, err)
	require.NotContains(t, string(body), muzeFixtureToken)
	require.NotContains(t, string(body), "encrypted_token")
	require.NotContains(t, string(body), "token_hash")
	require.NotContains(t, store.raw, muzeFixtureToken)
	r := muzeRequest()
	r.Revision = v.Revision
	require.NoError(t, s.Manual(ctx, r))
	require.ErrorContains(t, s.ConfigureMuze(ctx, "tk_"+strings.Repeat("b", 64), "s2e_fixture", v.Revision), "在途订单")
	require.ErrorContains(t, s.ConfigureMuze(ctx, muzeFixtureToken, "s2e_other", v.Revision), "在途订单")
	require.ErrorContains(t, s.ConfigureMuze(ctx, muzeFixtureToken, "s2e_fixture", 1), "其他页面")
}

func TestMuzeQuotaRefreshIgnoresPrimaryBackoff(t *testing.T) {
	s, store, p, _ := muzeHarness(t)
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, muzeRequest()))
	require.NoError(t, s.run(ctx, false))
	p.available = 4
	store.mutate(t, func(state *accountRefillState) { state.ProviderRetryAt = time.Now().Add(time.Hour) })
	require.NoError(t, s.run(ctx, false))
	v, err := s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, v.Muze.Products[0].AvailableNow)
	p.available = 3
	require.NoError(t, s.Check(ctx))
	v, err = s.View(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, v.Muze.Products[0].AvailableNow)
	require.NotNil(t, v.Muze.CheckedAt)
	require.Equal(t, 1, p.orders, "refresh never submits another order")
}

func TestMuzePauseResumesSavedImportAndLeaseSync(t *testing.T) {
	s, store, p, accounts := muzeHarness(t)
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, muzeRequest()))
	require.NoError(t, s.run(ctx, false))
	lease := time.Now().Add(time.Hour).Truncate(time.Second)
	p.cycle.Pushed, p.cycle.Status, p.cycle.AccountID, p.cycle.Email = true, "using", "344", "fixture@example.test"
	p.cycle.RefundDueAt = lease.Format(time.RFC3339)
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
	p.cycle.RefundDueAt = lease.Add(time.Hour).Format(time.RFC3339)
	require.NoError(t, s.Check(ctx))
	require.Equal(t, lease.Add(time.Hour).Unix(), accounts.accounts[0].Extra[AccountSourceExpiryExtraKey])
	require.Equal(t, 1, p.orders)
}

func TestMuzeOrderStoppedDoesNotRetryOrAdvanceRotation(t *testing.T) {
	s, store, p, _ := muzeHarness(t)
	ctx := context.Background()
	require.NoError(t, s.Manual(ctx, muzeRequest()))
	require.NoError(t, s.run(ctx, false))
	p.cycle.Status, p.cycle.RefundPhase = "returned", "returned"
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
