package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failingQuotaSnapshotRepo struct {
	*sparkShadowUsageTestRepo
	attempts int
}

func (r *failingQuotaSnapshotRepo) UpdateExtra(context.Context, int64, map[string]any) error {
	r.attempts++
	return errors.New("snapshot write unavailable")
}

// A valid upstream quota response is not a fresh observation until the custom
// durable snapshot succeeds. Keep the old data and surface the official error.
func TestGetOpenAIUsage_PersistFailureKeepsOldQuotaAndReportsRefreshFailure(t *testing.T) {
	observedAt := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	parentID := int64(100)
	parent := &Account{ID: parentID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{"chatgpt_account_id": "audit-quota-parent"}}
	shadow := &Account{
		ID: 200, ParentAccountID: &parentID,
		Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		QuotaDimension: QuotaDimensionSpark,
		Extra: map[string]any{
			"codex_7d_used_percent":  97.0,
			"codex_usage_updated_at": observedAt.Format(time.RFC3339),
		},
	}
	repo := &failingQuotaSnapshotRepo{sparkShadowUsageTestRepo: &sparkShadowUsageTestRepo{
		accounts: map[int64]*Account{100: parent, 200: shadow},
	}}
	tokens := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(parent): "fake-access-token"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"additional_rate_limits":[{"metered_feature":"codex_bengalfox","rate_limit":{"primary_window":{"used_percent":10,"reset_after_seconds":3600,"limit_window_seconds":18000},"secondary_window":{"used_percent":20,"reset_after_seconds":86400,"limit_window_seconds":604800}}}]}`))
	}))
	defer srv.Close()
	svc := &AccountUsageService{
		accountRepo: repo,
		openAIQuotaService: NewOpenAIQuotaService(repo, nil,
			NewOpenAITokenProvider(repo, tokens, nil), newQuotaRedirectingFactory(srv), nil),
	}
	usage, err := svc.getOpenAIUsage(context.Background(), shadow, true)
	require.NoError(t, err)
	require.Equal(t, 1, repo.attempts)
	require.Equal(t, errorCodeQuotaRefreshFailed, usage.ErrorCode)
	require.NotEmpty(t, usage.Error)
	require.Equal(t, "passive", usage.Source)
	require.NotNil(t, usage.UpdatedAt)
	require.True(t, usage.UpdatedAt.Equal(observedAt))
	require.NotNil(t, usage.SevenDay)
	require.Equal(t, 97.0, usage.SevenDay.Utilization)
	require.Equal(t, observedAt.Format(time.RFC3339), shadow.Extra["codex_usage_updated_at"])
	require.Equal(t, 97.0, shadow.Extra["codex_7d_used_percent"])
}
