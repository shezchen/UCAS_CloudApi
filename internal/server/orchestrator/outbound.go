package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	requestent "github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

// OutboundPersistentStream wraps a stream and tracks all responses for final saving to database.
// It implements the streams.Stream interface and handles persistence in the Close method.
//
//nolint:containedctx // Checked.
type OutboundPersistentStream struct {
	ctx context.Context

	RequestService  *biz.RequestService
	UsageLogService *biz.UsageLogService

	stream      streams.Stream[*httpclient.StreamEvent]
	request     *ent.Request
	requestExec *ent.RequestExecution

	transformer      transformer.Outbound
	perf             *biz.PerformanceRecord
	responseChunks   []*httpclient.StreamEvent
	closed           bool
	state            *PersistenceState
	attemptFinalizer func(context.Context, bool)
}

var _ streams.Stream[*httpclient.StreamEvent] = (*OutboundPersistentStream)(nil)

func NewOutboundPersistentStream(
	ctx context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	request *ent.Request,
	requestExec *ent.RequestExecution,
	requestService *biz.RequestService,
	usageLogService *biz.UsageLogService,
	outboundTransformer transformer.Outbound,
	perf *biz.PerformanceRecord,
	state *PersistenceState,
	attemptFinalizers ...func(context.Context, bool),
) *OutboundPersistentStream {
	s := &OutboundPersistentStream{
		ctx:             ctx,
		stream:          stream,
		request:         request,
		requestExec:     requestExec,
		RequestService:  requestService,
		UsageLogService: usageLogService,
		transformer:     outboundTransformer,
		perf:            perf,
		responseChunks:  make([]*httpclient.StreamEvent, 0),
		closed:          false,
		state:           state,
	}
	if len(attemptFinalizers) > 0 {
		s.attemptFinalizer = attemptFinalizers[0]
	}

	return s
}

func (ts *OutboundPersistentStream) closeWithAttemptOutcome(success bool) error {
	err := ts.stream.Close()
	if err != nil {
		success = false
	}
	if ts.attemptFinalizer != nil {
		ts.attemptFinalizer(ts.ctx, success)
	}
	return err
}

func (ts *OutboundPersistentStream) Next() bool {
	return ts.stream.Next()
}

func (ts *OutboundPersistentStream) Current() *httpclient.StreamEvent {
	event := ts.stream.Current()
	if event != nil {
		// For raw binary audio chunks (TTS stream_format=audio), persist only a size
		// summary to avoid buffering the full audio payload in memory.
		ts.responseChunks = append(ts.responseChunks, httpclient.SummarizeBinaryChunk(event))
		// Check if this is a terminal event, which indicates the stream completed successfully.
		// For Chat Completions API this is the raw [DONE] event; for Responses API this is
		// response.completed; for Anthropic Messages API this is message_stop.
		if isSuccessfulOutboundTerminalStreamEvent(ts.transformer.APIFormat(), event) {
			ts.state.OutboundStreamCompleted = true
		}
	}

	return event
}

