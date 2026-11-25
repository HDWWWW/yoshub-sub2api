//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type riskTestStore struct {
	mu       sync.Mutex
	state    *AccountRiskState
	stopped  []int64
	resumed  []int64
	accounts *riskTestAccounts
	fail     bool
	busy     int
	cas      bool
}

func (s *riskTestStore) Lookup401AlertTarget(ctx context.Context, sourceID, ownerID int64) (Account401AlertTarget, error) {
	var target Account401AlertTarget
	for _, id := range []int64{sourceID, ownerID} {
		a, err := s.accounts.GetByID(ctx, id)
		if err != nil || a == nil || a.Platform != PlatformOpenAI || !a.IsActive() || !a.Schedulable {
			return Account401AlertTarget{}, err
		}
		if id == sourceID {
			target = Account401AlertTarget{AccountID: a.ID, Name: a.Name, GroupIDs: a.GroupIDs}
		}
	}
	return target, nil
}

func (s *riskTestStore) Read(context.Context) (*AccountRiskState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.state)
	var out AccountRiskState
	_ = json.Unmarshal(raw, &out)
	return &out, nil
}
func (s *riskTestStore) Update(_ context.Context, fn func(*AccountRiskState, AccountRiskScheduling) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy > 0 {
		s.busy--
		return ErrAccountRiskBusy
	}
	if s.fail {
		return errors.New("database unavailable")
	}
	stop := func(_ int64, a *Account) (bool, error) {
		s.accounts.mu.Lock()
		defer s.accounts.mu.Unlock()
		if !s.cas {
			return false, nil
		}
		s.stopped = append(s.stopped, a.ID)
		delete(s.state.CapacityPauses, a.ID)
		for i := range s.accounts.pool {
			if s.accounts.pool[i].ID == a.ID {
				s.accounts.pool[i].Schedulable = false
				delete(s.accounts.pool[i].Extra, AccountRiskCapacityPauseExtraKey)
			}
		}
		return true, nil
	}
	pause := func(_ int64, a *Account, token string) (bool, error) {
		s.accounts.mu.Lock()
		defer s.accounts.mu.Unlock()
		if !s.cas {
			return false, nil
		}
		for i := range s.accounts.pool {
			current := &s.accounts.pool[i]
			if current.ID == a.ID && current.Schedulable {
				current.Schedulable = false
				current.Extra = maps.Clone(current.Extra)
				if current.Extra == nil {
					current.Extra = map[string]any{}
				}
				current.Extra[AccountRiskCapacityPauseExtraKey] = token
				s.stopped = append(s.stopped, a.ID)
				return true, nil
			}
		}
		return false, nil
	}
	resume := func(_ int64, a *Account, token string) (bool, error) {
		s.accounts.mu.Lock()
		defer s.accounts.mu.Unlock()
		if !s.cas {
			return false, nil
		}
		for i := range s.accounts.pool {
			current := &s.accounts.pool[i]
			if current.ID == a.ID && !current.Schedulable && current.Extra[AccountRiskCapacityPauseExtraKey] == token {
				current.Schedulable = true
				delete(current.Extra, AccountRiskCapacityPauseExtraKey)
				s.resumed = append(s.resumed, a.ID)
				return true, nil
			}
		}
		return false, nil
	}
	apply401 := func(_ int64, _, a *Account, reason string, until *time.Time) (bool, error) {
		s.accounts.mu.Lock()
		defer s.accounts.mu.Unlock()
		for i := range s.accounts.pool {
			current := &s.accounts.pool[i]
			if !s.cas || current.ID != a.ID || !current.IsActive() {
				continue
			}
			if until == nil {
				current.Status, current.Schedulable, current.ErrorMessage = StatusError, false, reason
				delete(s.state.CapacityPauses, a.ID)
				delete(current.Extra, AccountRiskCapacityPauseExtraKey)
			} else {
				current.TempUnschedulableUntil = until
			}
			return true, nil
		}
		return false, nil
	}
	reduceConcurrency := func(_ int64, a *Account, limit int) (bool, error) {
		s.accounts.mu.Lock()
		defer s.accounts.mu.Unlock()
		for i := range s.accounts.pool {
			current := &s.accounts.pool[i]
			if s.cas && current.ID == a.ID && current.UpdatedAt.Equal(a.UpdatedAt) && canReduceRiskConcurrency(current, limit) {
				current.Concurrency = limit
				return true, nil
			}
		}
		return false, nil
	}
	return fn(s.state, AccountRiskScheduling{ApplyProbe401: apply401, ReduceConcurrency: reduceConcurrency, Stop: stop, Pause: pause, Resume: resume})
}

