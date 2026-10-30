//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestNewAccountQuotaPauseDefaultsPersistOnlyAtCreation(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	repo := newAccountRepositoryWithSQL(tx.Client(), tx, nil)
	account := &service.Account{Name: "import-defaults", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Concurrency: 50, Priority: 7}
	require.NoError(t, repo.Create(ctx, account))
	stored, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, float64(99), stored.Credentials["account_scheduling_threshold"])
	require.Equal(t, 7, stored.Priority)
	require.Equal(t, 50, stored.Concurrency)
	delete(stored.Credentials, "account_scheduling_threshold")
	require.NoError(t, repo.Update(ctx, stored))
	stored, err = repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.NotContains(t, stored.Credentials, "account_scheduling_threshold", "later edits must not reapply import defaults")
}
