package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

const (
	ungroupedOpenAIRefreshAtKey = "_ungrouped_refresh_at_ms"
	ungroupedOpenAIQuotaAtKey   = "ungrouped_quota_checked_at"
)

type ungroupedOpenAIQuotaReader interface {
	QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error)
}

// ungroupedOpenAIRefreshExecutor forces fresh credentials for imported accounts,
// even when expires_at is absent or distant. OAuthRefreshAPI still owns the
// credential lock, database re-read and persistence. The timestamp prevents two
// scanners from rotating the same token again within one scan interval.
type ungroupedOpenAIRefreshExecutor struct {
	OAuthRefreshExecutor
	interval time.Duration
}

func ungroupedOpenAIRefreshEligible(account *Account) bool {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeOAuth ||
		!account.IsActive() || len(account.GroupIDs) != 0 || len(account.AccountGroups) != 0 || len(account.Groups) != 0 ||
		account.IsCredentialShadow() || account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() ||
		strings.TrimSpace(account.GetOpenAIRefreshToken()) == "" {
		return false
	}
	now := time.Now()
	if account.AutoPauseOnExpired && account.ExpiresAt != nil && !account.ExpiresAt.After(now) {
		return false
	}
	return account.TempUnschedulableUntil == nil || !account.TempUnschedulableUntil.After(now)
}

func (e *ungroupedOpenAIRefreshExecutor) CanRefresh(account *Account) bool {
	return ungroupedOpenAIRefreshEligible(account) && e.OAuthRefreshExecutor.CanRefresh(account)
}

func (e *ungroupedOpenAIRefreshExecutor) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !e.CanRefresh(account) {
		return false
	}
	last := account.GetCredentialAsInt64(ungroupedOpenAIRefreshAtKey)
	return last <= 0 || time.Since(time.UnixMilli(last)) >= e.interval
}

func (e *ungroupedOpenAIRefreshExecutor) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	credentials, err := e.OAuthRefreshExecutor.Refresh(ctx, account)
	if err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, errors.New("ungrouped OAuth refresh returned no credentials")
	}
	credentials = MergeCredentials(account.Credentials, credentials)
	credentials[ungroupedOpenAIRefreshAtKey] = time.Now().UnixMilli()
	return credentials, nil
}

func (s *TokenRefreshService) ungroupedRefreshInterval() time.Duration {
	if s.cfg == nil || s.cfg.UngroupedCheckIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(min(s.cfg.UngroupedCheckIntervalSeconds, 3600)) * time.Second
}

func (s *TokenRefreshService) ungroupedRefreshLoop(interval time.Duration) {
	defer s.wg.Done()
	ctx := s.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.processUngroupedRefreshContext(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.processUngroupedRefreshContext(ctx)
		}
	}
}

// Scans use keyset pagination and a separate cursor from expiry maintenance.
// Cycles do not overlap; when a bounded cycle ends, the next resumes after the
// completed prefix instead of repeatedly refreshing the start of a large pool.
func (s *TokenRefreshService) processUngroupedRefreshContext(parent context.Context) {
	interval := s.ungroupedRefreshInterval()
	if interval <= 0 || s.ungroupedQuota == nil || s.refreshAPI == nil || !s.ungroupedMu.TryLock() {
		return
	}
	defer s.ungroupedMu.Unlock()
	pager := s.candidatePager
	if pager == nil {
		pager, _ = s.accountRepo.(OAuthRefreshCandidatePager)
	}
	if pager == nil {
		slog.Error("token_refresh.ungrouped_candidate_pager_missing")
		return
	}
	var registration tokenRefreshRegistration
	for _, candidate := range s.registrations {
		if candidate.platform == PlatformOpenAI && candidate.executor != nil {
			registration = candidate
			break
		}
	}
	if registration.executor == nil {
		return
	}
	executor := &ungroupedOpenAIRefreshExecutor{OAuthRefreshExecutor: registration.executor, interval: interval}
	state := &tokenRefreshProviderState{
		service: s, registration: registration,
		rateGate: s.providerRateGate(PlatformOpenAI), poolGate: s.providerConcurrencyGate(PlatformOpenAI),
	}
	ctx, cancel := context.WithTimeout(parent, s.cycleTimeout())
	defer cancel()
	processed, failed := 0, 0
	for ctx.Err() == nil && !state.isTripped() {
		page, err := pager.ListOAuthRefreshCandidatePage(ctx, OAuthRefreshPageOptions{
			Platforms: []string{PlatformOpenAI}, AfterID: s.ungroupedAfterID, Limit: s.candidatePageSize(),
			ActiveOnly: true, RequireRefreshToken: true, ExcludeRetryCooldown: true, UngroupedOnly: true,
		})
		if err != nil {
			slog.Warn("token_refresh.ungrouped_list_failed", "error", err)
			break
		}
		if page == nil || (page.NextAfterID <= s.ungroupedAfterID && (page.HasMore || len(page.Accounts) > 0)) ||
			!isStrictlyIncreasingAccountPage(page.Accounts, s.ungroupedAfterID) {
			slog.Error("token_refresh.ungrouped_invalid_page")
			break
		}
		completed, errorsByAccount := s.processUngroupedPage(ctx, page.Accounts, executor, state)
		allCompleted := true
		for i, account := range page.Accounts {
			if errorsByAccount[i] != nil && !errors.Is(errorsByAccount[i], errRefreshSkipped) {
				failed++
				slog.Warn("token_refresh.ungrouped_account_failed", "account_id", account.ID,
					"error", logredact.RedactText(errorsByAccount[i].Error()))
			}
			if completed[i] {
				processed++
				if allCompleted {
					s.ungroupedAfterID = account.ID
				}
			} else {
				allCompleted = false
			}
		}
		if !allCompleted {
			break
		}
		s.ungroupedAfterID = page.NextAfterID
		if !page.HasMore {
			s.ungroupedAfterID = 0
			break
		}
	}
	if processed > 0 || failed > 0 {
		slog.Info("token_refresh.ungrouped_cycle_completed", "processed", processed, "failed", failed, "resume_after_id", s.ungroupedAfterID)
	}
}

