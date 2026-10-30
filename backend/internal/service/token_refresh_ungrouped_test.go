package service

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type ungroupedRefreshRepo struct {
	AccountRepository
	mu              sync.Mutex
	accounts        map[int64]*Account
	pages           []OAuthRefreshPageOptions
	assignOnRead    int64
	quotaWriteError bool
}

func copyUngroupedTestAccount(account *Account) *Account {
	result := snapshotOAuthRefreshAccount(account)
	result.Extra = shallowCopyMap(account.Extra)
	result.GroupIDs = slices.Clone(account.GroupIDs)
	return result
}

func (r *ungroupedRefreshRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	account := r.accounts[id]
	if account == nil {
		return nil, ErrAccountNotFound
	}
	if r.assignOnRead == id {
		account.GroupIDs = []int64{91}
	}
	return copyUngroupedTestAccount(account), nil
}

func (r *ungroupedRefreshRepo) ListOAuthRefreshCandidatePage(ctx context.Context, options OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pages = append(r.pages, options)
	ids := make([]int64, 0, len(r.accounts))
	for id := range r.accounts {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	page := &OAuthRefreshCandidatePage{}
	for _, id := range ids {
		account := r.accounts[id]
		if id <= options.AfterID || !slices.Contains(options.Platforms, account.Platform) ||
			(options.ActiveOnly && !account.IsActive()) || account.Type != AccountTypeOAuth ||
			(options.UngroupedOnly && len(account.GroupIDs) > 0) ||
			(options.RequireRefreshToken && account.GetOpenAIRefreshToken() == "") {
			continue
		}
		page.Accounts = append(page.Accounts, *copyUngroupedTestAccount(account))
		page.NextAfterID = id
		if len(page.Accounts) == options.Limit {
			page.HasMore = true
			break
		}
	}
	return page, nil
}

func (r *ungroupedRefreshRepo) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[id].Credentials = shallowCopyMap(credentials)
	return nil
}

func (r *ungroupedRefreshRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.quotaWriteError {
		return errors.New("database unavailable")
	}
	if r.accounts[id].Extra == nil {
		r.accounts[id].Extra = make(map[string]any)
	}
	for key, value := range updates {
		r.accounts[id].Extra[key] = value
	}
	return nil
}

func (r *ungroupedRefreshRepo) SetError(_ context.Context, id int64, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[id].Status, r.accounts[id].ErrorMessage = StatusError, message
	return nil
}

func (r *ungroupedRefreshRepo) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accounts[id].TempUnschedulableUntil, r.accounts[id].TempUnschedulableReason = &until, reason
	return nil
}

type ungroupedRefreshExecutor struct {
	mu    sync.Mutex
	calls []int64
	errs  map[int64]error
}

func (*ungroupedRefreshExecutor) CacheKey(a *Account) string { return OpenAITokenCacheKey(a) }
func (*ungroupedRefreshExecutor) CanRefresh(a *Account) bool { return a.IsOpenAIOAuth() }
func (*ungroupedRefreshExecutor) NeedsRefresh(*Account, time.Duration) bool {
	return false // Imported credentials are not near expiry; the new scan must still initialize them.
}
func (e *ungroupedRefreshExecutor) Refresh(_ context.Context, account *Account) (map[string]any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, account.ID)
	if err := e.errs[account.ID]; err != nil {
		return nil, err
	}
	return map[string]any{"access_token": "fresh-access", "refresh_token": "fresh-refresh", "expires_at": time.Now().Add(8 * time.Hour).Format(time.RFC3339)}, nil
}

type ungroupedQuotaReader struct {
	mu          sync.Mutex
	repo        *ungroupedRefreshRepo
	calls       []int64
	errForID    int64
	missing     bool
	blockID     int64
	started     chan struct{}
	startedOnce sync.Once
}

