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

func TestSmartWSGroupPersistenceAndAuthProjection(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	repo := newGroupRepositoryWithSQL(integrationEntClient, integrationDB)
	group := &service.Group{Name: fmt.Sprintf("smart-ws-%d", suffix), Platform: service.PlatformOpenAI,
		Status: service.StatusActive, RateMultiplier: 1}
	require.NoError(t, repo.Create(ctx, group))
	user := mustCreateUser(t, integrationEntClient, &service.User{Email: fmt.Sprintf("smart-ws-%d@example.com", suffix), Concurrency: 1})
	keys := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: fmt.Sprintf("sk-smart-ws-%d", suffix), Name: "smart-ws-test", Status: service.StatusActive}
	require.NoError(t, keys.Create(ctx, key))
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", key.Key)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id=$1", key.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id=$1", user.ID)
		_, _ = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id=$1", group.ID)
	})
	got, err := repo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, "inherit", got.SmartWSMode)
	for _, mode := range []string{"enabled", "disabled", "inherit"} {
		got.SmartWSMode = mode
		require.NoError(t, repo.Update(ctx, got))
		got, err = repo.GetByID(ctx, group.ID)
		require.NoError(t, err)
		require.Equal(t, mode, got.SmartWSMode)
		authed, err := keys.GetByKeyForAuth(ctx, key.Key)
		require.NoError(t, err)
		require.Equal(t, mode, authed.Group.SmartWSMode)
	}
	_, err = integrationDB.ExecContext(ctx, "UPDATE groups SET smart_ws_mode='invalid' WHERE id=$1", group.ID)
	require.ErrorContains(t, err, "groups_smart_ws_mode_check")
}