func (s *TokenRefreshService) processUngroupedPage(ctx context.Context, accounts []Account, executor *ungroupedOpenAIRefreshExecutor, state *tokenRefreshProviderState) ([]bool, []error) {
	completed := make([]bool, len(accounts))
	errorsByAccount := make([]error, len(accounts))
	jobs := make(chan int, len(accounts))
	for i := range accounts {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for i := 0; i < min(len(accounts), s.providerConcurrency()); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if ctx.Err() != nil || state.isTripped() {
					continue
				}
				err := s.refreshUngroupedAccount(ctx, &accounts[index], executor, state)
				errorsByAccount[index] = err
				completed[index] = ctx.Err() == nil
			}
		}()
	}
	wg.Wait()
	return completed, errorsByAccount
}

func (s *TokenRefreshService) refreshUngroupedAccount(ctx context.Context, account *Account, executor *ungroupedOpenAIRefreshExecutor, state *tokenRefreshProviderState) error {
	if !executor.CanRefresh(account) {
		return errRefreshSkipped
	}
	refreshErr := s.refreshWithRetryWithRateGate(ctx, account, executor, executor, 0, state)
	state.recordResult(refreshErr)
	if refreshErr != nil && !errors.Is(refreshErr, errRefreshSkipped) {
		return refreshErr
	}
	// RefreshIfNeeded may have skipped a busy lock or observed another worker's
	// success. Read the durable row before deciding whether quota can be queried.
	fresh, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil {
		return err
	}
	if !executor.CanRefresh(fresh) || (errors.Is(refreshErr, errRefreshSkipped) && executor.NeedsRefresh(fresh, 0)) {
		return errRefreshSkipped
	}
	if last, ok := fresh.Extra[ungroupedOpenAIQuotaAtKey]; ok {
		if stamp, parseErr := time.Parse(time.RFC3339Nano, fmt.Sprint(last)); parseErr == nil && time.Since(stamp) < executor.interval {
			return nil
		}
	}

	release, err := state.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	releaseRate, err := state.acquireRate(ctx)
	if err != nil {
		return err
	}
	defer releaseRate()
	quotaCtx, cancel := context.WithTimeout(ctx, openaiQuotaUpstreamTimeout+defaultTokenRefreshCleanupTimeout)
	defer cancel()
	usage, err := s.ungroupedQuota.QueryUsage(quotaCtx, fresh.ID)
	if err != nil {
		// A quota read failure must not mark valid credentials as invalid or
		// override the account's preset status, schedulability or groups.
		return fmt.Errorf("query ungrouped account quota: %w", err)
	}
	if usage == nil || usage.RateLimit == nil || (usage.RateLimit.PrimaryWindow == nil && usage.RateLimit.SecondaryWindow == nil) {
		return errors.New("ungrouped quota response contains no usage windows")
	}
	now := time.Now()
	updates := buildOpenAIAutoResetUsageUpdates(usage, now)
	updates[ungroupedOpenAIQuotaAtKey] = now.UTC().Format(time.RFC3339Nano)
	updates[openaiQuotaCreditsKey] = openAICreditsSnapshot{Credits: usage.Credits, FetchedAt: usage.FetchedAt}
	if credits := usage.RateLimitResetCredits; credits != nil && (credits.AvailableCount == 0 || len(credits.Credits) > 0) {
		updates[openaiQuotaResetCreditsKey] = credits
	}
	if err := s.accountRepo.UpdateExtra(quotaCtx, fresh.ID, updates); err != nil {
		return fmt.Errorf("save ungrouped account quota: %w", err)
	}
	// Get the current row again so a concurrent group assignment or preset edit
	// cannot be overwritten in the scheduler cache by our earlier snapshot.
	latest, err := s.accountRepo.GetByID(quotaCtx, fresh.ID)
	if err != nil {
		return err
	}
	if latest != nil {
		s.postRefreshStateSync(quotaCtx, latest)
	}
	return nil
}
