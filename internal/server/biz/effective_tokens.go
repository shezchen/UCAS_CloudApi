package biz

import (
	"math"

	"github.com/looplj/axonhub/llm"
)

// MaxEffectiveTokensPerRequest is a defensive accounting ceiling, not a model
// context claim. Campus models default to at most a 1M context window; this
// deliberately generous bound prevents a custom compatible endpoint from
// overflowing aggregates or minting an unbounded permanent wallet balance with
// a fabricated usage object.
const MaxEffectiveTokensPerRequest int64 = 10_000_000

func saturatingAddNonNegative(left, right int64) int64 {
	left = max(left, int64(0))
	right = max(right, int64(0))
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func boundedEffectiveTokens(promptTokens, completionTokens, totalTokens, cachedTokens int64) int64 {
	promptTokens = max(promptTokens, int64(0))
	completionTokens = max(completionTokens, int64(0))
	totalTokens = max(totalTokens, int64(0))
	cachedTokens = max(cachedTokens, int64(0))

	nonCachedPromptTokens := int64(0)
	if promptTokens > cachedTokens {
		nonCachedPromptTokens = promptTokens - cachedTokens
	}
	totalWithoutCache := int64(0)
	if totalTokens > cachedTokens {
		totalWithoutCache = totalTokens - cachedTokens
	}

	// Some compatible upstreams populate total_tokens but omit one component.
	// Taking the larger consistent candidate is conservative and avoids making
	// partially reported usage disappear from quota or donor accounting.
	effective := max(
		saturatingAddNonNegative(nonCachedPromptTokens, completionTokens),
		totalWithoutCache,
	)
	return min(effective, MaxEffectiveTokensPerRequest)
}

// EffectiveTokens returns the quota-bearing token count for a normalized LLM
// usage record and whether cache-read reporting was explicitly available.
//
// PromptTokens includes cache reads in AxonHub's normalized usage model.
// Cache writes remain real input and are therefore retained; only cache reads
// are excluded. CompletionTokens already contains reasoning tokens, so no
// completion detail field is added again.
func EffectiveTokens(usage *llm.Usage) (tokens int64, cacheReadTokensKnown bool) {
	if usage == nil {
		return 0, false
	}

	cachedTokens := int64(0)
	if usage.PromptTokensDetails != nil {
		cacheReadTokensKnown = true
		cachedTokens = max(usage.PromptTokensDetails.CachedTokens, int64(0))
	}

	return boundedEffectiveTokens(
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.TotalTokens,
		cachedTokens,
	), cacheReadTokensKnown
}

// StoredEffectiveTokens reads the additive UsageLog accounting fields while
// remaining safe across an in-place upgrade. Rows written by the new version
// carry either a non-zero effective value or an explicit cache-read-known bit.
// Older rows have both fields at their zero defaults and therefore fall back to
// the same conservative prompt/completion accounting used before the cutover.
func StoredEffectiveTokens(
	storedEffective int64,
	cacheReadKnown bool,
	promptTokens int64,
	completionTokens int64,
	totalTokens int64,
	cachedReadTokens int64,
) int64 {
	if storedEffective > 0 || cacheReadKnown {
		return max(storedEffective, int64(0))
	}

	return boundedEffectiveTokens(
		promptTokens,
		completionTokens,
		totalTokens,
		cachedReadTokens,
	)
}