func (q *ungroupedQuotaReader) QueryUsage(ctx context.Context, id int64) (*OpenAIQuotaUsage, error) {
	account, err := q.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if account.GetOpenAIAccessToken() != "fresh-access" {
		return nil, errors.New("quota was queried before refreshed credentials were persisted")
	}
	q.mu.Lock()
	q.calls = append(q.calls, id)
	q.mu.Unlock()
	if id == q.blockID {
		q.startedOnce.Do(func() { close(q.started) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if id == q.errForID {
		return nil, errors.New("quota endpoint temporarily unavailable")
	}
	if q.missing {
		return &OpenAIQuotaUsage{}, nil
	}
	return &OpenAIQuotaUsage{
		FetchedAt: time.Now().Unix(),
		RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{UsedPercent: 25, LimitWindowSeconds: 18000, ResetAfterSeconds: 1000},
			SecondaryWindow: &OpenAIRateLimitWindow{UsedPercent: 40, LimitWindowSeconds: 604800, ResetAfterSeconds: 2000}},
	}, nil
}

func newUngroupedRefreshTestService(t *testing.T, count int) (*TokenRefreshService, *ungroupedRefreshRepo, *ungroupedRefreshExecutor, *ungroupedQuotaReader) {
	t.Helper()
	repo := &ungroupedRefreshRepo{accounts: make(map[int64]*Account)}
	for i := 1; i <= count; i++ {
		id := int64(i)
		repo.accounts[id] = &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			Concurrency: 7, Priority: 3, Schedulable: false,
			Credentials: map[string]any{"access_token": "imported-access", "refresh_token": "imported-refresh", "preset_field": "preserved"},
			Extra:       map[string]any{"privacy_mode": "training_opt_out", "preset_field": "preserved"}}
	}
	executor := &ungroupedRefreshExecutor{errs: make(map[int64]error)}
	quota := &ungroupedQuotaReader{repo: repo, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	s := &TokenRefreshService{
		accountRepo: repo, candidatePager: repo, refreshAPI: NewOAuthRefreshAPI(repo, nil), ungroupedQuota: quota,
		cfg:           &config.TokenRefreshConfig{Enabled: true, UngroupedCheckIntervalSeconds: 5, CandidatePageSize: 2, ProviderConcurrency: 1, MaxRetries: 1},
		registrations: []tokenRefreshRegistration{{platform: PlatformOpenAI, refresher: executor, executor: executor}},
		providerGates: map[string]*tokenRefreshRateGate{PlatformOpenAI: newTokenRefreshRateGateWithInterval(time.Nanosecond)},
		runCtx:        ctx, runCancel: cancel, stopCh: make(chan struct{}),
	}
	t.Cleanup(s.Stop)
	return s, repo, executor, quota
}

func TestUngroupedRefreshInitializesFreshTokensAndPersistsQuotaWithoutChangingPresets(t *testing.T) {
	s, repo, executor, quota := newUngroupedRefreshTestService(t, 9)
	repo.accounts[3].GroupIDs = []int64{91}
	repo.accounts[4].Type = AccountTypeAPIKey
	repo.accounts[5].Status = "disabled"
	repo.accounts[6].Platform = PlatformAnthropic
	parent := int64(2)
	repo.accounts[7].ParentAccountID = &parent
	repo.accounts[8].Credentials["auth_mode"] = OpenAIAuthModePersonalAccessToken
	until := time.Now().Add(time.Hour)
	repo.accounts[9].TempUnschedulableUntil = &until
	s.processUngroupedRefreshContext(context.Background())
	require.Equal(t, []int64{1, 2}, executor.calls)
	require.Equal(t, []int64{1, 2}, quota.calls)
	for _, id := range []int64{1, 2} {
		account := repo.accounts[id]
		require.Equal(t, "fresh-access", account.GetOpenAIAccessToken())
		require.Equal(t, "preserved", account.Credentials["preset_field"])
		require.Equal(t, "preserved", account.Extra["preset_field"])
		require.EqualValues(t, 25, account.Extra["codex_5h_used_percent"])
		require.EqualValues(t, 40, account.Extra["codex_7d_used_percent"])
		require.Equal(t, StatusActive, account.Status)
		require.False(t, account.Schedulable, "manual/preset pause must be preserved")
		require.Equal(t, 7, account.Concurrency)
		require.Equal(t, 3, account.Priority)
		require.Empty(t, account.GroupIDs)
	}
	for _, page := range repo.pages {
		require.True(t, page.UngroupedOnly)
		require.Equal(t, []string{PlatformOpenAI}, page.Platforms)
		require.LessOrEqual(t, page.Limit, 200)
	}

	s.processUngroupedRefreshContext(context.Background())
	require.Len(t, executor.calls, 2, "overlapping/restarted scans must not rotate a fresh token again")
	require.Len(t, quota.calls, 2)
	repo.accounts[1].Credentials[ungroupedOpenAIRefreshAtKey] = time.Now().Add(-time.Minute).UnixMilli()
	repo.accounts[1].Extra[ungroupedOpenAIQuotaAtKey] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	repo.accounts[2].GroupIDs = []int64{91}
	s.processUngroupedRefreshContext(context.Background())
	require.Equal(t, []int64{1, 2, 1}, executor.calls)
	require.Equal(t, []int64{1, 2, 1}, quota.calls)
}

func TestUngroupedRefreshRechecksAssignmentUnderCredentialLock(t *testing.T) {
	s, repo, executor, quota := newUngroupedRefreshTestService(t, 1)
	repo.assignOnRead = 1
	s.processUngroupedRefreshContext(context.Background())
	require.Empty(t, executor.calls)
	require.Empty(t, quota.calls)
	require.Equal(t, []int64{91}, repo.accounts[1].GroupIDs)
}

func TestUngroupedRefreshKeepsExistingRefreshErrorPolicyAndQuotaFailureIsNotAnAccountError(t *testing.T) {
	s, repo, executor, quota := newUngroupedRefreshTestService(t, 3)
	executor.errs[1] = errors.New("invalid_grant")
	quota.errForID = 2
	repo.accounts[2].Extra["codex_5h_used_percent"] = 12.0
	s.processUngroupedRefreshContext(context.Background())
	require.Equal(t, StatusError, repo.accounts[1].Status)
	require.Contains(t, repo.accounts[1].ErrorMessage, "invalid_grant")
	require.Equal(t, StatusActive, repo.accounts[2].Status)
	require.Empty(t, repo.accounts[2].ErrorMessage)
	require.EqualValues(t, 12, repo.accounts[2].Extra["codex_5h_used_percent"])
	require.NotContains(t, repo.accounts[2].Extra, ungroupedOpenAIQuotaAtKey)
	require.Contains(t, repo.accounts[3].Extra, ungroupedOpenAIQuotaAtKey)
	quota.errForID = 0
	s.processUngroupedRefreshContext(context.Background())
	require.Equal(t, []int64{1, 2, 3}, executor.calls)
	require.Contains(t, repo.accounts[2].Extra, ungroupedOpenAIQuotaAtKey, "retry failed quota read using the already refreshed token")
}

func TestUngroupedRefreshDoesNotPublishEmptyOrUnsavedQuota(t *testing.T) {
	for _, mode := range []string{"empty", "storage_error"} {
		t.Run(mode, func(t *testing.T) {
			s, repo, executor, quota := newUngroupedRefreshTestService(t, 1)
			quota.missing = mode == "empty"
			repo.quotaWriteError = mode == "storage_error"
			s.processUngroupedRefreshContext(context.Background())
			require.NotContains(t, repo.accounts[1].Extra, ungroupedOpenAIQuotaAtKey)
			require.NotContains(t, repo.accounts[1].Extra, "codex_5h_used_percent")
			require.Equal(t, StatusActive, repo.accounts[1].Status)
			quota.missing, repo.quotaWriteError = false, false
			s.processUngroupedRefreshContext(context.Background())
			require.Len(t, executor.calls, 1)
			require.Contains(t, repo.accounts[1].Extra, ungroupedOpenAIQuotaAtKey)
		})
	}
}

func TestUngroupedRefreshResumesPartialPageAndDoesNotOverlap(t *testing.T) {
	s, _, executor, quota := newUngroupedRefreshTestService(t, 3)
	quota.blockID = 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.processUngroupedRefreshContext(ctx) }()
	select {
	case <-quota.started:
	case <-time.After(2 * time.Second):
		t.Fatal("quota probe did not start")
	}
	s.processUngroupedRefreshContext(context.Background()) // Must skip the active cycle.
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scan ignored cancellation")
	}
	require.EqualValues(t, 1, s.ungroupedAfterID)
	quota.blockID = 0
	s.processUngroupedRefreshContext(context.Background())
	require.Equal(t, []int64{1, 2, 3}, executor.calls)
	require.Equal(t, []int64{1, 2, 2, 3}, quota.calls)
	require.Zero(t, s.ungroupedAfterID)
}

func TestUngroupedRefreshLoopFindsNewAccountsAndStopsInFlightQuotaRead(t *testing.T) {
	s, repo, _, quota := newUngroupedRefreshTestService(t, 1)
	account := repo.accounts[1]
	delete(repo.accounts, 1)
	quota.blockID = 1
	s.wg.Add(1)
	go s.ungroupedRefreshLoop(20 * time.Millisecond)
	require.Eventually(t, func() bool {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		return len(repo.pages) > 0
	}, time.Second, time.Millisecond)
	repo.mu.Lock()
	repo.accounts[1] = account // A push arrives after the initial empty scan.
	repo.mu.Unlock()
	select {
	case <-quota.started:
	case <-time.After(2 * time.Second):
		t.Fatal("periodic scan did not find the newly pushed account")
	}
	done := make(chan struct{})
	go func() { defer close(done); s.Stop() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the quota request")
	}
}

func TestUngroupedRefreshDisabled(t *testing.T) {
	s, repo, _, _ := newUngroupedRefreshTestService(t, 1)
	s.cfg.UngroupedCheckIntervalSeconds = 0
	s.processUngroupedRefreshContext(context.Background())
	require.Empty(t, repo.pages)
}
