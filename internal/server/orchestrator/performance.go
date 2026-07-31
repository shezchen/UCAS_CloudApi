package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

// withPerformanceRecording creates a unified middleware that handles all performance tracking.
// It initializes metrics, tracks first token in streams, and records final metrics.
func withPerformanceRecording(outbound *PersistentOutboundTransformer) pipeline.Middleware {
	return &performanceRecording{
		outbound: outbound,
	}
}

// performanceRecording is a unified middleware that handles all performance tracking.
type performanceRecording struct {
	pipeline.DummyMiddleware

	outbound *PersistentOutboundTransformer
}

func (m *performanceRecording) Name() string {
	return "record-performance"
}

func (m *performanceRecording) OnInboundLlmRequest(ctx context.Context, request *llm.Request) (*llm.Request, error) {
	if m.outbound.state.Perf == nil {
		m.outbound.state.Perf = &biz.PerformanceRecord{}
	}

	if request.Stream != nil {
		m.outbound.state.Perf.Stream = *request.Stream
	} else {
		m.outbound.state.Perf.Stream = false
	}

	return request, nil
}

func (m *performanceRecording) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	// Initialize performance metrics at the start of request
	channel := m.outbound.GetCurrentChannel()
	if channel == nil {
		return request, nil
	}

	// Preserve Stream flag from existing PerformanceRecord (set in OnInboundLlmRequest)
	var streamFlag bool
	if m.outbound.state.Perf != nil {
		streamFlag = m.outbound.state.Perf.Stream
	}

	// Create a new PerformanceRecord instance for each request.
	perf := biz.PerformanceRecord{}
	perf.StartTime = time.Now()
	perf.ChannelID = channel.ID
	perf.Donated = channel.UserID != nil
	perf.Success = false
	perf.RequestCompleted = false
	perf.Stream = streamFlag

	// Get the API key used for this request from context (set by TraceStickyKeyProvider)
	if apiKey, ok := contexts.GetChannelAPIKey(ctx); ok {
		perf.APIKey = apiKey
	}

	m.outbound.state.Perf = &perf

	log.Debug(ctx, "Started performance tracking",
		log.Int("channel_id", channel.ID),
		log.String("channel_name", channel.Name),
	)

	return request, nil
}

func (m *performanceRecording) OnOutboundRawResponse(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
	return response, nil
}

func (m *performanceRecording) OnOutboundLlmResponse(ctx context.Context, response *llm.Response) (*llm.Response, error) {
	if m.outbound.state.Perf == nil {
		return response, nil
	}

	if terminal, successful := llmTerminalOutcome(response); terminal && !successful {
		m.outbound.state.Perf.MarkFailed(http.StatusBadGateway)
		if m.outbound.state.ChannelService != nil {
			m.outbound.state.ChannelService.AsyncRecordPerformance(ctx, m.outbound.state.Perf)
		}

		return response, nil
	}

	// A syntactically decoded response is not necessarily a successful model
	// response. In particular, empty responses must remain retryable and must not
	// reset transient health before the pipeline rejects them.
	if !pipeline.HasResponseContent(response) {
		return response, nil
	}

	if response != nil && response.Usage != nil {
		if tokenCount := response.Usage.GetCompletionTokens(); tokenCount != nil && *tokenCount > 0 {
			m.outbound.state.Perf.CompletionTokens = *tokenCount
		}
	}

	if !m.outbound.state.Perf.RequestCompleted {
		m.outbound.state.Perf.MarkSuccess()
		if m.outbound.state.ChannelService != nil {
			m.outbound.state.ChannelService.AsyncRecordPerformance(ctx, m.outbound.state.Perf)
		}
	}

	return response, nil
}

func (m *performanceRecording) OnOutboundRawStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
	return stream, nil
}

func (m *performanceRecording) OnOutboundLlmStream(ctx context.Context, stream streams.Stream[*llm.Response]) (streams.Stream[*llm.Response], error) {
	return &recordPerformanceStream{
		ctx:    ctx,
		stream: stream,
		state:  m.outbound.state,
	}, nil
}

func (m *performanceRecording) OnOutboundRawError(ctx context.Context, err error) {
	// Record performance metrics for failed requests
	if m.outbound.state.Perf == nil {
		return
	}

	perf := m.outbound.state.Perf
	// Once a semantically valid upstream response has completed, a later
	// downstream transform error must not record the same attempt a second time
	// as an upstream health failure.
	if perf.RequestCompleted {
		return
	}

	if isNeutralAttemptError(ctx, err) {
		perf.MarkCanceled()
	} else {
		errorCode := ExtractErrorCode(err)
		perf.MarkFailed(errorCode)
	}

	if m.outbound.state.ChannelService != nil {
		m.outbound.state.ChannelService.AsyncRecordPerformance(ctx, perf)
	}
}

// isNeutralAttemptError classifies failures that did not establish a usable
// upstream attempt, or were caused by the caller leaving. These must not reduce
// channel health or trigger automatic disable/circuit transitions.
func isNeutralAttemptError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, errSkipCandidateByCircuitBreaker) ||
		isChannelQueueError(err) ||
		isLocalRPMExhaustedError(err)
}

// recordPerformanceStream records performance metrics for a stream of responses.
//
//nolint:containedctx // ctx is used for logging.
type recordPerformanceStream struct {
	ctx    context.Context
	stream streams.Stream[*llm.Response]
	state  *PersistenceState

	firstTokenSet      bool
	reasoningStartSet  bool
	reasoningEndSet    bool
	semanticOutput     bool
	recorded           bool
	terminalFailure    bool
	protocolIncomplete bool
}

