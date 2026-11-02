package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSnapshotGroupForceOpenAIFastRoundtrip(t *testing.T) {
	for _, mode := range []string{"priority", "ultrafast", "filter"} {
		t.Run(mode, func(t *testing.T) {
			groupID := int64(50)
			apiKey := &APIKey{
				ID: 82, UserID: 40, GroupID: &groupID, Key: "sk-fast-roundtrip", Status: StatusActive,
				User: &User{ID: 40, Status: StatusActive},
				Group: &Group{
					ID: groupID, Name: "fast-roundtrip", Platform: PlatformOpenAI, Status: StatusActive,
					Hydrated: true, ForceOpenAIFast: mode == "priority", ForceOpenAIUltrafast: mode == "ultrafast", FilterOpenAIServiceTier: mode == "filter", FreeOpenAIFast: true,
					ReverseAccountPriority: true,
				},
			}
			svc := &APIKeyService{}

			payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), apiKey)})
			require.NoError(t, err)
			var cached APIKeyAuthCacheEntry
			require.NoError(t, json.Unmarshal(payload, &cached))

			materialized, used, err := svc.applyAuthCacheEntry(apiKey.Key, &cached)
			require.NoError(t, err)
			require.True(t, used)
			require.NotNil(t, materialized.Group)
			require.True(t, materialized.Group.Hydrated)
			require.Equal(t, mode == "priority", materialized.Group.ForceOpenAIFast)
			require.Equal(t, mode == "ultrafast", materialized.Group.ForceOpenAIUltrafast)
			require.Equal(t, mode == "filter", materialized.Group.FilterOpenAIServiceTier)
			require.True(t, materialized.Group.FreeOpenAIFast)
			require.True(t, materialized.Group.ReverseAccountPriority)
			require.Equal(t, apiKeyAuthSnapshotVersion, cached.Snapshot.Version)
		})
	}
}
