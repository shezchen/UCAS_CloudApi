package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/looplj/axonhub/internal/dumper"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// InboundPersistentStream wraps a stream and tracks all responses for final saving to database.
// It implements the streams.Stream interface and handles persistence in the Close method.
//
//nolint:containedctx // Checked.
type InboundPersistentStream struct {
	ctx            context.Context
	stream         streams.Stream[*httpclient.StreamEvent]
	request        *ent.Request
	requestExec    *ent.RequestExecution
	requestService *biz.RequestService
	transformer    transformer.Inbound
	perf           *biz.PerformanceRecord
	responseChunks []*httpclient.StreamEvent
	closed         bool
	state          *PersistenceState
}

var _ streams.Stream[*httpclient.StreamEvent] = (*InboundPersistentStream)(nil)

func NewInboundPersistentStream(
	ctx context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	request *ent.Request,
	requestExec *ent.RequestExecution,
	requestService *biz.RequestService,
	transformer transformer.Inbound,
	perf *biz.PerformanceRecord,
	state *PersistenceState,
) *InboundPersistentStream {
	s := &InboundPersistentStream{
		ctx:            ctx,
		stream:         stream,
		request:        request,
		requestExec:    requestExec,
		requestService: requestService,
		transformer:    transformer,
		perf:           perf,
		responseChunks: make([]*httpclient.StreamEvent, 0),
		closed:         false,
		state:          state,
	}

	return s
}

func (ts *InboundPersistentStream) Next() bool {
	return ts.stream.Next()
}

func (ts *InboundPersistentStream) Current() *httpclient.StreamEvent {
	event := ts.stream.Current()
	if event != nil {
		// For raw binary audio chunks (TTS stream_format=audio), persist only a size
		// summary to avoid buffering the full audio payload in memory.
		ts.responseChunks = append(ts.responseChunks, httpclient.SummarizeBinaryChunk(event))
		if isSuccessfulTerminalStreamEvent(event) {
			ts.state.StreamCompleted = true
		}
	}

	return event
}

