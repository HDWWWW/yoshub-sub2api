//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type autoFastTestAccounts struct {
	pool map[int64]*Account
	err  error
}

func (a *autoFastTestAccounts) ListByGroup(_ context.Context, id int64) ([]Account, error) {
	var out []Account
	for _, v := range a.pool {
		if slices.Contains(v.GroupIDs, id) {
			out = append(out, *v)
		}
	}
	return out, a.err
}
func (a *autoFastTestAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	if a.err != nil {
		return nil, a.err
	}
	if a.pool[id] == nil {
		return nil, ErrAccountNotFound
	}
	return a.pool[id], nil
}

type autoFastTestGroups struct {
	groups map[int64]*Group
	err    error
}

func (g *autoFastTestGroups) GetByID(_ context.Context, id int64) (*Group, error) {
	if g.err != nil {
		return nil, g.err
	}
	if g.groups[id] == nil {
		return nil, ErrGroupNotFound
	}
	return g.groups[id], nil
}

// Model transaction isolation: state and flag changes only become visible on commit.
type autoFastTestStore struct {
	mu       sync.Mutex
	state    *AccountAutoFastState
	accounts *autoFastTestAccounts
	cas      bool
	err      error
	writes   int
}

func (s *autoFastTestStore) Read(context.Context) (*AccountAutoFastState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	b, _ := json.Marshal(s.state)
	var state AccountAutoFastState
	_ = json.Unmarshal(b, &state)
	return &state, nil
}
func (s *autoFastTestStore) Update(_ context.Context, fn func(*AccountAutoFastState, AccountAutoFastApply) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.state)
	var state AccountAutoFastState
	_ = json.Unmarshal(b, &state)
	changes := map[int64]bool{}
	err := fn(&state, func(_ int64, a *Account, on bool) (bool, error) {
		if !s.cas {
			return false, nil
		}
		changes[a.ID] = on
		return true, nil
	})
	if err != nil {
		return err
	}
	if s.err != nil {
		return s.err
	}
	for id, on := range changes {
		s.accounts.pool[id].Extra[OpenAIForceFastExtraKey] = on
		s.writes++
	}
	s.state = &state
	return nil
}

func autoFastHarness() (*AccountAutoFastService, *autoFastTestStore, *Account, time.Time) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	deadline := now.Add(45 * time.Minute)
	a := &Account{ID: 1, Name: "fixture", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Schedulable: true, GroupIDs: []int64{7}, ExpiresAt: &deadline,
		Extra:       map[string]any{"codex_7d_used_percent": 40., "codex_usage_updated_at": now.Format(time.RFC3339), "unrelated": "keep"},
		Credentials: map[string]any{"unchanged": "credential"}}
	accounts := &autoFastTestAccounts{pool: map[int64]*Account{1: a}}
	c := DefaultAccountAutoFastConfig()
	c.Enabled = true
	store := &autoFastTestStore{state: &AccountAutoFastState{Rules: map[int64]*AccountAutoFastRule{7: {GroupID: 7, Revision: 1, Config: c}}, Managed: map[int64]AccountAutoFastManaged{}}, accounts: accounts, cas: true}
	s := &AccountAutoFastService{store: store, accounts: accounts, groups: &autoFastTestGroups{groups: map[int64]*Group{
		7: {ID: 7, Platform: PlatformOpenAI, Status: StatusActive}, 8: {ID: 8, Platform: PlatformOpenAI, Status: StatusActive}}}}
	return s, store, a, now
}

func TestAccountAutoFastStagesAndExactBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		used      float64
		want      *bool
		stage     int
	}{
		{"before first", 45*time.Minute + time.Second, 20, nil, 0},
		{"45 opens", 45 * time.Minute, 49.9, ptrBool(true), 45},
		{"45 equals closes", 45 * time.Minute, 50, ptrBool(false), 45},
		{"30 opens", 30 * time.Minute, 64.9, ptrBool(true), 30},
		{"30 equals closes", 30 * time.Minute, 65, ptrBool(false), 30},
		{"15 opens", 15 * time.Minute, 79.9, ptrBool(true), 15},
		{"15 equals closes", 15 * time.Minute, 80, ptrBool(false), 15},
		{"before final still off", 5*time.Minute + time.Second, 100, ptrBool(false), 15},
		{"final unconditional", 5 * time.Minute, 100, ptrBool(true), 5},
		{"expired exits", 0, 0, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, a, now := autoFastHarness()
			at := now.Add(tc.remaining)
			a.ExpiresAt = &at
			a.Extra["codex_7d_used_percent"] = tc.used
			o, on := evaluateAutoFast(a, store.state.Rules[7].Config, now)
			require.Equal(t, tc.want, on)
			require.Equal(t, tc.stage, o.Stage)
		})
	}
}