func (s *recordPerformanceStream) Current() *llm.Response {
	event := s.stream.Current()
	if event == nil {
		return event
	}

	if tokenCount := event.Usage.GetCompletionTokens(); tokenCount != nil && *tokenCount > 0 && s.state.Perf != nil {
		s.state.Perf.CompletionTokens = *tokenCount
	}

	if pipeline.HasResponseContent(event) {
		s.semanticOutput = true
		if s.state != nil {
			s.state.AttemptSemanticOutput = true
		}
		if !s.firstTokenSet && s.state.Perf != nil {
			s.state.Perf.MarkFirstToken()
			s.firstTokenSet = true
		}
	}

	status := strings.ToLower(strings.TrimSpace(event.ProtocolStatus))
	if status == "incomplete" || status == "failed" || status == "error" || status == "canceled" || status == "cancelled" {
		s.protocolIncomplete = true
	}
	if !s.protocolIncomplete && isSuccessfulUnifiedStreamTerminal(event) && s.state != nil {
		s.state.OutboundStreamCompleted = true
	}

	if s.state.Perf != nil && len(event.Choices) > 0 {
		delta := event.Choices[0].Delta
		if delta != nil {
			if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
				if !s.reasoningStartSet {
					s.state.Perf.MarkReasoningStart()
					s.reasoningStartSet = true
				}
			} else if (delta.Content.Content != nil && *delta.Content.Content != "") || len(delta.Content.MultipleContent) > 0 || len(delta.ToolCalls) > 0 {
				if s.reasoningStartSet && !s.reasoningEndSet {
					s.state.Perf.MarkReasoningEnd()
					s.reasoningEndSet = true
				}
			}
		}
	}

	if terminal, successful := llmTerminalOutcome(event); terminal && !successful {
		s.terminalFailure = true
		s.recordFailure()
	} else if successful && s.semanticOutput {
		s.recordSuccess()
	}

	return event
}

func isSuccessfulUnifiedStreamTerminal(response *llm.Response) bool {
	if response == nil || response.Error != nil {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(response.ProtocolStatus)) {
	case "completed":
		return true
	case "incomplete", "failed", "error", "canceled", "cancelled":
		return false
	}

	if response == llm.DoneResponse || response.Object == "[DONE]" {
		return true
	}
	for _, choice := range response.Choices {
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			return true
		}
	}

	return false
}

func (s *recordPerformanceStream) Next() bool {
	return s.stream.Next()
}

func (s *recordPerformanceStream) Close() error {
	if !s.recorded && s.state != nil && s.state.Perf != nil {
		if s.terminalFailure {
			s.recordFailure()
		} else if s.semanticOutput && s.state.OutboundStreamCompleted {
			s.recordSuccess()
		} else if s.semanticOutput && s.ctx.Err() != nil {
			s.state.Perf.MarkCanceled()
			if s.state.ChannelService != nil {
				s.state.ChannelService.AsyncRecordPerformance(s.ctx, s.state.Perf)
			}
			s.recorded = true
		} else if s.semanticOutput {
			// Meaningful output was already committed to the client, so the
			// pipeline cannot safely retry. Still record the incomplete stream as
			// unhealthy so future sessions avoid the channel temporarily.
			s.state.Perf.MarkFailed(500)
			if s.state.ChannelService != nil {
				s.state.ChannelService.AsyncRecordPerformance(s.ctx, s.state.Perf)
			}
			s.recorded = true
		}
	}

	return s.stream.Close()
}

func llmTerminalOutcome(response *llm.Response) (terminal, successful bool) {
	if response == nil {
		return false, false
	}
	if response.Error != nil {
		return true, false
	}
	if response == llm.DoneResponse || response.Object == "[DONE]" {
		return true, true
	}

	switch strings.ToLower(strings.TrimSpace(response.ProtocolStatus)) {
	case "failed", "canceled", "cancelled", "error":
		return true, false
	case "completed", "incomplete":
		// An explicit response.incomplete is an honest client/request terminal,
		// but it does not make a channel unhealthy after the channel produced
		// meaningful content. Empty-response detection still retries it when no
		// semantic output was produced.
		return true, true
	}

	for _, choice := range response.Choices {
		if choice.FinishReason == nil {
			continue
		}

		if strings.TrimSpace(*choice.FinishReason) != "" {
			// Finish reasons are protocol-local termination reasons, not provider
			// health errors. In particular, a normal Chat Completions `length`
			// response may contain perfectly usable output.
			terminal = true
			successful = true
		}
	}

	return terminal, successful
}

func (s *recordPerformanceStream) Err() error {
	return s.stream.Err()
}

func (s *recordPerformanceStream) recordSuccess() {
	if s.recorded || s.state == nil || s.state.Perf == nil {
		return
	}

	s.state.Perf.MarkSuccess()
	if s.state.ChannelService != nil {
		s.state.ChannelService.AsyncRecordPerformance(s.ctx, s.state.Perf)
	}
	s.recorded = true
}

func (s *recordPerformanceStream) recordFailure() {
	if s.recorded || s.state == nil || s.state.Perf == nil {
		return
	}

	s.state.Perf.MarkFailed(http.StatusBadGateway)
	if s.state.ChannelService != nil {
		s.state.ChannelService.AsyncRecordPerformance(s.ctx, s.state.Perf)
	}
	s.recorded = true
}

// ExtractErrorCode extracts HTTP error code from error.
func ExtractErrorCode(err error) int {
	// Check if error is an HTTP error
	httpErr := &httpclient.Error{}
	if errors.As(err, &httpErr) {
		code := httpErr.StatusCode
		return code
	}

	// Default to 500
	return 500
}

type NoopPerformanceRecording struct {
	pipeline.DummyMiddleware
}

func (m *NoopPerformanceRecording) Name() string {
	return "noop-performance"
}
