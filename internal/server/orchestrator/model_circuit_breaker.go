package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

func withModelCircuitBreaker(outbound *PersistentOutboundTransformer, modelCircuitBreaker *biz.ModelCircuitBreaker) pipeline.Middleware {
	return &modelCircuitBreakerTracker{
		outbound:            outbound,
		modelCircuitBreaker: modelCircuitBreaker,
	}
}

type modelCircuitBreakerTracker struct {
	pipeline.DummyMiddleware

	outbound            *PersistentOutboundTransformer
	modelCircuitBreaker *biz.ModelCircuitBreaker

	probeActive      bool
	probeChannelID   int
	probeModelID     string
	attemptSucceeded bool
	attemptWasProbe  bool
}

func (m *modelCircuitBreakerTracker) Name() string {
	return "model-circuit-breaker-tracker"
}

func (m *modelCircuitBreakerTracker) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	if m.modelCircuitBreaker == nil {
		return request, nil
	}

	m.attemptSucceeded = false
	m.attemptWasProbe = false

	channel := m.outbound.GetCurrentChannel()
	modelID := m.outbound.GetRequestedModel()
	if channel == nil || modelID == "" {
		return request, nil
	}

	stats := m.modelCircuitBreaker.GetModelCircuitBreakerStats(ctx, channel.ID, modelID)
	if stats == nil || (stats.State != biz.StateOpen && stats.State != biz.StateHalfOpen) {
		return request, nil
	}

	if !m.modelCircuitBreaker.TryBeginProbe(ctx, channel.ID, modelID) {
		log.Debug(ctx, "skipping candidate by circuit breaker: probe conditions not met or another probe in progress",
			log.Int("channel_id", channel.ID),
			log.String("model_id", modelID),
		)

		return nil, errSkipCandidateByCircuitBreaker
	}

	m.probeActive = true
	m.probeChannelID = channel.ID
	m.probeModelID = modelID
	m.attemptWasProbe = true

	return request, nil
}

func (m *modelCircuitBreakerTracker) OnOutboundLlmResponse(ctx context.Context, response *llm.Response) (*llm.Response, error) {
	if m.outbound == nil || m.outbound.state == nil || m.modelCircuitBreaker == nil {
		return response, nil
	}

	channel := m.outbound.GetCurrentChannel()
	modelID := m.outbound.GetRequestedModel()
	if channel == nil || modelID == "" {
		return response, nil
	}
	if terminal, successful := llmTerminalOutcome(response); terminal && !successful {
		m.modelCircuitBreaker.RecordError(ctx, channel.ID, modelID, m.attemptWasProbe)
		m.probeActive = false
		m.attemptWasProbe = false

		return response, nil
	}
	if !pipeline.HasResponseContent(response) {
		return response, nil
	}

	m.modelCircuitBreaker.RecordSuccess(ctx, channel.ID, modelID)
	m.probeActive = false // RecordSuccess atomically closes the circuit and lease.
	m.attemptSucceeded = true
	m.attemptWasProbe = false

	return response, nil
}

func (m *modelCircuitBreakerTracker) OnOutboundRawError(ctx context.Context, err error) {
	if m.outbound == nil || m.outbound.state == nil || m.modelCircuitBreaker == nil {
		return
	}

	if m.attemptSucceeded {
		m.releaseProbeLease()
		return
	}

	// Capture whether this attempt was an active probe. Keep the lease held
	// through the state/backoff update so no concurrent request can enter the
	// stale transition window.
	wasProbe := m.attemptWasProbe

	if isNeutralAttemptError(ctx, err) {
		m.releaseProbeLease()
		return
	}

	channel := m.outbound.GetCurrentChannel()
	modelID := m.outbound.GetRequestedModel()
	if channel == nil || modelID == "" {
		return
	}

	m.modelCircuitBreaker.RecordError(ctx, channel.ID, modelID, wasProbe)
	m.releaseProbeLease()
	m.attemptWasProbe = false
}

func (m *modelCircuitBreakerTracker) OnOutboundLlmStream(ctx context.Context, stream streams.Stream[*llm.Response]) (streams.Stream[*llm.Response], error) {
	if m.outbound == nil || m.outbound.state == nil || m.modelCircuitBreaker == nil {
		return stream, nil
	}

	channel := m.outbound.GetCurrentChannel()
	modelID := m.outbound.GetRequestedModel()
	if channel == nil || modelID == "" {
		return stream, nil
	}

	return &probeReleasingStream{
		ctx:       ctx,
		stream:    stream,
		state:     m.outbound.state,
		channelID: channel.ID,
		modelID:   modelID,
		wasProbe:  m.probeActive,
		release: func() {
			if m.outbound != nil {
				m.releaseProbeLease()
			}
		},
		onSuccess: func() {
			m.attemptSucceeded = true
			m.attemptWasProbe = false
			m.probeActive = false
		},
		released:            false,
		recorded:            false,
		modelCircuitBreaker: m.modelCircuitBreaker,
	}, nil
}

func (m *modelCircuitBreakerTracker) releaseProbeLease() {
	if m.outbound == nil || m.outbound.state == nil || m.modelCircuitBreaker == nil {
		return
	}

	if !m.probeActive {
		return
	}

	m.modelCircuitBreaker.EndProbe(m.probeChannelID, m.probeModelID)
	m.probeActive = false
}

//nolint:containedctx // Checked.
type probeReleasingStream struct {
	ctx       context.Context
	stream    streams.Stream[*llm.Response]
	state     *PersistenceState
	release   func()
	onSuccess func()
	released  bool
	recorded  bool

	modelCircuitBreaker *biz.ModelCircuitBreaker
	channelID           int
	modelID             string
	wasProbe            bool
	semanticOutput      bool
	terminalFailure     bool
}

func (s *probeReleasingStream) Next() bool {
	return s.stream.Next()
}

func (s *probeReleasingStream) Current() *llm.Response {
	event := s.stream.Current()
	if event == nil {
		return nil
	}

	if s.modelCircuitBreaker == nil {
		return event
	}

	if pipeline.HasResponseContent(event) {
		s.semanticOutput = true
	}

	if !s.recorded {
		if terminal, successful := llmTerminalOutcome(event); terminal && !successful {
			s.terminalFailure = true
			s.modelCircuitBreaker.RecordError(s.ctx, s.channelID, s.modelID, s.wasProbe)
			s.recorded = true
			s.releaseOnce()
		} else if successful && s.semanticOutput {
			s.modelCircuitBreaker.RecordSuccess(s.ctx, s.channelID, s.modelID)
			if s.onSuccess != nil {
				s.onSuccess()
			}
			s.recorded = true
			s.releaseOnce()
		}
	}

	return event
}

func (s *probeReleasingStream) Err() error {
	return s.stream.Err()
}

func (s *probeReleasingStream) Close() error {
	if !s.recorded && (s.semanticOutput || s.terminalFailure) {
		if s.terminalFailure {
			s.modelCircuitBreaker.RecordError(s.ctx, s.channelID, s.modelID, s.wasProbe)
		} else if s.state != nil && s.state.StreamCompleted {
			s.modelCircuitBreaker.RecordSuccess(s.ctx, s.channelID, s.modelID)
			if s.onSuccess != nil {
				s.onSuccess()
			}
		} else if s.ctx.Err() == nil {
			s.modelCircuitBreaker.RecordError(s.ctx, s.channelID, s.modelID, s.wasProbe)
		}
		s.recorded = true
	}

	s.releaseOnce()

	return s.stream.Close()
}

func (s *probeReleasingStream) releaseOnce() {
	if !s.released && s.release != nil {
		s.released = true
		s.release()
	}
}
