//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupSpeedModePersistenceAndConstraint(t *testing.T) {
	ctx := context.Background()
	repo := newGroupRepositoryWithSQL(integrationEntClient, integrationDB)
	group := &service.Group{Name: fmt.Sprintf("speed-mode-%d", time.Now().UnixNano()), Platform: service.PlatformOpenAI,
		Status: service.StatusActive, RateMultiplier: 1, ForceOpenAIUltrafast: true, FreeOpenAIFast: true}
	require.NoError(t, repo.Create(ctx, group))
	t.Cleanup(func() { _, _ = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id=$1", group.ID) })
	got, err := repo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.True(t, got.ForceOpenAIUltrafast)
	require.False(t, got.ForceOpenAIFast || got.FilterOpenAIServiceTier)
	require.True(t, got.FreeOpenAIFast)
	got.ForceOpenAIUltrafast, got.FilterOpenAIServiceTier = false, true
	require.NoError(t, repo.Update(ctx, got))
	got, err = repo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.True(t, got.FilterOpenAIServiceTier)
	require.False(t, got.ForceOpenAIFast || got.ForceOpenAIUltrafast)
	_, err = integrationDB.ExecContext(ctx, "UPDATE groups SET force_openai_ultrafast=true WHERE id=$1", group.ID)
	require.ErrorContains(t, err, "groups_openai_fast_mode_exclusive")
	got, err = repo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.True(t, got.FilterOpenAIServiceTier)
	require.False(t, got.ForceOpenAIUltrafast)
}
