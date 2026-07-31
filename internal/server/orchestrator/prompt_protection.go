package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer"
)

const promptProtectionRejectedMessage = "request blocked by prompt protection policy"

func protectPrompts(inbound *PersistentInboundTransformer) pipeline.Middleware {
	return pipeline.OnLlmRequest("protect-prompts", func(ctx context.Context, llmRequest *llm.Request) (*llm.Request, error) {
		if inbound.state.PromptProtecter == nil {
			return llmRequest, nil
		}

		protected, mutated, err := inbound.state.PromptProtecter.ProtectWithMutation(ctx, llmRequest)
		if err != nil {
			if errors.Is(err, biz.ErrPromptProtectionRejected) {
				return nil, fmt.Errorf("%w: %s", transformer.ErrInvalidRequest, promptProtectionRejectedMessage)
			}

			log.Warn(ctx, "failed to protect prompts", log.Cause(err))

			return llmRequest, nil
		}

		if protected == nil {
			return llmRequest, nil
		}

		if mutated {
			inbound.state.PromptPayloadMutated = true
		}

		return protected, nil
	})
}