// isSuccessfulTerminalStreamEvent checks if an event proves successful
// completion. A Responses response.incomplete event is terminal on the wire but
// deliberately excluded: persistence must record it as non-completed.
func isSuccessfulTerminalStreamEvent(event *httpclient.StreamEvent) bool {
	if event == nil {
		return false
	}
	if event.Type == "response.completed" {
		if len(event.Data) == 0 {
			return true
		}

		var payload struct {
			Response struct {
				Status *string         `json:"status"`
				Error  json.RawMessage `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return false
		}
		if len(payload.Response.Error) > 0 && string(payload.Response.Error) != "null" {
			return false
		}
		if payload.Response.Status == nil {
			return true
		}

		status := strings.ToLower(strings.TrimSpace(*payload.Response.Status))
		return status == "completed"
	}

	// For chat completions, check for [DONE] event
	return bytes.Equal(event.Data, llm.DoneStreamEvent.Data) ||
		// For Anthropic Messages API, check for message_stop event
		event.Type == "message_stop" ||
		// For OpenAI audio APIs (TTS sse / STT stream) which have no [DONE] sentinel:
		// rely on the terminal *.done event surfaced as StreamEvent.Type.
		event.Type == "speech.audio.done" ||
		event.Type == "transcript.text.done" ||
		event.Type == httpclient.BinaryStreamDoneEventType
}

// aggregatedTerminalFailure returns an actionable error for an explicit
// protocol terminal that was not successful. It is intentionally separate from
// channel health: a response.incomplete after useful output can still prove the
// channel usable while the request/execution remains non-completed in storage.
func aggregatedTerminalFailure(meta llm.ResponseMeta) error {
	if !meta.Terminal || meta.Completed {
		return nil
	}
	return protocolTerminalFailure(meta.ProtocolStatus, meta.IncompleteReason)
}

func responseProtocolTerminalFailure(response *llm.Response) error {
	if response == nil || strings.TrimSpace(response.ProtocolStatus) == "" {
		return nil
	}
	return protocolTerminalFailure(response.ProtocolStatus, response.IncompleteReason)
}

func protocolTerminalFailure(protocolStatus string, incompleteReason string) error {
	status := strings.ToLower(strings.TrimSpace(protocolStatus))
	if status == "completed" {
		return nil
	}
	if status == "" {
		status = "not_completed"
	}
	code := "response_" + status
	message := "upstream response ended with protocol status " + status
	if status == "incomplete" {
		message = "upstream response incomplete"
	}
	if reason := strings.TrimSpace(incompleteReason); reason != "" {
		message += ": " + reason
	}

	return &llm.ResponseError{
		StatusCode: http.StatusBadGateway,
		Detail: llm.ErrorDetail{
			Type:    "api_error",
			Code:    code,
			Message: message,
		},
	}
}

func (ts *InboundPersistentStream) Err() error {
	return ts.stream.Err()
}

func (ts *InboundPersistentStream) Close() error {
	if ts.closed {
		return nil
	}

	ts.closed = true
	ctx := ts.ctx

	log.Debug(ctx, "Closing persistent stream", log.Int("chunk_count", len(ts.responseChunks)), log.Bool("received_done", ts.state.StreamCompleted))

	streamErr := ts.stream.Err()
	ctxErr := ctx.Err()

	// If we received the [DONE] event, treat the stream as successfully completed
	// even if there's a context cancellation error. This handles the case where
	// the client disconnects immediately after receiving the last chunk.
	if ts.state.StreamCompleted {
		// Stream completed successfully - perform final persistence
		log.Debug(ctx, "Stream completed successfully (received terminal event), performing final persistence")
		ts.persistResponseChunks(ctx)

		return ts.stream.Close()
	}

	// If there's an explicit stream error (not just context cancellation), treat as failure
	// regardless of what chunks we have. Stream errors indicate the upstream response
	// was incomplete or corrupted.
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) && !errors.Is(streamErr, context.DeadlineExceeded) {
		if ts.request != nil {
			persistCtx := context.WithoutCancel(ctx)

			if err := ts.requestService.UpdateRequestStatusFromError(persistCtx, ts.request.ID, streamErr); err != nil {
				log.Warn(persistCtx, "Failed to update request status from error", log.Cause(err))
			}
		}

		return ts.stream.Close()
	}

	// If we haven't received a terminal event, check if the chunks we DO have form a complete response.
	// This handles models that aggregate internally (like Codex) or upstream proxy hung connections
	// where the provider sent the full JSON payload but failed to send [DONE] before dropping.
	var responseBody []byte
	var meta llm.ResponseMeta
	var aggErr error

	if len(ts.responseChunks) > 0 && !ts.state.StreamCompleted {
		responseBody, meta, aggErr = ts.transformer.AggregateStreamChunks(context.WithoutCancel(ctx), ts.responseChunks)
		if aggErr == nil && meta.ID != "" && len(responseBody) > 0 && meta.Completed {
			log.Debug(ctx, "Stream has valid complete response without terminal event, treating as completed")
			ts.state.StreamCompleted = true
		}
	}

	if aggErr == nil {
		if terminalErr := aggregatedTerminalFailure(meta); terminalErr != nil {
			persistCtx := context.WithoutCancel(ctx)
			if ts.request != nil {
				if err := ts.requestService.UpdateRequestStatusFromError(persistCtx, ts.request.ID, terminalErr); err != nil {
					log.Warn(persistCtx, "Failed to update request status from incomplete terminal", log.Cause(err))
				}
				if err := ts.requestService.SaveRequestChunks(persistCtx, ts.request.ID, ts.responseChunks); err != nil {
					log.Warn(persistCtx, "Failed to save incomplete request chunks", log.Cause(err))
				}
			}

			return ts.stream.Close()
		}
	}

	// Check if context was canceled (client disconnected before [DONE]).
	// Skip the error path if we determined the stream actually completed successfully above.
	if (ctxErr != nil || streamErr != nil) && !ts.state.StreamCompleted {
		if ts.request != nil {
			persistCtx := context.WithoutCancel(ctx)

			errToReport := ctxErr
			if errToReport == nil {
				errToReport = streamErr
			}

			if err := ts.requestService.UpdateRequestStatusFromError(persistCtx, ts.request.ID, errToReport); err != nil {
				log.Warn(persistCtx, "Failed to update request status from error", log.Cause(err))
			}
		}

		return ts.stream.Close()
	}

	// If the stream ended without a terminal event and we couldn't determine it was
	// completed through aggregation, mark it as incomplete/failed. This handles the case
	// where the upstream connection drops silently (EOF) without sending a terminal event,
	// which would otherwise fall through and incorrectly mark the request as "completed".
	if !ts.state.StreamCompleted {
		log.Debug(ctx, "Stream ended without terminal event or completed response, treating as incomplete")

		if ts.request != nil {
			persistCtx := context.WithoutCancel(ctx)

			errToReport := aggErr
			if errToReport == nil {
				errToReport = errors.New("stream ended without terminal event or completed response")
			}

			if err := ts.requestService.UpdateRequestStatusFromError(persistCtx, ts.request.ID, errToReport); err != nil {
				log.Warn(persistCtx, "Failed to update request status from error", log.Cause(err))
			}
		}

		return ts.stream.Close()
	}

	// Stream completed successfully - perform final persistence
	log.Debug(ctx, "Stream completed successfully, performing final persistence")

	// We already aggregated the chunks above, so pass them directly to avoid double-aggregation
	if len(responseBody) > 0 {
		ts._persistResponse(context.WithoutCancel(ctx), responseBody, meta)
	} else {
		ts.persistResponseChunks(ctx)
	}

	return ts.stream.Close()
}

func (ts *InboundPersistentStream) persistResponseChunks(ctx context.Context) {
	defer func() {
		if cause := recover(); cause != nil {
			log.Warn(ctx, "Failed to persist inbound response chunks", log.Any("cause", cause))
		}
	}()

	// Use context without cancellation to ensure persistence even if client canceled
	persistCtx := context.WithoutCancel(ctx)

	// Aggregate stream chunks first, then delegate to _persistResponse
	responseBody, meta, err := ts.transformer.AggregateStreamChunks(persistCtx, ts.responseChunks)
	if err != nil {
		log.Warn(persistCtx, "Failed to aggregate chunks for main request", log.Cause(err))
		dumper.DumpStreamEvents(persistCtx, ts.responseChunks, "response_chunks.json")
		if ts.request != nil {
			if updateErr := ts.requestService.UpdateRequestStatusFromError(persistCtx, ts.request.ID, err); updateErr != nil {
				log.Warn(persistCtx, "Failed to update request status from aggregation error", log.Cause(updateErr))
			}
		}

		return
	}
	if terminalErr := aggregatedTerminalFailure(meta); terminalErr != nil {
		ts.state.StreamCompleted = false
		if ts.request != nil {
			if updateErr := ts.requestService.UpdateRequestStatusFromError(persistCtx, ts.request.ID, terminalErr); updateErr != nil {
				log.Warn(persistCtx, "Failed to update request status from incomplete terminal", log.Cause(updateErr))
			}
			if saveErr := ts.requestService.SaveRequestChunks(persistCtx, ts.request.ID, ts.responseChunks); saveErr != nil {
				log.Warn(persistCtx, "Failed to save incomplete request chunks", log.Cause(saveErr))
			}
		}

		return
	}

	ts._persistResponse(persistCtx, responseBody, meta)
}

// _persistResponse performs the actual persistence with pre-aggregated data.
// This avoids redundant aggregation when the data is already available.
func (ts *InboundPersistentStream) _persistResponse(ctx context.Context, responseBody []byte, meta llm.ResponseMeta) {
	if ts.request == nil {
		return
	}
	if terminalErr := aggregatedTerminalFailure(meta); terminalErr != nil {
		ts.state.StreamCompleted = false
		if err := ts.requestService.UpdateRequestStatusFromError(ctx, ts.request.ID, terminalErr); err != nil {
			log.Warn(ctx, "Failed to update request status from incomplete terminal", log.Cause(err))
		}
		if err := ts.requestService.SaveRequestChunks(ctx, ts.request.ID, ts.responseChunks); err != nil {
			log.Warn(ctx, "Failed to save incomplete request chunks", log.Cause(err))
		}

		return
	}

	// Build latency metrics from performance record
	var metrics *biz.LatencyMetrics

	if ts.perf != nil {
		firstTokenLatencyMs, requestLatencyMs, _ := ts.perf.Calculate()

		metrics = &biz.LatencyMetrics{
			LatencyMs: &requestLatencyMs,
		}
		if ts.perf.Stream && ts.perf.FirstTokenTime != nil {
			metrics.FirstTokenLatencyMs = &firstTokenLatencyMs
		}
	}

	err := ts.requestService.UpdateRequestCompleted(ctx, ts.request.ID, meta.ID, responseBody, metrics)
	if err != nil {
		log.Warn(ctx, "Failed to update request status to completed", log.Cause(err))
	}

	// Save all response chunks at once
	if err := ts.requestService.SaveRequestChunks(ctx, ts.request.ID, ts.responseChunks); err != nil {
		log.Warn(ctx, "Failed to save request chunks", log.Cause(err))
	}
}

// PersistentInboundTransformer wraps an inbound transformer with enhanced capabilities.
type PersistentInboundTransformer struct {
	wrapped transformer.Inbound
	state   *PersistenceState
}

func (p *PersistentInboundTransformer) TransformError(ctx context.Context, rawErr error) *httpclient.Error {
	return p.wrapped.TransformError(ctx, rawErr)
}

func (p *PersistentInboundTransformer) TransformRequest(ctx context.Context, request *httpclient.Request) (*llm.Request, error) {
	llmRequest, err := p.wrapped.TransformRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	llmRequest.RawRequest = request
	p.state.RawRequest = request
	p.state.LlmRequest = llmRequest
	p.state.OriginalRequestStream = llmRequest.Stream

	return llmRequest, nil
}

func (p *PersistentInboundTransformer) TransformResponse(ctx context.Context, response *llm.Response) (*httpclient.Response, error) {
	return p.wrapped.TransformResponse(ctx, response)
}

func (p *PersistentInboundTransformer) TransformStream(ctx context.Context, stream streams.Stream[*llm.Response]) (streams.Stream[*httpclient.StreamEvent], error) {
	channelStream, err := p.wrapped.TransformStream(ctx, stream)
	if err != nil {
		return nil, err
	}

	persistentStream := NewInboundPersistentStream(
		ctx,
		channelStream,
		p.state.Request,
		p.state.RequestExec,
		p.state.RequestService,
		p, // Use the PersistentInboundTransformer as the transformer
		p.state.Perf,
		p.state,
	)

	return persistentStream, nil
}

func (p *PersistentInboundTransformer) AggregateStreamChunks(ctx context.Context, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return p.wrapped.AggregateStreamChunks(ctx, chunks)
}
