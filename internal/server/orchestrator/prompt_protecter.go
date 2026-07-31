package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/llm"
)

type PromptProtecter interface {
	ProtectWithMutation(ctx context.Context, req *llm.Request) (*llm.Request, bool, error)
}
