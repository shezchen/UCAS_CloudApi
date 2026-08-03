package api

import (
	"context"
	"net/http"

	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
)

func transformOrchestratorError(ctx context.Context, err error, orch *orchestrator.ChatCompletionOrchestrator) *httpclient.Error {
	err = wrapQuotaExhaustedAsResponseError(err)
	if orch != nil {
		err = applyUpstreamErrorPolicy(err)
		return orch.Inbound.TransformError(ctx, err)
	}

	return &httpclient.Error{
		StatusCode: http.StatusInternalServerError,
		Status:     http.StatusText(http.StatusInternalServerError),
		Body:       []byte(`{"error":{"message":"internal server error","type":"internal_server_error"}}`),
	}
}

func applyUpstreamErrorPolicy(err error) error {
	// UCAS is a cooperative service: users must receive the provider's real
	// error so they can report actionable diagnostics to the maintainer.  The
	// persisted legacy hidden/custom setting is intentionally ignored.
	return err
}