type riskTestAccounts struct {
	mu   sync.RWMutex
	pool []Account
	err  error
}

func (a *riskTestAccounts) ListByGroup(context.Context, int64) ([]Account, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]Account(nil), a.pool...), a.err
}

func (a *riskTestAccounts) GetByID(_ context.Context, id int64) (*Account, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, account := range a.pool {
		if account.ID == id {
			return &account, a.err
		}
	}
	return nil, a.err
}

func (a *riskTestAccounts) GetByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	var out []*Account
	for _, id := range ids {
		account, err := a.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if account != nil {
			out = append(out, account)
		}
	}
	return out, nil
}

type riskTestGroups struct {
	reverse  bool
	inactive bool
}

func (g *riskTestGroups) GetByID(_ context.Context, id int64) (*Group, error) {
	status := StatusActive
	if g.inactive {
		status = "inactive"
	}
	return &Group{ID: id, Status: status, Platform: PlatformOpenAI, ReverseAccountPriority: g.reverse}, nil
}

type riskTestLoads struct {
	first, second int
	byID          map[int64]int
	err           error
	missing       bool
	slots         int
	busy          bool
}

func (l *riskTestLoads) GetAccountsLoadBatchFresh(_ context.Context, as []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	m := map[int64]*AccountLoadInfo{}
	if !l.missing {
		for i, a := range as {
			n := l.first
			if i == 1 {
				n = l.second
			}
			if l.byID != nil {
				var ok bool
				n, ok = l.byID[a.ID]
				if !ok {
					continue
				}
			}
			m[a.ID] = &AccountLoadInfo{CurrentConcurrency: n}
		}
	}
	return m, l.err
}
func (l *riskTestLoads) AcquireAccountSlot(context.Context, int64, int) (*AcquireResult, error) {
	l.slots++
	return &AcquireResult{Acquired: !l.busy, ReleaseFunc: func() { l.slots-- }}, nil
}

type riskTestProbe struct {
	calls         int
	result        AccountRiskProbeResult
	err           error
	lastConfig    AccountRiskConfig
	lastAccountID int64
}

func (p *riskTestProbe) RunRiskProbe(_ context.Context, account *Account, config AccountRiskConfig) (AccountRiskProbeResult, error) {
	p.calls++
	p.lastConfig = config
	p.lastAccountID = account.ID
	return p.result, p.err
}

func riskHarness() (*AccountRiskService, *riskTestStore, *riskTestAccounts, *riskTestLoads, time.Time) {
	now := time.Now().UTC()
	c := DefaultAccountRiskConfig()
	c.Enabled = true
	store := &riskTestStore{state: &AccountRiskState{Rules: map[int64]*AccountRiskRule{7: {GroupID: 7, Revision: 1, Config: c}}}, cas: true}
	accounts := &riskTestAccounts{pool: []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Priority: 1, Concurrency: 100, GroupIDs: []int64{7}}, {ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Priority: 2, Concurrency: 100, GroupIDs: []int64{7}}}}
	store.accounts = accounts
	loads := &riskTestLoads{first: 59, second: 90}
	s := &AccountRiskService{store: store, accounts: accounts, groups: &riskTestGroups{}, loads: loads, probe: &riskTestProbe{}, cfg: &config.Config{}, worker: "one"}
	return s, store, accounts, loads, now
}

