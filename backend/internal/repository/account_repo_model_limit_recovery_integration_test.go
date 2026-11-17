//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Exercise the database compare-and-delete with ordered interleavings rather
// than sleeps: an old usage observer must not erase a newer quota restriction.
func TestAccountRepositoryClearModelRateLimitIfUnchanged(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	const scope = "claude-fable-5"
	const windowReason = "anthropic_7d_oi_window_exhausted"

	for _, tc := range []struct {
		name   string
		reason string
	}{
		{name: "new credits restriction", reason: "anthropic_fable_credits_required"},
		{name: "new window with the same reason", reason: windowReason},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := mustCreateAccount(t, tx.Client(), &service.Account{
				Name: tc.name, Platform: service.PlatformAnthropic, Type: service.AccountTypeOAuth,
				Extra: map[string]any{"unrelated": "preserve"},
			})
			require.NoError(t, repo.SetModelRateLimit(ctx, account.ID, scope, time.Now().Add(time.Hour), windowReason))
			snapshot, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			observed := snapshot.Extra["model_rate_limits"].(map[string]any)[scope].(map[string]any)

			// A second request updates the same scope after the first observed it.
			require.NoError(t, repo.SetModelRateLimit(ctx, account.ID, scope, time.Now().Add(2*time.Hour), tc.reason))
			require.NoError(t, repo.SetModelRateLimit(ctx, account.ID, "other-model", time.Now().Add(3*time.Hour), "other restriction"))
			before, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)

			cleared, err := repo.ClearModelRateLimitIfUnchanged(ctx, account.ID, scope, observed)
			require.NoError(t, err)
			require.False(t, cleared, "stale recovery must not remove the newer same-scope restriction")
			after, err := repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			require.Equal(t, before.Extra, after.Extra)

			// The current observer can clear its own generation and only its scope.
			current := after.Extra["model_rate_limits"].(map[string]any)[scope].(map[string]any)
			cleared, err = repo.ClearModelRateLimitIfUnchanged(ctx, account.ID, scope, current)
			require.NoError(t, err)
			require.True(t, cleared)
			after, err = repo.GetByID(ctx, account.ID)
			require.NoError(t, err)
			limits := after.Extra["model_rate_limits"].(map[string]any)
			require.NotContains(t, limits, scope)
			require.Equal(t, before.Extra["model_rate_limits"].(map[string]any)["other-model"], limits["other-model"])
			require.Equal(t, "preserve", after.Extra["unrelated"])

			cleared, err = repo.ClearModelRateLimitIfUnchanged(ctx, account.ID, scope, current)
			require.NoError(t, err)
			require.False(t, cleared, "deleting an already cleared scope is a compare miss")
			cleared, err = repo.ClearModelRateLimitIfUnchanged(ctx, account.ID, "other-model", nil)
			require.NoError(t, err)
			require.False(t, cleared, "missing observations must never enable unconditional clearing")
		})
	}
}
