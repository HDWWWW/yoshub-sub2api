//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type pagedBulkConcurrencyRepo struct {
	accountRepoStubForBulkUpdate
	pages []int
}

func (r *pagedBulkConcurrencyRepo) ListWithFilters(_ context.Context, params pagination.PaginationParams, platform, accountType, status, search string, groupID int64, privacyMode string) ([]Account, *pagination.PaginationResult, error) {
	r.pages = append(r.pages, params.Page)
	if groupID != 12 || platform != PlatformOpenAI || search != "target" {
		return nil, &pagination.PaginationResult{}, nil
	}
	accounts := make([]Account, 0)
	for i := (params.Page - 1) * params.PageSize; i < 501 && i < params.Page*params.PageSize; i++ {
		accounts = append(accounts, Account{ID: int64(i + 1)})
	}
	return accounts, &pagination.PaginationResult{Total: 501}, nil
}

func TestBulkConcurrencyUpdatesAllFilteredPages(t *testing.T) {
	repo := &pagedBulkConcurrencyRepo{}
	svc := &adminServiceImpl{accountRepo: repo}
	concurrency := 37
	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		Filters: &BulkUpdateAccountFilters{Group: "12", Platform: PlatformOpenAI, Search: "target"}, Concurrency: &concurrency,
	})
	require.NoError(t, err)
	require.Equal(t, []int{1, 2}, repo.pages)
	require.Len(t, repo.bulkUpdateIDs, 501)
	require.Equal(t, int64(501), repo.bulkUpdateIDs[500])
	require.Equal(t, 501, result.Success)
	require.Equal(t, &concurrency, repo.lastBulkUpdate.Concurrency)
	require.Nil(t, repo.lastBulkUpdate.Priority)
	require.Empty(t, repo.lastBulkUpdate.Credentials)
}

func TestBulkConcurrencyRejectsInvalidValuesBeforeWrites(t *testing.T) {
	for _, value := range []int{0, -1, 2147483648} {
		repo := &accountRepoStubForBulkUpdate{}
		svc := &adminServiceImpl{accountRepo: repo}
		_, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{AccountIDs: []int64{1}, Concurrency: &value})
		require.Error(t, err)
		require.Zero(t, repo.bulkUpdateCalls)
	}
}