func TestAccountRiskContinuousCapacity(t *testing.T) {
	s, store, _, _, now := riskHarness()
	for second := 0; second < 10; second++ {
		job, err := riskSingleTick(t, s, context.Background(), now.Add(time.Duration(second)*time.Second))
		require.NoError(t, err)
		require.Nil(t, job)
		require.Empty(t, store.stopped)
	}
	job, err := riskSingleTick(t, s, context.Background(), now.Add(10*time.Second))
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, []int64{1}, store.stopped, "pause before dispatching the verification")
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "wrong"}, nil, now.Add(11*time.Second)))
	require.Equal(t, []int64{1}, store.stopped)
	require.Contains(t, store.state.Rules[7].Events[0].Reason, "59.0%")
}

func TestAccountRiskResetsAndNeverStopsOnUnknown(t *testing.T) {
	for _, kind := range []string{"first boundary", "second boundary", "sample gap", "load failure", "missing load", "account failure", "pair change", "capacity change", "disabled", "same priority", "worker change", "CAS change", "inactive group", "cooldown", "quota", "unlimited"} {
		t.Run(kind, func(t *testing.T) {
			s, store, accounts, loads, now := riskHarness()
			ctx := context.Background()
			for i := 0; i < 10; i++ {
				_, err := riskSingleTick(t, s, ctx, now.Add(time.Duration(i)*time.Second))
				require.NoError(t, err)
			}
			tick := now.Add(10 * time.Second)
			switch kind {
			case "first boundary":
				loads.first = 60
			case "second boundary":
				loads.second = 89
			case "sample gap":
				tick = now.Add(14 * time.Second)
			case "load failure":
				loads.err = errors.New("redis down")
			case "missing load":
				loads.missing = true
			case "account failure":
				accounts.err = errors.New("db down")
			case "pair change":
				accounts.pool[0].ID = 3
			case "capacity change":
				accounts.pool[0].Concurrency = 101
			case "disabled":
				store.state.Rules[7].Config.Enabled = false
			case "same priority":
				accounts.pool[1].Priority = 1
			case "worker change":
				s.worker = "two"
			case "CAS change":
				store.cas = false
			case "inactive group":
				s.groups.(*riskTestGroups).inactive = true
			case "cooldown":
				until := time.Now().Add(time.Hour)
				accounts.pool[0].TempUnschedulableUntil = &until
			case "quota":
				accounts.pool[0].Credentials = map[string]any{"account_scheduling_threshold": 99}
				accounts.pool[0].Extra = map[string]any{"codex_5h_used_percent": 100., "codex_5h_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339)}
			case "unlimited":
				accounts.pool[0].Concurrency = 0
			}
			job, err := riskSingleTick(t, s, ctx, tick)
			require.NoError(t, err)
			require.Nil(t, job)
			require.Empty(t, store.stopped)
		})
	}
}

func TestAccountRiskPriorityAndAccountIsolation(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	s.cfg.Gateway.Scheduling.AccountExpiryPriorityEnabled = true
	a, b := now.Add(2*time.Hour), now.Add(time.Hour)
	accounts.pool[0].ExpiresAt = &a
	accounts.pool[1].ExpiresAt = &b
	pool, err := s.candidates(context.Background(), 7, now)
	require.NoError(t, err)
	require.Equal(t, int64(2), pool[0].ID)
	require.Zero(t, accounts.pool[0].schedulingPriority, "never mutate shared account snapshots")
	s.groups.(*riskTestGroups).reverse = true
	pool, err = s.candidates(context.Background(), 7, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), pool[0].ID)
	store.state.Rules[7].Config.Enabled = false
	_, err = riskSingleTick(t, s, context.Background(), now)
	require.NoError(t, err)
	require.Empty(t, store.stopped)
}

func riskDueJob(t *testing.T, s *AccountRiskService, accounts *riskTestAccounts, now time.Time) *accountRiskJob {
	t.Helper()
	accounts.pool = accounts.pool[:1]
	job, err := riskSingleTick(t, s, context.Background(), now)
	require.NoError(t, err)
	require.Nil(t, job, "wait one full interval before first test")
	job, err = riskSingleTick(t, s, context.Background(), now.Add(300*time.Second))
	require.NoError(t, err)
	require.NotNil(t, job)
	return job
}

