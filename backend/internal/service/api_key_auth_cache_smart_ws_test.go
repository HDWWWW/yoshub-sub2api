package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSnapshotSmartWSRoundtrip(t *testing.T) {
	for _, mode := range []string{"inherit", "enabled", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			groupID := int64(7)
			key := &APIKey{ID: 1, UserID: 2, GroupID: &groupID, Key: "sk-test", Status: StatusActive,
				User:  &User{ID: 2, Status: StatusActive},
				Group: &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, SmartWSMode: mode}}
			svc := &APIKeyService{}
			payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), key)})
			require.NoError(t, err)
			var cached APIKeyAuthCacheEntry
			require.NoError(t, json.Unmarshal(payload, &cached))
			got, used, err := svc.applyAuthCacheEntry(key.Key, &cached)
			require.NoError(t, err)
			require.True(t, used)
			require.True(t, got.Group.Hydrated)
			require.Equal(t, mode, got.Group.SmartWSMode)
			cached.Snapshot.Version--
			_, used, _ = svc.applyAuthCacheEntry(key.Key, &cached)
			require.False(t, used, "an older snapshot must reload the new group policy")
		})
	}
}
