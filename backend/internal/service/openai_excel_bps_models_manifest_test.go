package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestExcelBPSManifestHonorsModelRoutesAndETag(t *testing.T) {
	bps := *excelAccount()
	bps.ID, bps.Status, bps.Schedulable = 31, StatusActive, true
	bps.Credentials["model_mapping"] = map[string]any{"bps-alias": "gpt-6-astra", "native-alias": "gpt-6-astra"}
	bps.Extra["openai_excel_bps_models"] = []string{"gpt-6-astra"}
	native := Account{ID: 32, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	group := &Group{ID: 17, Platform: PlatformOpenAI, ModelRoutingEnabled: true,
		ModelRouting: map[string][]int64{"bps-alias": {31}, "native-alias": {32}},
	}
	group.CodexModelsManifestConfig.Enabled = true
	svc := &OpenAIGatewayService{accountRepo: codexModelsVisibilityAccountRepo{
		byGroup: map[int64][]Account{group.ID: {bps, native}},
	}}
	body := []byte(`{"models":[{"slug":"bps-alias","multi_agent_version":"v2","multi_agent_reasoning_effort":"high"},{"slug":"native-alias","multi_agent_version":"v2","multi_agent_reasoning_effort":"high"}]}`)
	upstreamETag := codexModelsManifestBodyETag(body)
	manifest := &OpenAIModelsResponse{Body: body, ETag: upstreamETag}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, manifest, upstreamETag))
	require.False(t, manifest.NotModified)
	require.NotEqual(t, upstreamETag, manifest.ETag)
	require.Equal(t, gjson.Null, gjson.GetBytes(manifest.Body, "models.0.multi_agent_version").Type)
	require.Equal(t, "v2", gjson.GetBytes(manifest.Body, "models.1.multi_agent_version").String())
	require.Equal(t, "v2", gjson.GetBytes(body, "models.0.multi_agent_version").String(), "do not mutate shared upstream cache")
	second := &OpenAIModelsResponse{Body: body, ETag: upstreamETag}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, second, manifest.ETag))
	require.True(t, second.NotModified)
	require.Empty(t, second.Body)
}