func TestAccountRiskSingleResultGuards(t *testing.T) {
	for _, kind := range []string{"missing text", "expected text", "case sensitive", "network error", "incomplete", "new account", "credential change", "disabled while testing", "revision changed", "CAS changed"} {
		t.Run(kind, func(t *testing.T) {
			s, store, accounts, _, now := riskHarness()
			job := riskDueJob(t, s, accounts, now)
			result := AccountRiskProbeResult{Completed: true, Text: "wrong", InputTokens: 4, OutputTokens: 1}
			var probeErr error
			switch kind {
			case "expected text":
				result.Text = "answer 21 OK"
			case "case sensitive":
				store.state.Rules[7].Config.ExpectedText = "YES"
				result.Text = "yes"
			case "network error":
				probeErr = errors.New("reset")
			case "incomplete":
				result.Completed = false
			case "new account":
				accounts.pool = append(accounts.pool, Account{ID: 8, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 100})
			case "credential change":
				accounts.pool[0].Credentials = map[string]any{"access_token": "rotated"}
			case "disabled while testing":
				store.state.Rules[7].Config.Enabled = false
			case "revision changed":
				store.state.Rules[7].Revision++
			case "CAS changed":
				store.cas = false
			}
			require.NoError(t, s.finishProbe(context.Background(), job, result, probeErr, now.Add(301*time.Second)))
			require.Empty(t, store.state.ActiveProbes)
			if kind == "missing text" || kind == "case sensitive" {
				require.Equal(t, []int64{1}, store.stopped)
			} else {
				require.Empty(t, store.stopped)
			}
		})
	}
}

func TestAccountRiskProbeReservedAcrossInstancesAndConfigChanges(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	job := riskDueJob(t, s, accounts, now)
	s2 := *s
	s2.worker = "second"
	other, err := riskSingleTick(t, &s2, context.Background(), now.Add(301*time.Second))
	require.NoError(t, err)
	require.Nil(t, other)
	c := store.state.Rules[7].Config
	c.ProbePrompt = "another question"
	require.NoError(t, s.Configure(context.Background(), 7, 1, c))
	require.NotEmpty(t, store.state.ActiveProbes)
	require.ErrorIs(t, s.Configure(context.Background(), 7, 1, c), ErrAccountRiskConflict)
	require.NoError(t, s.finishProbe(context.Background(), job, AccountRiskProbeResult{Completed: true, Text: "bad"}, nil, now.Add(302*time.Second)))
	require.Empty(t, store.stopped)
	job, err = riskSingleTick(t, s, context.Background(), now.Add(303*time.Second))
	require.NoError(t, err)
	require.Nil(t, job)
}

func TestAccountRiskProbeBusyAndDispatchOnce(t *testing.T) {
	for _, busy := range []bool{false, true} {
		s, store, accounts, loads, now := riskHarness()
		job := riskDueJob(t, s, accounts, now)
		loads.busy = busy
		probe := s.probe.(*riskTestProbe)
		probe.result = AccountRiskProbeResult{Completed: true, Text: "21"}
		s.runProbe(context.Background(), job)
		if busy {
			require.Zero(t, probe.calls)
		} else {
			require.Equal(t, 1, probe.calls)
			require.Zero(t, loads.slots)
		}
		require.Empty(t, store.stopped)
	}
}

func TestAccountRiskProbeResultSaveBusyDoesNotRedispatch(t *testing.T) {
	s, store, accounts, _, now := riskHarness()
	job := riskDueJob(t, s, accounts, now)
	store.busy = 2
	probe := s.probe.(*riskTestProbe)
	probe.result = AccountRiskProbeResult{Completed: true, Text: "wrong"}
	s.runProbe(context.Background(), job)
	require.Equal(t, 1, probe.calls)
	require.Equal(t, []int64{1}, store.stopped)
	require.Empty(t, store.state.ActiveProbes)
}

