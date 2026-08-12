package pipeline

import (
	"context"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

// autoAggregatePipeline builds a pipeline whose outbound upgrades the
// non-streaming client request to a stream, which is the only route into
// autoAggregateStream.
func autoAggregatePipeline(t *testing.T, meta llm.ResponseMeta) (*terminalObservingOutbound, *mockMiddleware, *pipeline) {
	t.Helper()

	executor := &mockExecutor{doStream: func(context.Context, *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
		return streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte("raw")}}), nil
	}}

	text := "aggregated text"
	baseOutbound := &mockOutbound{
		transformRequest: func(_ context.Context, request *llm.Request) (*httpclient.Request, error) {
			request.Stream = lo.ToPtr(true)
			return &httpclient.Request{}, nil
		},
		transformStream: func(context.Context, *httpclient.Request, streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
			return streams.SliceStream([]*llm.Response{
				{Choices: []llm.Choice{{Delta: &llm.Message{Content: llm.MessageContent{Content: &text}}}}},
			}), nil
		},
		hasMoreChannels: func() bool { return false },
	}
	outbound := &terminalObservingOutbound{mockOutbound: baseOutbound}

	inbound := &mockInbound{
		transformStream: transformLlmContentToEvents,
		aggregateStreamChunks: func(context.Context, []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
			return []byte(`{"id":"agg"}`), meta, nil
		},
	}

	middleware := &mockMiddleware{}

	return outbound, middleware, NewFactory(executor).Pipeline(inbound, outbound, WithMiddlewares(middleware))
}

func TestAutoAggregateStream_ExplicitTerminalFailureIsRejected(t *testing.T) {
	tests := []struct {
		name string
		meta llm.ResponseMeta
		code string
	}{
		{
			name: "terminal without completed",
			meta: llm.ResponseMeta{ID: "agg", Terminal: true},
			code: "response_not_completed",
		},
		{
			name: "protocol incomplete",
			meta: llm.ResponseMeta{ID: "agg", ProtocolStatus: "incomplete", IncompleteReason: "max_output_tokens"},
			code: "response_incomplete",
		},
		{
			name: "protocol failed",
			meta: llm.ResponseMeta{ID: "agg", ProtocolStatus: "failed"},
			code: "response_failed",
		},
		{
			name: "protocol canceled contradicting completed",
			meta: llm.ResponseMeta{ID: "agg", Terminal: true, Completed: true, ProtocolStatus: "canceled"},
			code: "response_canceled",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outbound, middleware, p := autoAggregatePipeline(t, test.meta)

			result, err := p.Process(context.Background(), &httpclient.Request{})
			require.Nil(t, result)
			require.Error(t, err)

			var responseErr *llm.ResponseError

			require.ErrorAs(t, err, &responseErr)
			require.Equal(t, http.StatusBadGateway, responseErr.StatusCode)
			require.Equal(t, test.code, responseErr.Detail.Code)

			require.Len(t, outbound.failures, 1, "rejected aggregation must notify the attempt observer")
			require.Zero(t, outbound.successes)
			require.Nil(t, outbound.acceptedChanges, "a rejected aggregation must not commit the stream attempt")
			require.Equal(t, 1, middleware.errorCalls)
		})
	}
}

func TestAutoAggregateStream_SilentUpstreamIsServed(t *testing.T) {
	// Providers that never report a finish_reason produce a zero meta. That is
	// not a failure signal, so the attempt must still be served instead of
	// burning a generation on every candidate channel.
	outbound, middleware, p := autoAggregatePipeline(t, llm.ResponseMeta{ID: "agg"})

	result, err := p.Process(context.Background(), &httpclient.Request{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Stream)
	require.NotNil(t, result.Response)
	require.Equal(t, http.StatusOK, result.Response.StatusCode)
	require.Equal(t, `{"id":"agg"}`, string(result.Response.Body))
	require.Empty(t, outbound.failures)
	require.Equal(t, 1, outbound.successes)
	require.Equal(t, []bool{true}, outbound.acceptedChanges)
	require.Zero(t, middleware.errorCalls)
}

func TestAutoAggregateStream_SuccessfulTerminalIsServed(t *testing.T) {
	outbound, middleware, p := autoAggregatePipeline(t, llm.ResponseMeta{ID: "agg", Terminal: true, Completed: true})

	result, err := p.Process(context.Background(), &httpclient.Request{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Response)
	require.Equal(t, http.StatusOK, result.Response.StatusCode)
	require.Equal(t, 1, outbound.successes)
	require.Equal(t, []bool{true}, outbound.acceptedChanges)
	require.Zero(t, middleware.errorCalls)
}