func ptrBool(v bool) *bool { return &v }

func TestAccountAutoFastDeadlineSource(t *testing.T) {
	for _, key := range []string{AccountSourceExpiryExtraKey, "import_original_expires_at", "local", "missing", "inactive", "other platform"} {
		t.Run(key, func(t *testing.T) {
			_, store, a, now := autoFastHarness()
			expiry := now.Add(2 * time.Hour)
			a.ExpiresAt = &expiry
			source := now.Add(30 * time.Minute)
			a.Extra[AccountSourceExpiryOriginExtraKey] = "supplier_api"
			switch key {
			case "local":
				a.ExpiresAt = &source
			case "missing":
				a.ExpiresAt = nil
			case "inactive":
				a.Status = "disabled"
			case "other platform":
				a.Platform = PlatformAnthropic
			default:
				a.Extra[key] = source.Unix()
			}
			o, on := evaluateAutoFast(a, store.state.Rules[7].Config, now)
			if key == "missing" || key == "inactive" || key == "other platform" {
				require.Nil(t, on)
				return
			}
			require.Equal(t, ptrBool(true), on)
			require.Equal(t, source, *o.Deadline)
			if key == AccountSourceExpiryExtraKey {
				require.Equal(t, "supplier_api", o.Source)
			}
			if key == "local" {
				require.Equal(t, "local", o.Source)
			}
		})
	}
}

func TestAccountAutoFastUnknownQuotaNeverBecomesZero(t *testing.T) {
	for _, kind := range []string{"missing", "stale", "invalid time", "future time", "negative", "over100", "nan", "infinite", "invalid string", "reset expired", "reset invalid", "relative expired", "relative invalid", "relative malformed"} {
		t.Run(kind, func(t *testing.T) {
			_, store, a, now := autoFastHarness()
			switch kind {
			case "missing":
				delete(a.Extra, "codex_7d_used_percent")
			case "stale":
				a.Extra["codex_usage_updated_at"] = now.Add(-301 * time.Second).Format(time.RFC3339)
			case "invalid time":
				a.Extra["codex_usage_updated_at"] = "invalid"
			case "future time":
				a.Extra["codex_usage_updated_at"] = now.Add(2 * time.Minute).Format(time.RFC3339)
			case "negative":
				a.Extra["codex_7d_used_percent"] = -1.
			case "over100":
				a.Extra["codex_7d_used_percent"] = 101.
			case "nan":
				a.Extra["codex_7d_used_percent"] = math.NaN()
			case "infinite":
				a.Extra["codex_7d_used_percent"] = math.Inf(1)
			case "invalid string":
				a.Extra["codex_7d_used_percent"] = "oops"
			case "reset expired":
				a.Extra["codex_7d_reset_at"] = now.Format(time.RFC3339)
			case "reset invalid":
				a.Extra["codex_7d_reset_at"] = "invalid"
			case "relative expired":
				a.Extra["codex_usage_updated_at"] = now.Add(-time.Minute).Format(time.RFC3339)
				a.Extra["codex_7d_reset_after_seconds"] = 30.
			case "relative invalid":
				a.Extra["codex_7d_reset_after_seconds"] = -1.
			case "relative malformed":
				a.Extra["codex_7d_reset_after_seconds"] = "invalid"
			}
			o, on := evaluateAutoFast(a, store.state.Rules[7].Config, now)
			require.Nil(t, on)
			require.Equal(t, "quota_unknown", o.Mode)
			require.Nil(t, o.Used)
			at := now.Add(5 * time.Minute)
			a.ExpiresAt = &at
			o, on = evaluateAutoFast(a, store.state.Rules[7].Config, now)
			require.Equal(t, ptrBool(true), on)
			require.Equal(t, "final", o.Mode)
		})
	}
}

