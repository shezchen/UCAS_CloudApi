package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/looplj/axonhub/llm/httpclient"
)

// Process executes the non-streaming LLM pipeline
// Steps: outbound transform -> HTTP request -> outbound response transform -> inbound response transform.
func (p *pipeline) notStream(
	ctx context.Context,
	executor Executor,
	request *httpclient.Request,
) (*httpclient.Response, error) {
	httpResp, err := executor.Do(ctx, request)
	if err != nil {
		// Apply error response middlewares
		p.applyRawErrorResponseMiddlewares(ctx, err)

		if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
			return nil, WrapUpstreamError(p.Outbound.TransformError(ctx, httpErr))
		}

		return nil, WrapUpstreamError(fmt.Errorf("failed to do request: %w", err))
	}

	// Apply raw response middlewares
	httpResp, err = p.applyRawResponseMiddlewares(ctx, httpResp)
	if err != nil {
		p.applyRawErrorResponseMiddlewares(ctx, err)

		return nil, fmt.Errorf("failed to apply raw response middlewares: %w", err)
	}

	llmResp, err := p.Outbound.TransformResponse(ctx, httpResp)
	if err != nil {
		p.applyRawErrorResponseMiddlewares(ctx, err)

		return nil, WrapUpstreamError(fmt.Errorf("failed to transform response: %w", err))
	}

	// A successfully decoded HTTP body is not necessarily a successful model
	// response. Responses API may return response.incomplete/failed/canceled
	// together with useful-looking text or tool calls. Reject that attempt before
	// persistence, affinity, accounting, and other success middlewares run.
	if outcome := ResponseTerminalOutcome(llmResp); outcome.Terminal && !outcome.Successful {
		p.applyRawErrorResponseMiddlewares(ctx, outcome.Err)

		return nil, WrapUpstreamError(outcome.Err)
	}

	// Reject empty responses before persistence/accounting middlewares run. A
	// usage-only response must remain retryable and must not consume the logical
	// request's one usage-log slot before a later successful candidate.
	if p.emptyResponseDetection && !hasResponseContent(llmResp) {
		p.applyRawErrorResponseMiddlewares(ctx, ErrEmptyResponse)

		return nil, ErrEmptyResponse
	}

	// Apply LLM response middlewares only after the response is accepted.
	llmResp, err = p.applyLlmResponseMiddlewares(ctx, llmResp)
	if err != nil {
		p.applyRawErrorResponseMiddlewares(ctx, err)

		return nil, fmt.Errorf("failed to apply llm response middlewares: %w", err)
	}

	slog.DebugContext(ctx, "LLM response", slog.Any("response", llmResp))

	finalResp, err := p.Inbound.TransformResponse(ctx, llmResp)
	if err != nil {
		p.applyRawErrorResponseMiddlewares(ctx, err)

		return nil, fmt.Errorf("failed to transform final response: %w", err)
	}

	// Apply inbound raw response middlewares after final response transformation
	finalResp, err = p.applyInboundRawResponseMiddlewares(ctx, finalResp)
	if err != nil {
		p.applyRawErrorResponseMiddlewares(ctx, err)

		return nil, fmt.Errorf("failed to apply inbound raw response middlewares: %w", err)
	}

	return finalResp, nil
}

func (p *pipeline) autoAggregateStream(
	ctx context.Context,
	executor Executor,
	request *httpclient.Request,
) (*httpclient.Response, error) {
	inboundStream, err := p.stream(ctx, executor, request, 0, false)
	if err != nil {
		return nil, err
	}
	defer inboundStream.Close()

	chunks := make([]*httpclient.StreamEvent, 0, 8)
	for inboundStream.Next() {
		event := inboundStream.Current()
		if event != nil {
			chunks = append(chunks, event)
		}
	}

	if err := inboundStream.Err(); err != nil {
		// Close persistence first, then let the concrete pipeline error win the
		// execution diagnostic. Otherwise an unaccepted-but-semantic upstream
		// stream can overwrite this error with a generic empty-response fallback.
		_ = inboundStream.Close()
		p.applyRawErrorResponseMiddlewares(ctx, err)
		return nil, err
	}

	if len(chunks) == 0 {
		_ = inboundStream.Close()
		p.applyRawErrorResponseMiddlewares(ctx, ErrEmptyStreamChunks)
		return nil, ErrEmptyStreamChunks
	}

	body, _, err := p.Inbound.AggregateStreamChunks(ctx, chunks)
	if err != nil {
		_ = inboundStream.Close()
		p.applyRawErrorResponseMiddlewares(ctx, err)
		return nil, err
	}

	if len(body) == 0 {
		_ = inboundStream.Close()
		p.applyRawErrorResponseMiddlewares(ctx, ErrEmptyAggregatedBody)
		return nil, ErrEmptyAggregatedBody
	}

	resp := &httpclient.Response{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":  []string{"application/json"},
			"Cache-Control": []string{"no-cache"},
		},
		Body: body,
	}

	resp, err = p.applyInboundRawResponseMiddlewares(ctx, resp)
	if err != nil {
		_ = inboundStream.Close()
		p.applyRawErrorResponseMiddlewares(ctx, err)
		return nil, fmt.Errorf("failed to apply inbound raw response middlewares: %w", err)
	}

	p.acceptStreamAttempt()

	return resp, nil
}
