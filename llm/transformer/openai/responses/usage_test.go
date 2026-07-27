package responses

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
)

func TestUsageUnmarshalPreservesCacheReadFieldPresence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		payload          string
		wantDetails      bool
		wantCachedTokens int64
	}{
		{
			name: "missing details remains unknown",
			payload: `{
				"input_tokens": 100,
				"output_tokens": 20,
				"total_tokens": 120
			}`,
		},
		{
			name: "empty details remains unknown",
			payload: `{
				"input_tokens": 100,
				"input_tokens_details": {},
				"output_tokens": 20,
				"total_tokens": 120
			}`,
		},
		{
			name: "explicit zero is known",
			payload: `{
				"input_tokens": 100,
				"input_tokens_details": {"cached_tokens": 0},
				"output_tokens": 20,
				"total_tokens": 120
			}`,
			wantDetails: true,
		},
		{
			name: "explicit nonzero is known",
			payload: `{
				"input_tokens": 100,
				"input_tokens_details": {"cached_tokens": 40},
				"output_tokens": 20,
				"total_tokens": 120
			}`,
			wantDetails:      true,
			wantCachedTokens: 40,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var upstream Usage
			require.NoError(t, json.Unmarshal([]byte(tt.payload), &upstream))

			normalized := upstream.ToUsage()
			require.NotNil(t, normalized)
			if tt.wantDetails {
				require.NotNil(t, normalized.PromptTokensDetails)
				require.Equal(t, tt.wantCachedTokens, normalized.PromptTokensDetails.CachedTokens)
			} else {
				require.Nil(t, normalized.PromptTokensDetails)
			}
		})
	}
}

func TestResponsesUsageRoundTripPreservesExplicitZeroCacheRead(t *testing.T) {
	t.Parallel()

	normalized := &llm.Usage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		PromptTokensDetails: &llm.PromptTokensDetails{
			CachedTokens: 0,
		},
	}

	responsesUsage := ConvertLLMUsageToResponsesUsage(normalized)
	require.NotNil(t, responsesUsage)

	payload, err := json.Marshal(responsesUsage)
	require.NoError(t, err)

	var decoded Usage
	require.NoError(t, json.Unmarshal(payload, &decoded))
	roundTripped := decoded.ToUsage()
	require.NotNil(t, roundTripped.PromptTokensDetails)
	require.Zero(t, roundTripped.PromptTokensDetails.CachedTokens)
}