func TestAccountAutoFastLifecyclePreservesAccountSemantics(t *testing.T) {
	s, store, a, now := autoFastHarness()
	ctx := context.Background()
	for _, tc := range []struct {
		elapsed time.Duration
		used    float64
		want    bool
	}{
		{0, 40, true}, {15 * time.Minute, 70, false}, {30 * time.Minute, 75, true}, {40 * time.Minute, 100, true}, {45 * time.Minute, 100, false},
	} {
		at := now.Add(tc.elapsed)
		a.Extra["codex_usage_updated_at"] = at.Format(time.RFC3339)
		a.Extra["codex_7d_used_percent"] = tc.used
		require.NoError(t, s.tick(ctx, at))
		require.Equal(t, tc.want, a.IsOpenAIForceFastEnabled())
		require.Equal(t, tc.want, store.state.Rules[7].Accounts[0].Fast)
	}
	require.Empty(t, store.state.Managed)
	require.Len(t, store.state.Rules[7].Events, 4)
	require.Equal(t, "keep", a.Extra["unrelated"])
	require.True(t, a.Schedulable)
	require.Equal(t, StatusActive, a.Status)
	require.Equal(t, map[string]any{"unchanged": "credential"}, a.Credentials)
}

func TestAccountAutoFastExitsAndRestores(t *testing.T) {
	for _, kind := range []string{"disabled", "renewed", "left group", "inactive group", "deleted group", "conflicting groups", "deleted account", "manual edit"} {
		t.Run(kind, func(t *testing.T) {
			s, store, a, now := autoFastHarness()
			ctx := context.Background()
			require.NoError(t, s.tick(ctx, now))
			require.True(t, a.IsOpenAIForceFastEnabled())
			switch kind {
			case "disabled":
				c := store.state.Rules[7].Config
				c.Enabled = false
				require.NoError(t, s.Configure(ctx, 7, 1, c))
			case "renewed":
				at := now.Add(2 * time.Hour)
				a.ExpiresAt = &at
			case "left group":
				a.GroupIDs = nil
			case "inactive group":
				s.groups.(*autoFastTestGroups).groups[7].Status = "disabled"
			case "deleted group":
				delete(s.groups.(*autoFastTestGroups).groups, 7)
			case "conflicting groups":
				a.GroupIDs = []int64{7, 8}
				c := store.state.Rules[7].Config
				store.state.Rules[8] = &AccountAutoFastRule{GroupID: 8, Config: c}
			case "deleted account":
				delete(store.accounts.pool, 1)
			case "manual edit":
				a.Extra[OpenAIForceFastExtraKey] = false
				a.GroupIDs = nil
			}
			require.NoError(t, s.tick(ctx, now.Add(10*time.Second)))
			require.Empty(t, store.state.Managed)
			if kind != "deleted account" {
				require.False(t, a.IsOpenAIForceFastEnabled())
			}
			if kind == "conflicting groups" {
				require.Equal(t, "conflict", store.state.Rules[7].Accounts[0].Mode)
				require.False(t, store.state.Rules[7].Accounts[0].Fast)
			}
		})
	}
}

func TestAccountAutoFastPreservesOriginalTrueAndManualOverride(t *testing.T) {
	for _, manual := range []bool{false, true} {
		s, store, a, now := autoFastHarness()
		ctx := context.Background()
		a.Extra[OpenAIForceFastExtraKey] = true
		a.Extra["codex_7d_used_percent"] = 100.
		require.NoError(t, s.tick(ctx, now))
		require.False(t, a.IsOpenAIForceFastEnabled())
		if manual {
			a.Extra[OpenAIForceFastExtraKey] = true
		}
		c := store.state.Rules[7].Config
		c.Enabled = false
		require.NoError(t, s.Configure(ctx, 7, 1, c))
		require.True(t, a.IsOpenAIForceFastEnabled())
		require.Empty(t, store.state.Managed)
	}
}

func TestAccountAutoFastSampleGapKeepsManagedValue(t *testing.T) {
	s, store, a, now := autoFastHarness()
	ctx := context.Background()
	require.NoError(t, s.tick(ctx, now))
	require.NoError(t, s.tick(ctx, now.Add(301*time.Second)))
	require.True(t, a.IsOpenAIForceFastEnabled())
	require.Len(t, store.state.Managed, 1)
	require.Equal(t, "quota_unknown", store.state.Rules[7].Accounts[0].Mode)
}

func TestAccountAutoFastCASFailureAndAtomicErrors(t *testing.T) {
	for _, kind := range []string{"cas", "commit", "list"} {
		t.Run(kind, func(t *testing.T) {
			s, store, a, now := autoFastHarness()
			switch kind {
			case "cas":
				store.cas = false
			case "commit":
				store.err = errors.New("db down")
			case "list":
				store.accounts.err = errors.New("list failed")
			}
			err := s.tick(context.Background(), now)
			if kind != "cas" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.False(t, a.IsOpenAIForceFastEnabled())
			require.Empty(t, store.state.Managed)
			require.Empty(t, store.state.Rules[7].Events)
			store.cas, store.err, store.accounts.err = true, nil, nil
			require.NoError(t, s.tick(context.Background(), now.Add(10*time.Second)))
			require.True(t, a.IsOpenAIForceFastEnabled())
		})
	}
}

