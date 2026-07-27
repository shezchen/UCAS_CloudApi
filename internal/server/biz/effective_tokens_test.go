package biz

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
)

func TestEffectiveTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		usage     *llm.Usage
		want      int64
		wantKnown bool
	}{
		{
			name: "nil usage",
		},
		{
			name: "unknown cache reporting counts all normalized input and output",
			usage: &llm.Usage{
				PromptTokens:     120,
				CompletionTokens: 30,
				TotalTokens:      150,
			},
			want: 150,
		},
		{
			name: "total-only compatible usage remains quota-bearing",
			usage: &llm.Usage{
				TotalTokens: 75,
			},
			want: 75,
		},
		{
			name: "prompt-only partial usage remains quota-bearing",
			usage: &llm.Usage{
				PromptTokens: 63,
			},
			want: 63,
		},
		{
			name: "completion-only partial usage remains quota-bearing",
			usage: &llm.Usage{
				CompletionTokens: 17,
			},
			want: 17,
		},
		{
			name: "larger component sum wins over inconsistent total",
			usage: &llm.Usage{
				PromptTokens:     50,
				CompletionTokens: 20,
				TotalTokens:      10,
			},
			want: 70,
		},
		{
			name: "larger total wins when components are partial",
			usage: &llm.Usage{
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalTokens:      90,
			},
			want: 90,
		},
		{
			name: "total-only usage excludes explicitly reported cache reads",
			usage: &llm.Usage{
				TotalTokens: 75,
				PromptTokensDetails: &llm.PromptTokensDetails{
					CachedTokens: 25,
				},
			},
			want:      50,
			wantKnown: true,
		},
		{
			name: "cache reads are excluded",
			usage: &llm.Usage{
				PromptTokens:     120,
				CompletionTokens: 30,
				PromptTokensDetails: &llm.PromptTokensDetails{
					CachedTokens: 40,
				},
			},
			want:      110,
			wantKnown: true,
		},
		{
			name: "cache writes remain inside prompt usage",
			usage: &llm.Usage{
				PromptTokens:     120,
				CompletionTokens: 30,
				PromptTokensDetails: &llm.PromptTokensDetails{
					CachedTokens:      40,
					WriteCachedTokens: 50,
				},
			},
			want:      110,
			wantKnown: true,
		},
		{
			name: "zero cache is still known when details are present",
			usage: &llm.Usage{
				PromptTokens:     10,
				CompletionTokens: 5,
				PromptTokensDetails: &llm.PromptTokensDetails{
					CachedTokens: 0,
				},
			},
			want:      15,
			wantKnown: true,
		},
		{
			name: "invalid cache larger than prompt clamps non-cache input to zero",
			usage: &llm.Usage{
				PromptTokens:     10,
				CompletionTokens: 5,
				PromptTokensDetails: &llm.PromptTokensDetails{
					CachedTokens: 20,
				},
			},
			want:      5,
			wantKnown: true,
		},
		{
			name: "negative provider counters cannot reduce effective usage",
			usage: &llm.Usage{
				PromptTokens:     -10,
				CompletionTokens: -5,
				PromptTokensDetails: &llm.PromptTokensDetails{
					CachedTokens: -20,
				},
			},
			wantKnown: true,
		},
		{
			name: "reasoning tokens are not double counted",
			usage: &llm.Usage{
				PromptTokens:     10,
				CompletionTokens: 25,
				CompletionTokensDetails: &llm.CompletionTokensDetails{
					ReasoningTokens: 20,
				},
			},
			want: 35,
		},
		{
			name: "provider counters cannot overflow accounting",
			usage: &llm.Usage{
				PromptTokens:     math.MaxInt64,
				CompletionTokens: math.MaxInt64,
				TotalTokens:      math.MaxInt64,
			},
			want: MaxEffectiveTokensPerRequest,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, known := EffectiveTokens(tt.usage)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.wantKnown, known)
		})
	}
}

func TestStoredEffectiveTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		storedEffective  int64
		cacheReadKnown   bool
		promptTokens     int64
		completionTokens int64
		totalTokens      int64
		cachedReadTokens int64
		want             int64
	}{
		{
			name:             "new fully cached row keeps explicit zero",
			cacheReadKnown:   true,
			promptTokens:     100,
			totalTokens:      100,
			cachedReadTokens: 100,
		},
		{
			name:            "new nonzero stored value is authoritative",
			storedEffective: 12,
			promptTokens:    100,
			totalTokens:     100,
			want:            12,
		},
		{
			name:             "legacy row falls back to conservative components",
			promptTokens:     100,
			completionTokens: 20,
			totalTokens:      120,
			cachedReadTokens: 40,
			want:             80,
		},
		{
			name:             "legacy partial row cannot disappear",
			completionTokens: 15,
			want:             15,
		},
		{
			name:             "legacy overflow is capped",
			promptTokens:     math.MaxInt64,
			completionTokens: math.MaxInt64,
			totalTokens:      math.MaxInt64,
			want:             MaxEffectiveTokensPerRequest,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := StoredEffectiveTokens(
				tt.storedEffective,
				tt.cacheReadKnown,
				tt.promptTokens,
				tt.completionTokens,
				tt.totalTokens,
				tt.cachedReadTokens,
			)
			require.Equal(t, tt.want, got)
		})
	}
}