func TestAccountRiskProbeUsesCustomPayloadWithoutSideEffects(t *testing.T) {
	for _, status := range []int{200, 307, 308, 401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resp := newJSONResponse(status, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"21\"}]}]}}\n\n")
			resp.Header.Set("x-codex-primary-used-percent", "99")
			up := &queuedHTTPUpstream{responses: []*http.Response{resp}}
			repo := &openAIAccountTestRepo{}
			s := &AccountTestService{httpUpstream: up, accountRepo: repo}
			a := &Account{ID: 9, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 3, Credentials: map[string]any{"access_token": "test-token"}}
			c := DefaultAccountRiskConfig()
			c.ProbePrompt = "custom question"
			c.ReasoningEffort = "xhigh"
			c.Model = "gpt-6.1-sol"
			result, err := s.RunRiskProbe(context.Background(), a, c)
			require.Equal(t, status, result.UpstreamStatus)
			if status == 200 {
				require.NoError(t, err)
				require.Equal(t, "21", result.Text)
				require.True(t, result.Completed)
			} else {
				require.Error(t, err)
				require.False(t, result.Completed)
			}
			require.Len(t, up.requests, 1)
			require.True(t, HTTPUpstreamRedirectsDisabled(up.requests[0].Context()))
			raw, err := io.ReadAll(up.requests[0].Body)
			require.NoError(t, err)
			require.Equal(t, "custom question", gjson.GetBytes(raw, "input.0.content.0.text").String())
			require.Equal(t, "xhigh", gjson.GetBytes(raw, "reasoning.effort").String())
			require.Equal(t, c.Model, gjson.GetBytes(raw, "model").String())
			require.Empty(t, repo.updatedExtra)
			require.Zero(t, repo.setErrorID)
			require.Zero(t, repo.rateLimitedID)
		})
	}
}

func TestAccountRiskProbeTerminalParsing(t *testing.T) {
	for _, tc := range []struct {
		name, body, text   string
		chat, jsonBody, ok bool
	}{
		{"complete", `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":21,"output_tokens":1}}}` + "\n\n", "answer", false, false, true},
		{"reasoning excluded", `data: {"type":"response.reasoning_text.delta","delta":"21"}` + "\n\ndata: " + `{"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n", "", false, false, true},
		{"interrupted", `data: {"type":"response.output_text.delta","delta":"21"}` + "\n\n", "", false, false, false},
		{"incomplete", `data: {"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n", "", false, false, false},
		{"business error", `data: {"type":"error","error":{"message":"21"}}` + "\n\n", "", false, false, false},
		{"missing status", `data: {"type":"response.completed"}` + "\n\n", "", false, false, false},
		{"JSON", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"21"}]}]}`, "21", false, true, true},
		{"chat", `data: {"choices":[{"delta":{"content":"21"},"finish_reason":"stop"}]}` + "\n\n", "21", true, false, true},
		{"truncated chat", `data: {"choices":[{"delta":{"content":"21"},"finish_reason":"length"}]}` + "\n\n", "", true, false, false},
		{"large frame", "data: " + strings.Repeat("x", (1<<20)+1) + "\n\n", "", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseRiskProbeResponse(strings.NewReader(tc.body), tc.chat, tc.jsonBody)
			if tc.ok {
				require.NoError(t, err)
				require.True(t, r.Completed)
				require.Equal(t, tc.text, r.Text)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestAccountRiskConfigValidation(t *testing.T) {
	good := DefaultAccountRiskConfig()
	require.NoError(t, good.Validate())
	for _, mutate := range []func(*AccountRiskConfig){func(c *AccountRiskConfig) { c.FirstBelowPercent = 0 }, func(c *AccountRiskConfig) { c.SecondAtLeastPercent = 101 }, func(c *AccountRiskConfig) { c.SustainSeconds = 0 }, func(c *AccountRiskConfig) { c.ProbeIntervalSeconds = 9 }, func(c *AccountRiskConfig) { c.ProbePrompt = " " }, func(c *AccountRiskConfig) { c.ExpectedText = "" }, func(c *AccountRiskConfig) { c.Model = "bad\nmodel" }, func(c *AccountRiskConfig) { c.ReasoningEffort = "invalid" }} {
		c := good
		mutate(&c)
		require.Error(t, c.Validate())
	}
}

func riskSingleTick(t *testing.T, s *AccountRiskService, ctx context.Context, now time.Time) (*accountRiskJob, error) {
	t.Helper()
	jobs, err := s.tick(ctx, now)
	require.LessOrEqual(t, len(jobs), 1)
	if len(jobs) == 0 {
		return nil, err
	}
	return jobs[0], err
}
