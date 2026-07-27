package responses

import (
	"encoding/json"

	"github.com/looplj/axonhub/llm"
)

type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	InputTokenDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens       int64 `json:"output_tokens"`
	OutputTokenDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int64 `json:"total_tokens"`

	// CacheReadTokensKnown records JSON field presence without changing the
	// wire representation. It is exported so generic comparison/inspection
	// tools can safely handle Usage values.
	CacheReadTokensKnown bool `json:"-"`
}

// Equal compares the wire-visible usage value. CacheReadTokensKnown is
// transport-presence metadata used only while normalizing to llm.Usage, so it
// must not make otherwise identical Responses API payloads unequal.
func (u Usage) Equal(other Usage) bool {
	return u.InputTokens == other.InputTokens &&
		u.InputTokenDetails.CachedTokens == other.InputTokenDetails.CachedTokens &&
		u.OutputTokens == other.OutputTokens &&
		u.OutputTokenDetails.ReasoningTokens == other.OutputTokenDetails.ReasoningTokens &&
		u.TotalTokens == other.TotalTokens
}

// UnmarshalJSON preserves whether the upstream explicitly supplied the
// cache-read field. A missing details object and an explicit cached_tokens=0
// have the same Go zero value but different accounting confidence.
func (u *Usage) UnmarshalJSON(data []byte) error {
	type usageAlias Usage
	var decoded usageAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*u = Usage(decoded)

	var raw struct {
		InputTokenDetails map[string]json.RawMessage `json:"input_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, u.CacheReadTokensKnown = raw.InputTokenDetails["cached_tokens"]

	return nil
}

func (u *Usage) ToUsage() *llm.Usage {
	usage := &llm.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CompletionTokensDetails: &llm.CompletionTokensDetails{
			ReasoningTokens: u.OutputTokenDetails.ReasoningTokens,
		},
	}
	if u.CacheReadTokensKnown || u.InputTokenDetails.CachedTokens != 0 {
		usage.PromptTokensDetails = &llm.PromptTokensDetails{
			CachedTokens: u.InputTokenDetails.CachedTokens,
		}
	}

	return usage
}

// ConvertLLMUsageToResponsesUsage converts llm.Usage to Responses API Usage.
func ConvertLLMUsageToResponsesUsage(usage *llm.Usage) *Usage {
	if usage == nil {
		return nil
	}

	result := &Usage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	}

	if usage.PromptTokensDetails != nil {
		result.InputTokenDetails.CachedTokens = usage.PromptTokensDetails.CachedTokens
		result.CacheReadTokensKnown = true
	}

	if usage.CompletionTokensDetails != nil {
		result.OutputTokenDetails.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}

	return result
}
