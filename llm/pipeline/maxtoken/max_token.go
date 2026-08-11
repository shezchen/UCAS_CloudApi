package maxtoken

import (
	"context"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
)

// EnsureMaxTokens creates a decorator that enforces a max tokens ceiling:
// requests without max_tokens get defaultValue, and requests asking for more
// than defaultValue are clamped down to it.
func EnsureMaxTokens(defaultValue int64) pipeline.Middleware {
	return pipeline.OnLlmRequest("max-tokens", func(ctx context.Context, request *llm.Request) (*llm.Request, error) {
		if request.MaxTokens == nil {
			request.MaxTokens = &defaultValue
		}

		if *request.MaxTokens > defaultValue {
			request.MaxTokens = &defaultValue
		}

		return request, nil
	})
}
