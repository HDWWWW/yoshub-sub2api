package repository

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupReversePriorityPersistenceAndAuthProjection(t *testing.T) {
	apiKeys, client := newAPIKeyRepoSQLite(t)
	ctx := context.Background()
	user := mustCreateAPIKeyRepoUser(t, ctx, client, "reverse-priority@test.com")
	groups := newGroupRepositoryWithSQL(client, apiKeys.sql)
	group := &service.Group{Name: "reverse-priority", Platform: service.PlatformOpenAI, Status: service.StatusActive, RateMultiplier: 1}
	require.NoError(t, groups.Create(ctx, group))
	got, err := groups.GetByIDLite(ctx, group.ID)
	require.NoError(t, err)
	require.False(t, got.ReverseAccountPriority)
	got.ReverseAccountPriority = true
	require.NoError(t, groups.Update(ctx, got))
	key := &service.APIKey{UserID: user.ID, Key: "sk-reverse-priority-unit", Name: "Reverse Priority", GroupID: &group.ID, Status: service.StatusActive}
	require.NoError(t, apiKeys.Create(ctx, key))
	loaded, err := apiKeys.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.NotNil(t, loaded.Group)
	require.True(t, loaded.Group.ReverseAccountPriority)
	got.ReverseAccountPriority = false
	require.NoError(t, groups.Update(ctx, got))
	loaded, err = apiKeys.GetByKeyForAuth(ctx, key.Key)
	require.NoError(t, err)
	require.False(t, loaded.Group.ReverseAccountPriority)
}
