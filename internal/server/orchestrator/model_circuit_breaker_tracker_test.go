package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestModelCircuitBreakerTracker_EmptyDoesNotRecoverButToolCallDoes(t *testing.T) {
	ctx := context.Background()
	breaker := biz.NewModelCircuitBreaker()
	for range 3 {
		breaker.RecordError(ctx, 7, "gpt-test", false)
	}

	state := &PersistenceState{
		OriginalModel: "gpt-test",
		CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{
			Channel: &ent.Channel{ID: 7},
		}},
	}
	tracker := &modelCircuitBreakerTracker{
		outbound:            &PersistentOutboundTransformer{state: state},
		modelCircuitBreaker: breaker,
	}

	empty := &llm.Response{Choices: []llm.Choice{{Message: &llm.Message{Role: "assistant"}}}}
	_, err := tracker.OnOutboundLlmResponse(ctx, empty)
	require.NoError(t, err)
	stats := breaker.GetModelCircuitBreakerStats(ctx, 7, "gpt-test")
	require.Equal(t, biz.StateHalfOpen, stats.State)
	require.Equal(t, 3, stats.ConsecutiveFailures)

	toolOnly := &llm.Response{Choices: []llm.Choice{{
		Message: &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1"}}},
	}}}
	_, err = tracker.OnOutboundLlmResponse(ctx, toolOnly)
	require.NoError(t, err)
	stats = breaker.GetModelCircuitBreakerStats(ctx, 7, "gpt-test")
	require.Equal(t, biz.StateClosed, stats.State)
	require.Zero(t, stats.ConsecutiveFailures)
}

func TestModelCircuitBreakerStream_UsesActualChannelAndSemanticCompletion(t *testing.T) {
	ctx := context.Background()
	breaker := biz.NewModelCircuitBreaker()
	for range 3 {
		breaker.RecordError(ctx, 9, "gpt-stream", false)
	}

	state := &PersistenceState{
		OriginalModel:   "gpt-stream",
		StreamCompleted: true,
		CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{
			Channel: &ent.Channel{ID: 9},
		}},
	}
	tracker := &modelCircuitBreakerTracker{
		outbound:            &PersistentOutboundTransformer{state: state},
		modelCircuitBreaker: breaker,
	}
	toolEvent := &llm.Response{Choices: []llm.Choice{{
		Delta: &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1"}}},
	}}}
	wrapped, err := tracker.OnOutboundLlmStream(ctx, streams.SliceStream([]*llm.Response{toolEvent, llm.DoneResponse}))
	require.NoError(t, err)

	for wrapped.Next() {
		_ = wrapped.Current()
	}
	require.NoError(t, wrapped.Close())

	stats := breaker.GetModelCircuitBreakerStats(ctx, 9, "gpt-stream")
	require.Equal(t, biz.StateClosed, stats.State)
	require.Zero(t, stats.ConsecutiveFailures)
}

func TestModelCircuitBreakerTracker_LocalProbeLeaseSkipIsNotAHealthFailure(t *testing.T) {
	ctx := context.Background()
	breaker := biz.NewModelCircuitBreaker()
	breaker.RecordError(ctx, 5, "gpt-test", false)

	state := &PersistenceState{
		OriginalModel: "gpt-test",
		CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{
			Channel: &ent.Channel{ID: 5},
		}},
	}
	tracker := &modelCircuitBreakerTracker{
		outbound:            &PersistentOutboundTransformer{state: state},
		modelCircuitBreaker: breaker,
	}
	tracker.OnOutboundRawError(ctx, errSkipCandidateByCircuitBreaker)

	stats := breaker.GetModelCircuitBreakerStats(ctx, 5, "gpt-test")
	require.Equal(t, 1, stats.ConsecutiveFailures)
}

func TestModelCircuitBreakerTracker_HalfOpenStreamSuccessCannotBeDoubleRecordedAsFailure(t *testing.T) {
	ctx := context.Background()
	breaker := biz.NewModelCircuitBreaker()
	for range biz.DefaultModelCircuitBreakerPolicy().HalfOpenThreshold {
		breaker.RecordError(ctx, 12, "gpt-stream", false)
	}

	state := &PersistenceState{
		OriginalModel:   "gpt-stream",
		StreamCompleted: true,
		CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{
			Channel: &ent.Channel{ID: 12},
		}},
	}
	tracker := &modelCircuitBreakerTracker{
		outbound:            &PersistentOutboundTransformer{state: state},
		modelCircuitBreaker: breaker,
	}
	_, err := tracker.OnOutboundRawRequest(ctx, &httpclient.Request{})
	require.NoError(t, err)

	competingTracker := &modelCircuitBreakerTracker{
		outbound:            &PersistentOutboundTransformer{state: state},
		modelCircuitBreaker: breaker,
	}
	_, err = competingTracker.OnOutboundRawRequest(ctx, &httpclient.Request{})
	require.ErrorIs(t, err, errSkipCandidateByCircuitBreaker,
		"a half-open model must admit only one live recovery request")
	competingTracker.OnOutboundRawError(ctx, err)
	require.Equal(t, biz.DefaultModelCircuitBreakerPolicy().HalfOpenThreshold,
		breaker.GetModelCircuitBreakerStats(ctx, 12, "gpt-stream").ConsecutiveFailures,
		"a locally skipped competitor must not damage upstream health")

	toolEvent := &llm.Response{Choices: []llm.Choice{{
		Delta: &llm.Message{
			Role:      "assistant",
			ToolCalls: []llm.ToolCall{{ID: "call-1", Type: "function"}},
		},
	}}}
	wrapped, err := tracker.OnOutboundLlmStream(
		ctx,
		streams.SliceStream([]*llm.Response{toolEvent, llm.DoneResponse}),
	)
	require.NoError(t, err)
	for wrapped.Next() {
		_ = wrapped.Current()
	}
	require.NoError(t, wrapped.Close())

	tracker.OnOutboundRawError(ctx, errors.New("late downstream transform failure"))
	stats := breaker.GetModelCircuitBreakerStats(ctx, 12, "gpt-stream")
	require.Equal(t, biz.StateClosed, stats.State)
	require.Zero(t, stats.ConsecutiveFailures,
		"semantic stream success must win over any later downstream error callback")
	require.False(t, breaker.TryBeginProbe(ctx, 12, "gpt-stream"),
		"a recovered closed model must no longer accept recovery probes")
}