func TestAccountAutoFastFailedRestoreRetriesAfterDisable(t *testing.T) {
	s, store, a, now := autoFastHarness()
	ctx := context.Background()
	require.NoError(t, s.tick(ctx, now))
	store.cas = false
	c := store.state.Rules[7].Config
	c.Enabled = false
	require.NoError(t, s.Configure(ctx, 7, 1, c))
	require.Len(t, store.state.Managed, 1)
	require.True(t, a.IsOpenAIForceFastEnabled())
	store.cas = true
	require.NoError(t, s.tick(ctx, now.Add(10*time.Second)))
	require.Empty(t, store.state.Managed)
	require.False(t, a.IsOpenAIForceFastEnabled())
}

func TestAccountAutoFastConfigureOverlapRevisionAndDefaults(t *testing.T) {
	s, store, a, _ := autoFastHarness()
	ctx := context.Background()
	view, err := s.View(ctx, 8)
	require.NoError(t, err)
	require.False(t, view.Config.Enabled)
	a.GroupIDs = []int64{7, 8}
	c := DefaultAccountAutoFastConfig()
	c.Enabled = true
	err = s.Configure(ctx, 8, 0, c)
	var invalid *AccountAutoFastConfigError
	require.ErrorAs(t, err, &invalid)
	require.Contains(t, err.Error(), "#1")
	require.Nil(t, store.state.Rules[8])
	err = s.Configure(ctx, 7, 0, c)
	require.ErrorIs(t, err, ErrAccountAutoFastConflict)
	c.Enabled = false
	require.NoError(t, s.Configure(ctx, 7, 1, c))
	require.EqualValues(t, 2, store.state.Rules[7].Revision)
	c.Enabled = true
	require.NoError(t, s.Configure(ctx, 8, 0, c))
	c.Stages[0].Minutes = 100
	require.Equal(t, 45, store.state.Rules[8].Config.Stages[0].Minutes)
}

func TestAccountAutoFastWorkersShareTenSecondGate(t *testing.T) {
	s, store, a, now := autoFastHarness()
	second := &AccountAutoFastService{store: store, accounts: s.accounts, groups: s.groups}
	var wg sync.WaitGroup
	for _, worker := range []*AccountAutoFastService{s, second} {
		wg.Add(1)
		go func(w *AccountAutoFastService) {
			defer wg.Done()
			require.NoError(t, w.tick(context.Background(), now))
		}(worker)
	}
	wg.Wait()
	require.Equal(t, 1, store.writes)
	a.Extra["codex_7d_used_percent"] = 100.
	require.NoError(t, second.tick(context.Background(), now.Add(9*time.Second)))
	require.True(t, a.IsOpenAIForceFastEnabled())
	require.NoError(t, second.tick(context.Background(), now.Add(10*time.Second)))
	require.False(t, a.IsOpenAIForceFastEnabled())
}

func TestAccountAutoFastConfigValidation(t *testing.T) {
	for _, kind := range []string{"empty", "too many", "duplicate", "ascending", "zero", "too long", "negative percent", "over percent", "nan", "final zero", "final overlaps", "age low", "age high"} {
		t.Run(kind, func(t *testing.T) {
			c := DefaultAccountAutoFastConfig()
			switch kind {
			case "empty":
				c.Stages = nil
			case "too many":
				c.Stages = make([]AccountAutoFastStage, 13)
			case "duplicate":
				c.Stages[1].Minutes = 45
			case "ascending":
				c.Stages[1].Minutes = 46
			case "zero":
				c.Stages[2].Minutes = 0
			case "too long":
				c.Stages[0].Minutes = 10081
			case "negative percent":
				c.Stages[0].UsedBelowPercent = -1
			case "over percent":
				c.Stages[0].UsedBelowPercent = 101
			case "nan":
				c.Stages[0].UsedBelowPercent = math.NaN()
			case "final zero":
				c.FinalMinutes = 0
			case "final overlaps":
				c.FinalMinutes = 15
			case "age low":
				c.UsageMaxAgeSeconds = 29
			case "age high":
				c.UsageMaxAgeSeconds = 86401
			}
			require.Error(t, c.Validate())
		})
	}
	require.NoError(t, DefaultAccountAutoFastConfig().Validate())
}