// isSuccessfulOutboundTerminalStreamEvent applies provider-format terminal
// semantics. Responses streams require a real, parseable response.completed
// JSON event; their optional transport [DONE] marker (or a bare event type with
// no payload) never proves model completion.
func isSuccessfulOutboundTerminalStreamEvent(apiFormat llm.APIFormat, event *httpclient.StreamEvent) bool {
	if !isResponsesFormat(apiFormat) {
		return isSuccessfulTerminalStreamEvent(event)
	}
	if event == nil || len(event.Data) == 0 {
		return false
	}

	var payload struct {
		Type     string `json:"type"`
		Response *struct {
			Status *string         `json:"status"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(event.Data, &payload); err != nil || payload.Type != "response.completed" || payload.Response == nil {
		return false
	}

	normalized := *event
	normalized.Type = payload.Type
	return isSuccessfulTerminalStreamEvent(&normalized)
}

func (ts *OutboundPersistentStream) Err() error {
	return ts.stream.Err()
}

func (ts *OutboundPersistentStream) Close() error {
	if ts.closed {
		return nil
	}

	ts.closed = true
	ctx := ts.ctx

	log.Debug(ctx, "Closing persistent stream", log.Int("chunk_count", len(ts.responseChunks)), log.Bool("received_done", ts.state.OutboundStreamCompleted))

	streamErr := ts.stream.Err()
	ctxErr := ctx.Err()

	var responseBody []byte
	var meta llm.ResponseMeta
	var aggErr error
	aggregatedTerminal := false
	aggregatedCompleted := false

	if len(ts.responseChunks) > 0 {
		responseBody, meta, aggErr = ts.transformer.AggregateStreamChunks(context.WithoutCancel(ctx), ts.state.RawProviderRequest, ts.responseChunks)
		outcome := pipeline.ResponseMetaTerminalOutcome(meta)
		aggregatedTerminal = aggErr == nil && outcome.Terminal
		aggregatedCompleted = aggregatedTerminal && outcome.Successful
		ts.logFinalizationDecision(ctx, "aggregated_outbound_chunks", streamErr, ctxErr, aggregatedCompleted, aggErr)
		if aggregatedCompleted {
			log.Debug(ctx, "Stream has valid complete response without terminal event, treating as completed")
			ts.state.OutboundStreamCompleted = true
		}
	} else {
		ts.logFinalizationDecision(ctx, "no_outbound_chunks_to_aggregate", streamErr, ctxErr, false, nil)
	}

	if aggErr == nil && aggregatedTerminal && !aggregatedCompleted {
		terminalErr := aggregatedTerminalFailure(meta)
		if terminalErr == nil {
			terminalErr = errors.New("upstream stream reached a non-completed terminal state")
		}
		persistCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
		defer cancel()
		ts.persistAggregatedFailure(persistCtx, meta, terminalErr)

		return ts.closeWithAttemptOutcome(false)
	}

	acceptedSemanticOutput := ts.state.AttemptAccepted && ts.state.AttemptSemanticOutput
	providerCompleted := ts.state.OutboundStreamCompleted || aggregatedCompleted
	if aggErr != nil || !acceptedSemanticOutput || !providerCompleted {
		decision := "incomplete_stream_without_terminal_event"
		if aggErr != nil {
			decision = "stream_aggregation_failed"
		} else if !acceptedSemanticOutput {
			decision = "stream_not_accepted_or_empty"
		} else if streamErr != nil || ctxErr != nil {
			decision = "incomplete_stream_with_error"
		}
		ts.logFinalizationDecision(ctx, decision, streamErr, ctxErr, aggregatedCompleted, aggErr)
		persistCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
		defer cancel()

		errToReport := streamErr
		if errToReport == nil {
			errToReport = ctxErr
		}
		if errToReport == nil {
			errToReport = aggErr
		}
		if errToReport == nil && !acceptedSemanticOutput {
			errToReport = pipeline.ErrEmptyResponse
		}
		if errToReport == nil {
			errToReport = llm.ErrStreamIncomplete
		}
		ts.persistAggregatedFailure(persistCtx, meta, errToReport)

		return ts.closeWithAttemptOutcome(false)
	}

	// Stream completed successfully - perform final persistence
	log.Debug(ctx, "Stream completed successfully, performing final persistence")
	decision := "completed_after_aggregation"
	if len(responseBody) == 0 {
		decision = "completed_via_chunk_persistence"
	}
	ts.logFinalizationDecision(ctx, decision, streamErr, ctxErr, aggregatedCompleted, aggErr)

	if len(responseBody) > 0 {
		ts.persistAggregatedResponse(context.WithoutCancel(ctx), responseBody, meta)
	} else {
		ts.persistResponseChunks(ctx)
	}

	return ts.closeWithAttemptOutcome(true)
}

func (ts *OutboundPersistentStream) logFinalizationDecision(ctx context.Context, decision string, streamErr error, ctxErr error, aggregatedCompleted bool, aggregatedErr error) {
	fields := []log.Field{
		log.String("decision", decision),
		log.Bool("terminal_event_seen", ts.state.OutboundStreamCompleted),
		log.Bool("attempt_accepted", ts.state.AttemptAccepted),
		log.Bool("semantic_output", ts.state.AttemptSemanticOutput),
		log.Int("chunk_count", len(ts.responseChunks)),
		log.String("api_format", string(ts.transformer.APIFormat())),
		log.Bool("aggregated_completed", aggregatedCompleted),
	}

	if streamErr != nil {
		fields = append(fields, log.String("stream_err", streamErr.Error()))
	}
	if ctxErr != nil {
		fields = append(fields, log.String("ctx_err", ctxErr.Error()))
	}
	if aggregatedErr != nil {
		fields = append(fields, log.String("aggregated_err", aggregatedErr.Error()))
	}

	log.Debug(ctx, "Outbound stream finalization decision", fields...)
}

func (ts *OutboundPersistentStream) persistResponseChunks(ctx context.Context) {
	defer func() {
		if cause := recover(); cause != nil {
			log.Warn(ctx, "Failed to persist outbound response chunks", log.Any("cause", cause))
		}
	}()

	// Update request execution with aggregated chunks
	if ts.requestExec != nil {
		// Use context without cancellation to ensure persistence even if client canceled
		persistCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
		defer cancel()

		responseBody, meta, err := ts.transformer.AggregateStreamChunks(persistCtx, ts.state.RawProviderRequest, ts.responseChunks)
		if err != nil {
			log.Warn(persistCtx, "Failed to aggregate chunks using transformer", log.Cause(err))
			if updateErr := ts.RequestService.UpdateRequestExecutionStatusFromError(persistCtx, ts.requestExec.ID, err); updateErr != nil {
				log.Warn(persistCtx, "Failed to update request execution status from aggregation error", log.Cause(updateErr))
			}
			return
		}
		if terminalErr := aggregatedTerminalFailure(meta); terminalErr != nil {
			ts.persistAggregatedFailure(persistCtx, meta, terminalErr)
			return
		}

		ts.persistAggregatedResponse(persistCtx, responseBody, meta)
	}
}

func (ts *OutboundPersistentStream) persistAggregatedFailure(ctx context.Context, meta llm.ResponseMeta, terminalErr error) {
	ts.state.OutboundStreamCompleted = false
	if ts.requestExec == nil {
		return
	}

	// An accepted attempt was committed to the client and can no longer be
	// transparently replaced by another route, so any usage billed upstream must
	// be metered — including usage-only failure terminals with no semantic
	// output (the upstream still charged for the prompt). A pre-commit failure
	// remains eligible for failover, so it must not claim the logical request's
	// one usage record or steal attribution from a later successful attempt.
	if usage := meta.Usage; usage != nil {
		if ts.state.AttemptAccepted {
			if _, err := ts.UsageLogService.CreateUsageLogFromRequest(ctx, ts.request, ts.requestExec, usage); err != nil {
				log.Warn(ctx, "Failed to create usage log from accepted failed request", log.Cause(err))
			}
		} else {
			// Surface the unmetered upstream cost for reconciliation. The raw
			// chunks saved below preserve the full billing evidence on the
			// per-attempt execution record.
			log.Warn(ctx, "Upstream billed usage on a pre-commit failed attempt is not metered",
				log.Int("request_id", ts.requestExec.RequestID),
				log.Int("request_execution_id", ts.requestExec.ID),
				log.Int("channel_id", ts.requestExec.ChannelID),
				log.String("model_id", ts.requestExec.ModelID),
				log.Int64("prompt_tokens", usage.PromptTokens),
				log.Int64("completion_tokens", usage.CompletionTokens),
				log.Int64("total_tokens", usage.TotalTokens),
				log.Cause(terminalErr),
			)
		}
	}
	if err := ts.RequestService.UpdateRequestExecutionStatusFromError(ctx, ts.requestExec.ID, terminalErr); err != nil {
		log.Warn(ctx, "Failed to update request execution from incomplete terminal", log.Cause(err))
	}
	if err := ts.RequestService.SaveRequestExecutionChunks(ctx, ts.requestExec.ID, ts.responseChunks); err != nil {
		log.Warn(ctx, "Failed to save incomplete request execution chunks", log.Cause(err))
	}
}

func (ts *OutboundPersistentStream) persistAggregatedResponse(ctx context.Context, responseBody []byte, meta llm.ResponseMeta) {
	if ts.requestExec == nil {
		return
	}
	if terminalErr := aggregatedTerminalFailure(meta); terminalErr != nil {
		ts.persistAggregatedFailure(ctx, meta, terminalErr)
		return
	}

	// Try to create usage log from aggregated response
	if usage := meta.Usage; usage != nil {
		_, err := ts.UsageLogService.CreateUsageLogFromRequest(ctx, ts.request, ts.requestExec, usage)
		if err != nil {
			log.Warn(ctx, "Failed to create usage log from request", log.Cause(err))
		}
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

	err := ts.RequestService.UpdateRequestExecutionCompleted(
		ctx,
		ts.requestExec.ID,
		meta.ID,
		responseBody,
		metrics,
	)
	if err != nil {
		log.Warn(
			ctx,
			"Failed to update request execution with chunks, trying basic completion",
			log.Cause(err),
		)
	}

	// Save all response chunks at once
	if err := ts.RequestService.SaveRequestExecutionChunks(ctx, ts.requestExec.ID, ts.responseChunks); err != nil {
		log.Warn(ctx, "Failed to save request execution chunks", log.Cause(err))
	}
}

func isTerminalAggregated(meta llm.ResponseMeta) bool {
	return pipeline.ResponseMetaTerminalOutcome(meta).Terminal
}

var errSkipCandidateByCircuitBreaker = errors.New("skip candidate by circuit breaker")

// PersistentOutboundTransformer wraps an outbound transformer with shared persistence state.
type PersistentOutboundTransformer struct {
	wrapped transformer.Outbound
	state   *PersistenceState
}

func shouldForceStreamingForCandidate(candidate *ChannelModelsCandidate, req *llm.Request) bool {
	if candidate == nil || candidate.Channel == nil || req == nil {
		return false
	}

	if req.Stream != nil && *req.Stream {
		return false
	}

	if candidate.Channel.Policies.Stream != objects.CapabilityPolicyRequire {
		return false
	}

	return supportsAutoAggregateRequest(req)
}

func selectOutboundForCandidate(candidate *ChannelModelsCandidate) transformer.Outbound {
	if candidate == nil || candidate.Channel == nil {
		return nil
	}

	if candidate.APIFormat != "" && candidate.Channel.Outbounds != nil {
		if out, ok := candidate.Channel.Outbounds[candidate.APIFormat]; ok {
			return out
		}
	}

	return candidate.Channel.Outbound
}

// APIFormat returns the API format of the transformer.
func (p *PersistentOutboundTransformer) APIFormat() llm.APIFormat {
	return p.wrapped.APIFormat()
}

// SetStreamAttemptAccepted is called by the pipeline at the point where a
// streaming attempt can no longer be transparently replaced by another route.
func (p *PersistentOutboundTransformer) SetStreamAttemptAccepted(accepted bool) {
	if p == nil || p.state == nil {
		return
	}
	p.state.AttemptAccepted = accepted
}

func (p *PersistentOutboundTransformer) resetProviderAttemptState() {
	if p == nil || p.state == nil {
		return
	}
	p.state.OutboundStreamCompleted = false
	p.state.AttemptAccepted = false
	p.state.AttemptSemanticOutput = false
	p.state.CurrentRouteAttempt = nil
}

func (p *PersistentOutboundTransformer) TransformError(ctx context.Context, rawErr *httpclient.Error) *llm.ResponseError {
	return p.wrapped.TransformError(ctx, rawErr)
}

func (p *PersistentOutboundTransformer) TransformRequest(ctx context.Context, llmRequest *llm.Request) (*httpclient.Request, error) {
	// Candidates should already be selected by inbound transformer
	if len(p.state.ChannelModelsCandidates) == 0 {
		return nil, errors.New("no candidates available: candidates should be selected by inbound transformer")
	}

	// Select current candidate for this attempt
	if p.state.CurrentCandidateIndex >= len(p.state.ChannelModelsCandidates) {
		return nil, fmt.Errorf("%w: all candidates exhausted", biz.ErrInternal)
	}

	candidate := p.state.ChannelModelsCandidates[p.state.CurrentCandidateIndex]
	if p.state.ForcedCredential != "" {
		candidateCopy := *candidate
		candidateCopy.ForcedCredential = p.state.ForcedCredential
		keys := enabledCandidateCredentials(candidate.Channel)
		if len(keys) != 1 || keys[0] != p.state.ForcedCredential {
			forcedChannel, err := p.state.ChannelService.GetChannelWithKey(ctx, candidate.Channel.ID, p.state.ForcedCredential)
			if err != nil {
				return nil, fmt.Errorf("failed to build TestChannel-PASS credential route: %w", err)
			}
			candidateCopy.Channel = forcedChannel
		}
		candidate = &candidateCopy
	}
	entry := candidate.Models[p.state.CurrentModelIndex]

	p.state.CurrentCandidate = candidate
	p.state.StreamCompleted = false
	p.resetProviderAttemptState()

	p.wrapped = selectOutboundForCandidate(candidate)

	log.Debug(ctx, "using candidate",
		log.String("channel", candidate.Channel.Name),
		log.String("request_model", p.state.OriginalModel),
		log.String("actual_model", entry.ActualModel),
		log.String("api_format", candidate.APIFormat),
	)

	llmRequest.Model = entry.ActualModel

	// Apply channel transform options to create a new request
	llmRequest = applyTransformOptions(llmRequest, candidate.Channel.Settings)
	llmRequest = filterResponseCustomToolMessagesForNonResponsesOutbound(llmRequest, p.wrapped.APIFormat())

	if shouldForceStreamingForCandidate(candidate, llmRequest) {
		streamPtr := lo.ToPtr(true)
		llmRequest.Stream = streamPtr
		if llmRequest.StreamOptions == nil {
			llmRequest.StreamOptions = &llm.StreamOptions{}
		}
		llmRequest.StreamOptions.IncludeUsage = true
		if p.state != nil && p.state.LlmRequest != nil {
			p.state.LlmRequest.Stream = streamPtr
			if p.state.LlmRequest.StreamOptions == nil {
				p.state.LlmRequest.StreamOptions = &llm.StreamOptions{}
			}
			p.state.LlmRequest.StreamOptions.IncludeUsage = true
		}
	}

	httpRequest, err := p.wrapped.TransformRequest(ctx, llmRequest)
	p.captureCurrentRoute(ctx)
	return httpRequest, err
}

func filterResponseCustomToolMessagesForNonResponsesOutbound(
	llmRequest *llm.Request,
	outboundFormat llm.APIFormat,
) *llm.Request {
	if llmRequest == nil {
		return nil
	}

	if !isResponsesFormat(llmRequest.APIFormat) || isResponsesFormat(outboundFormat) || !containsResponseCustomToolMessages(llmRequest.Messages) {
		return llmRequest
	}

	cloned := *llmRequest
	cloned.Messages = shared.FilterOutResponseCustomToolMessages(llmRequest.Messages)

	return &cloned
}

func isResponsesFormat(format llm.APIFormat) bool {
	return format == llm.APIFormatOpenAIResponse || format == llm.APIFormatOpenAIResponseCompact
}

func containsResponseCustomToolMessages(messages []llm.Message) bool {
	for _, msg := range messages {
		for _, toolCall := range msg.ToolCalls {
			if toolCall.Type == llm.ToolTypeResponsesCustomTool || toolCall.ResponseCustomToolCall != nil {
				return true
			}
		}
	}

	return false
}

func (p *PersistentOutboundTransformer) TransformResponse(ctx context.Context, response *httpclient.Response) (*llm.Response, error) {
	return p.wrapped.TransformResponse(ctx, response)
}

func (p *PersistentOutboundTransformer) TransformStream(ctx context.Context, req *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	attempt := p.attemptSnapshot(ctx)
	persistentStream := NewOutboundPersistentStream(
		ctx,
		stream,
		p.state.Request,
		p.state.RequestExec,
		p.state.RequestService,
		p.state.UsageLogService,
		p.wrapped, // Pass the wrapped outbound transformer for chunk aggregation
		p.state.Perf,
		p.state,
		func(finalCtx context.Context, success bool) {
			p.triggerProgrammaticTestForAttempt(finalCtx, attempt, success)
		},
	)

	return p.wrapped.TransformStream(ctx, req, persistentStream)
}

func (p *PersistentOutboundTransformer) AggregateStreamChunks(
	ctx context.Context, req *httpclient.Request,
	chunks []*httpclient.StreamEvent,
) ([]byte, llm.ResponseMeta, error) {
	return p.wrapped.AggregateStreamChunks(ctx, req, chunks)
}

// GetRequestExecution returns the current request execution.
func (p *PersistentOutboundTransformer) GetRequestExecution() *ent.RequestExecution {
	return p.state.RequestExec
}

// GetRequest returns the current request.
func (p *PersistentOutboundTransformer) GetRequest() *ent.Request {
	return p.state.Request
}

// GetCurrentChannel returns the current channel.
func (p *PersistentOutboundTransformer) GetCurrentChannel() *biz.Channel {
	if p.state == nil || p.state.CurrentCandidate == nil {
		return nil
	}

	return p.state.CurrentCandidate.Channel
}

// GetCurrentModelID returns the current model ID for logging purposes.
func (p *PersistentOutboundTransformer) GetCurrentModelID() string {
	if p.state.CurrentCandidate == nil || len(p.state.CurrentCandidate.Models) == 0 {
		return ""
	}

	return p.state.CurrentCandidate.Models[p.state.CurrentModelIndex].ActualModel
}

// GetRequestedModel returns the originally requested model ID.
func (p *PersistentOutboundTransformer) GetRequestedModel() string {
	return p.state.OriginalModel
}

func (p *PersistentOutboundTransformer) currentRoute(ctx context.Context) (biz.RouteKey, string, string, bool) {
	if p == nil || p.state == nil || p.state.CurrentCandidate == nil {
		return biz.RouteKey{}, "", "", false
	}
	candidate := p.state.CurrentCandidate
	if candidate.Channel == nil || p.state.CurrentModelIndex < 0 || p.state.CurrentModelIndex >= len(candidate.Models) {
		return biz.RouteKey{}, "", "", false
	}

	entry := candidate.Models[p.state.CurrentModelIndex]
	credential := ""
	if !candidate.Channel.Credentials.IsOAuth() {
		enabledKeys := enabledCandidateCredentials(candidate.Channel)
		if p.state.ForcedCredential != "" && slices.Contains(enabledKeys, p.state.ForcedCredential) {
			credential = p.state.ForcedCredential
		} else if candidate.ForcedCredential != "" && slices.Contains(enabledKeys, candidate.ForcedCredential) {
			credential = candidate.ForcedCredential
		} else if selected, ok := contexts.GetChannelAPIKey(ctx); ok && slices.Contains(enabledKeys, selected) {
			credential = selected
		} else if len(enabledKeys) == 1 {
			credential = enabledKeys[0]
		}
	}

	return biz.RouteKey{
		ChannelID:      candidate.Channel.ID,
		CredentialID:   biz.RouteCredentialFingerprint(candidate.Channel, credential),
		ActualModel:    entry.ActualModel,
		APIFormat:      candidate.APIFormat,
		ConfigRevision: biz.RouteConfigRevision(candidate.Channel, candidate.APIFormat, p.state.Proxy),
	}, credential, entry.RequestModel, true
}

func (p *PersistentOutboundTransformer) captureCurrentRoute(ctx context.Context) {
	if p == nil || p.state == nil {
		return
	}
	key, credential, requestModel, ok := p.currentRoute(ctx)
	if !ok {
		p.state.CurrentRouteAttempt = nil
		return
	}
	attempt := &routeAttempt{Key: key, Credential: credential, RequestModel: requestModel}
	p.state.CurrentRouteAttempt = attempt
	if p.state.AttemptedRoutes == nil {
		p.state.AttemptedRoutes = make(map[biz.RouteKey]struct{})
	}
	p.state.AttemptedRoutes[key] = struct{}{}
}

func (p *PersistentOutboundTransformer) attemptSnapshot(ctx context.Context) *routeAttempt {
	if p == nil || p.state == nil {
		return nil
	}
	if p.state.CurrentRouteAttempt != nil {
		attempt := *p.state.CurrentRouteAttempt
		return &attempt
	}
	key, credential, requestModel, ok := p.currentRoute(ctx)
	if !ok {
		return nil
	}
	return &routeAttempt{Key: key, Credential: credential, RequestModel: requestModel}
}

func enabledCandidateCredentials(channel *biz.Channel) []string {
	if channel == nil {
		return nil
	}
	if keys := channel.GetEnabledAPIKeys(); len(keys) > 0 {
		return keys
	}
	if channel.Channel == nil {
		return nil
	}
	return channel.Credentials.GetEnabledAPIKeys(channel.DisabledAPIKeys)
}

func (p *PersistentOutboundTransformer) resultRouteKey(ctx context.Context) *biz.RouteKey {
	attempt := p.attemptSnapshot(ctx)
	if attempt == nil {
		return nil
	}
	key := attempt.Key
	return &key
}

// OnAttemptFailure never writes route availability. It only starts the same
// TestChannel verdict path asynchronously; the pipeline can fail over without
// waiting for diagnostic work.
func (p *PersistentOutboundTransformer) OnAttemptFailure(ctx context.Context, err error) {
	// A schema-validating 400 describes the request bytes, not route health.
	// Do not launch TestChannel or let the failed client payload influence the
	// binary availability verdict.
	if pipeline.IsDeterministicRequestError(err) {
		return
	}
	p.triggerProgrammaticTest(ctx, false)
}

// OnAttemptSuccess also does not write availability. A production success on a
// TestChannel-failed route merely asks TestChannel to verify recovery.
func (p *PersistentOutboundTransformer) OnAttemptSuccess(ctx context.Context) {
	p.triggerProgrammaticTest(ctx, true)
}

func (p *PersistentOutboundTransformer) triggerProgrammaticTest(ctx context.Context, success bool) {
	if p == nil || p.state == nil || p.state.UnifiedRoutes == nil || p.state.ProgrammaticTester == nil {
		return
	}

	attempt := p.attemptSnapshot(ctx)
	p.triggerProgrammaticTestForAttempt(ctx, attempt, success)
}

func (p *PersistentOutboundTransformer) triggerProgrammaticTestForAttempt(ctx context.Context, attempt *routeAttempt, success bool) {
	if p == nil || p.state == nil || p.state.UnifiedRoutes == nil || p.state.ProgrammaticTester == nil || attempt == nil {
		return
	}
	key := attempt.Key
	if success {
		availability := p.state.UnifiedRoutes.Availability(key)
		if availability.Known && availability.Available {
			return
		}
	}

	tester := p.state.ProgrammaticTester
	testBaseCtx := contexts.DetachForAsync(ctx)
	p.state.UnifiedRoutes.TriggerTest(testBaseCtx, key, func(testCtx context.Context) (biz.RouteTestVerdict, error) {
		testCtx = contexts.WithSource(testCtx, requestent.SourceTest)
		model := attempt.RequestModel
		var (
			passed bool
			detail string
		)
		if attempt.Credential != "" {
			result, err := tester.TestSingleAPIKeyRoute(testCtx, objects.GUID{ID: key.ChannelID}, attempt.Credential, &model, p.state.Proxy, key)
			if err != nil {
				return biz.RouteTestVerdict{}, err
			}
			if result == nil || result.routeKey == nil || *result.routeKey != key || testCtx.Err() != nil {
				return biz.RouteTestVerdict{}, testCtx.Err()
			}
			passed = result.Success
			if result.Error != nil {
				detail = biz.SanitizeCampusDiagnosticError(*result.Error)
			}
		} else {
			result, err := tester.TestChannelRoute(testCtx, objects.GUID{ID: key.ChannelID}, &model, p.state.Proxy, key)
			if err != nil {
				return biz.RouteTestVerdict{}, err
			}
			if result == nil || result.routeKey == nil || *result.routeKey != key || testCtx.Err() != nil {
				return biz.RouteTestVerdict{}, testCtx.Err()
			}
			passed = result.Success
			if result.Error != nil {
				detail = biz.SanitizeCampusDiagnosticError(*result.Error)
			}
		}

		return biz.RouteTestVerdict{Completed: true, Pass: passed, Error: detail}, nil
	})
}

// HasMoreChannels returns true if there are more candidates available for retry.
// It implements the pipeline.Retryable interface.
func (p *PersistentOutboundTransformer) HasMoreChannels() bool {
	if p == nil || p.state == nil || len(p.state.ChannelModelsCandidates) == 0 {
		return false
	}
	return len(p.routeOptions(false, false)) > 0
}

type rescueRouteOption struct {
	candidateIndex int
	modelIndex     int
	route          biz.RouteKey
	credential     string
}

// routeOptions expands the retry universe to exact routes. Credential count
// never affects the primary channel ring; it only creates distinct rescue
// opportunities inside the selected channel slot.
func (p *PersistentOutboundTransformer) routeOptions(passOnly, untriedOnly bool) []rescueRouteOption {
	if p == nil || p.state == nil || p.state.UnifiedRoutes == nil {
		return nil
	}
	options := make([]rescueRouteOption, 0)
	seen := make(map[biz.RouteKey]struct{})
	for candidateIndex, candidate := range p.state.ChannelModelsCandidates {
		if candidate == nil || candidate.Channel == nil {
			continue
		}
		credentials := enabledCandidateCredentials(candidate.Channel)
		if candidate.Channel.Credentials.IsOAuth() {
			// OAuth/coding-plan channels have one effective credential route. Any
			// legacy APIKeys stored beside OAuth are not used by the transformer and
			// must not manufacture extra rescue slots.
			credentials = []string{""}
		}
		if len(credentials) == 0 {
			if len(candidate.Channel.Credentials.GetAllAPIKeys()) > 0 {
				// A key-backed channel with every key disabled has no executable route.
				continue
			}
			credentials = []string{""}
		}
		for modelIndex, entry := range candidate.Models {
			for _, credential := range credentials {
				route := biz.RouteKey{
					ChannelID:      candidate.Channel.ID,
					CredentialID:   biz.RouteCredentialFingerprint(candidate.Channel, credential),
					ActualModel:    entry.ActualModel,
					APIFormat:      candidate.APIFormat,
					ConfigRevision: biz.RouteConfigRevision(candidate.Channel, candidate.APIFormat, p.state.Proxy),
				}
				if _, duplicate := seen[route]; duplicate {
					continue
				}
				seen[route] = struct{}{}
				if passOnly {
					availability := p.state.UnifiedRoutes.Availability(route)
					if !availability.Known || !availability.Available {
						continue
					}
				}
				if untriedOnly {
					if _, tried := p.state.AttemptedRoutes[route]; tried {
						continue
					}
				}
				options = append(options, rescueRouteOption{
					candidateIndex: candidateIndex,
					modelIndex:     modelIndex,
					route:          route,
					credential:     credential,
				})
			}
		}
	}
	return options
}

func (p *PersistentOutboundTransformer) selectRouteOption(options []rescueRouteOption) (rescueRouteOption, bool) {
	if len(options) == 0 {
		return rescueRouteOption{}, false
	}
	channelIDs := make([]int, 0, len(options))
	for _, option := range options {
		channelIDs = append(channelIDs, option.route.ChannelID)
	}
	channelID, ok := p.state.UnifiedRoutes.NextRescue(p.state.OriginalModel, channelIDs)
	if !ok {
		return rescueRouteOption{}, false
	}
	routes := make([]biz.RouteKey, 0, len(options))
	for _, option := range options {
		if option.route.ChannelID == channelID {
			routes = append(routes, option.route)
		}
	}
	route, ok := p.state.UnifiedRoutes.NextRescueRoute(p.state.OriginalModel, channelID, routes)
	if !ok {
		return rescueRouteOption{}, false
	}
	for _, option := range options {
		if option.route == route {
			return option, true
		}
	}
	return rescueRouteOption{}, false
}

// resetPassThroughStreamState cancels the current attempt's fan-out goroutine (if any)
// and clears pass-through stream state so the next attempt starts with a clean slate.
// Must be called before every retry to prevent goroutine leaks and stale reads of
// state.RawStreamErr.
func (p *PersistentOutboundTransformer) resetPassThroughStreamState() {
	if p.state.RawStreamCancel != nil {
		p.state.RawStreamCancel()
		p.state.RawStreamCancel = nil
	}

	p.state.RawStreamCh = nil
	p.state.RawStreamErr = nil
}

// NextChannel moves to the next available candidate for retry.
// It implements the pipeline.Retryable interface.
func (p *PersistentOutboundTransformer) NextChannel(ctx context.Context) error {
	// Cancel any in-flight pass-through stream goroutine from the previous attempt
	// so it exits promptly and releases its upstream HTTP connection.
	p.resetPassThroughStreamState()
	p.resetProviderAttemptState()

	// Verified rescue is authoritative. Within that set, every exact PASS route
	// is tried once before repetition. If no PASS exists at all, the same rule is
	// applied best-effort to every executable route (including FAIL/unknown), and
	// only then are repetitions allowed. The pipeline's configured retry budget
	// remains the sole bound.
	options := p.routeOptions(true, true)
	if len(options) == 0 {
		options = p.routeOptions(true, false)
	}
	if len(options) == 0 {
		options = p.routeOptions(false, true)
	}
	if len(options) == 0 {
		options = p.routeOptions(false, false)
	}
	selected, ok := p.selectRouteOption(options)
	if !ok {
		return errors.New("no more candidates available for retry")
	}
	p.state.CurrentCandidateIndex = selected.candidateIndex
	p.state.CurrentModelIndex = selected.modelIndex
	p.state.ForcedCredential = selected.credential

	// Reset request execution for the new candidate
	p.state.RequestExec = nil
	p.state.PassThroughApplied = false

	candidate := p.state.ChannelModelsCandidates[p.state.CurrentCandidateIndex]
	p.state.CurrentCandidate = candidate
	p.wrapped = selectOutboundForCandidate(candidate)

	if log.DebugEnabled(ctx) {
		model := candidate.Models[p.state.CurrentModelIndex].ActualModel
		log.Debug(ctx, "switching to next channel for retry",
			log.String("channel", candidate.Channel.Name),
			log.String("model", model),
			log.Int("index", p.state.CurrentCandidateIndex),
			log.String("api_format", candidate.APIFormat),
		)
	}

	return nil
}

// CanRetry returns true if the current channel can be retried.
// It implements the pipeline.ChannelRetryable interface, it just check the error is retryable, the
// pipeline will ensure the maxSameChannelRetries is not exceeded.
func (p *PersistentOutboundTransformer) CanRetry(err error) bool {
	// Same-channel status-code and error-pattern retries were a second routing
	// policy. The unified rescue queue always tries a distinct route first and
	// owns any configured repetition budget.
	return false
}

// PrepareForRetry implements the pipeline.ChannelRetryable interface.
// This will reset the request execution for the same channel, so that the same request can be retried.
// It will try the next model in the same channel if available.
func (p *PersistentOutboundTransformer) PrepareForRetry(ctx context.Context) error {
	candidate := p.state.CurrentCandidate

	// Reset request execution for the same channel.
	p.state.RequestExec = nil
	p.state.PassThroughApplied = false
	p.resetProviderAttemptState()

	// Cancel any in-flight pass-through stream goroutine from the previous attempt
	// so it exits promptly and releases its upstream HTTP connection.
	p.resetPassThroughStreamState()

	// If there's another model in the list, advance to it. Prefer a different
	// ActualModel when available so an upstream "model not supported" rejection
	// does not immediately retry the same rejected model.
	if p.state.CurrentModelIndex+1 < len(candidate.Models) {
		nextIndex := p.state.CurrentModelIndex + 1
		currentModel := candidate.Models[p.state.CurrentModelIndex].ActualModel
		for i := nextIndex; i < len(candidate.Models); i++ {
			if candidate.Models[i].ActualModel != currentModel {
				nextIndex = i
				break
			}
		}
		p.state.CurrentModelIndex = nextIndex
		p.wrapped = selectOutboundForCandidate(candidate)

		if log.DebugEnabled(ctx) {
			model := candidate.Models[p.state.CurrentModelIndex].ActualModel
			log.Debug(ctx, "prepared same channel retry for next model",
				log.Any("channel", candidate.Channel.Name),
				log.Any("model", model),
				log.String("api_format", candidate.APIFormat),
				log.Int("current_candidate_index", p.state.CurrentCandidateIndex),
				log.Int("current_entry_index", p.state.CurrentModelIndex),
			)
		}

		return nil
	}

	// Otherwise, we're retrying the current (last) model.
	// It handle the models count less than retry policy.
	if log.DebugEnabled(ctx) {
		model := candidate.Models[p.state.CurrentModelIndex].ActualModel
		log.Debug(ctx, "prepared same channel retry for same model",
			log.Any("channel", candidate.Channel.Name),
			log.Any("model", model),
			log.Int("current_candidate_index", p.state.CurrentCandidateIndex),
			log.Int("current_entry_index", p.state.CurrentModelIndex),
		)
	}

	return nil
}

// CustomizeExecutor customizes the executor for the current channel.
// If the current channel has an executor, it will be used.
// Otherwise, the default executor will be used.
//
// The customized executor will be used to execute the request.
// e.g. the aws bedrock process need a custom executor to handle the request.
// It implements the pipeline.ChannelCustomizedExecutor interface.
func (p *PersistentOutboundTransformer) CustomizeExecutor(executor pipeline.Executor) pipeline.Executor {
	// Start with the default executor, then layer customizations.
	customizedExecutor := executor

	channel := p.GetCurrentChannel()
	if channel == nil {
		return customizedExecutor
	}

	// 1. Apply proxy settings. Test proxy override takes precedence over channel settings.
	if p.state.Proxy != nil {
		if channel.HTTPClient != nil {
			customizedExecutor = channel.HTTPClient.WithProxy(p.state.Proxy)
		} else {
			customizedExecutor = httpclient.NewHttpClientWithProxy(p.state.Proxy)
		}
	} else if channel.HTTPClient != nil {
		// Use the channel's own HTTP client, which is pre-configured with its proxy settings.
		customizedExecutor = channel.HTTPClient
	}
	// 2. Allow the selected outbound transformer (e.g., for AWS signing or Responses WebSocket) to further customize the client.
	outbound := p.wrapped
	if outbound == nil {
		outbound = channel.Outbound
	}
	if custom, ok := outbound.(pipeline.ChannelCustomizedExecutor); ok {
		return custom.CustomizeExecutor(customizedExecutor)
	}

	return customizedExecutor
}
