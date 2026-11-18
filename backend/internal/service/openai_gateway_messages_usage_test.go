//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestTerminalUsagePrefersResponseAndRetainsProgress(t *testing.T) {
	const observed = `{"type":"response.in_progress","response":{"usage":{"input_tokens":7,"output_tokens":2}}}`
	for _, tc := range []struct {
		name, frame             string
		input, output, cache30m int
	}{
		{"nested_snapshot", `{"response":{"usage":{"input_tokens":12,"input_tokens_details":{"cache_write_tokens":8,"cache_write_tokens_details":{"ttl_30m_tokens":8,"ttl_12h_tokens":0}}}},"usage":{"input_tokens":99,"output_tokens":99}}`, 12, 0, 8},
		{"envelope_only", `{"response":{"usage":null},"usage":{"input_tokens":4,"output_tokens":3}}`, 4, 3, 0},
		{"empty_terminal", `{"response":{"usage":{"input_tokens":0,"output_tokens":0}},"usage":{"input_tokens":99}}`, 7, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var usage OpenAIUsage
			s := &OpenAIGatewayService{}
			s.parseSSEUsageBytesWithType([]byte(observed), "response.in_progress", &usage)
			s.parseSSEUsageBytesWithType([]byte(tc.frame), "response.completed", &usage)
			require.Equal(t, tc.input, usage.InputTokens)
			require.Equal(t, tc.output, usage.OutputTokens)
			require.Equal(t, tc.cache30m, usage.CacheCreation30mInputTokens)
		})
	}
}

func TestCopyOpenAIUsageFromResponsesUsageTrustsCanonicalCacheCreationValue(t *testing.T) {
	usage := &apicompat.ResponsesUsage{
		InputTokens:              20,
		OutputTokens:             2,
		CacheCreationInputTokens: 0,
		InputTokensDetails: &apicompat.ResponsesInputTokensDetails{
			CachedTokens:     3,
			CacheWriteTokens: 19,
		},
	}

	got := copyOpenAIUsageFromResponsesUsage(usage)

	require.Equal(t, 20, got.InputTokens)
	require.Equal(t, 3, got.CacheReadInputTokens)
	require.Zero(t, got.CacheCreationInputTokens)
}
