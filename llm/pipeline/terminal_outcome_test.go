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

type terminalObservingOutbound struct {
	*mockOutbound
	successes int
	failures  []error
}

func (o *terminalObservingOutbound) OnAttemptSuccess(context.Context) {
	o.successes++
}

func (o *terminalObservingOutbound) OnAttemptFailure(_ context.Context, err error) {
	o.failures = append(o.failures, err)
}

func semanticTextResponse(status, finishReason string) *llm.Response {
	text := "provider returned partial text"
	return &llm.Response{
		ProtocolStatus: status,
		Choices: []llm.Choice{{
			Message:      &llm.Message{Content: llm.MessageContent{Content: &text}},
			FinishReason: lo.ToPtr(finishReason),
		}},
	}
}

func semanticToolResponse(status string) *llm.Response {
	return &llm.Response{
		ProtocolStatus: status,
		Choices: []llm.Choice{{
			Message:      &llm.Message{ToolCalls: []llm.ToolCall{{ID: "call-partial", Type: "function"}}},
			FinishReason: lo.ToPtr("tool_calls"),
		}},
	}
}

func TestResponseTerminalOutcome_ExplicitProtocolFailureOverridesSemanticOutput(t *testing.T) {
	tests := []struct {
		name     string
		response *llm.Response
		code     string
	}{
		{name: "incomplete text", response: semanticTextResponse("incomplete", "length"), code: "response_incomplete"},
		{name: "failed text", response: semanticTextResponse("failed", "stop"), code: "response_failed"},
		{name: "canceled tool call", response: semanticToolResponse("canceled"), code: "response_canceled"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := ResponseTerminalOutcome(test.response)
			require.True(t, outcome.Terminal)
			require.False(t, outcome.Successful)

			var responseErr *llm.ResponseError
			require.ErrorAs(t, outcome.Err, &responseErr)
			require.Equal(t, http.StatusBadGateway, responseErr.StatusCode)
			require.Equal(t, test.code, responseErr.Detail.Code)
		})
	}
}

func TestResponseTerminalOutcome_ChatFinishReasonsRemainSuccessful(t *testing.T) {
	for _, finishReason := range []string{"length", "tool_calls", "content_filter"} {
		t.Run(finishReason, func(t *testing.T) {
			outcome := ResponseTerminalOutcome(semanticTextResponse("", finishReason))
			require.True(t, outcome.Terminal)
			require.True(t, outcome.Successful)
			require.NoError(t, outcome.Err)
		})
	}
}

func TestResponseMetaTerminalOutcome_ExplicitFailureWinsContradictoryCompletedFlag(t *testing.T) {
	outcome := ResponseMetaTerminalOutcome(llm.ResponseMeta{
		Terminal:         true,
		Completed:        true,
		ProtocolStatus:   "incomplete",
		IncompleteReason: "max_output_tokens",
	})
	require.True(t, outcome.Terminal)
	require.False(t, outcome.Successful)
	require.ErrorContains(t, outcome.Err, "max_output_tokens")
}

func TestPipeline_NonStreamProtocolTerminalFailureFailsOverBeforeSuccessMiddlewares(t *testing.T) {
	tests := []struct {
		name     string
		response *llm.Response
	}{
		{name: "incomplete with text", response: semanticTextResponse("incomplete", "length")},
		{name: "failed with text", response: semanticTextResponse("failed", "stop")},
		{name: "canceled with tool call", response: semanticToolResponse("canceled")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			executor := &mockExecutor{do: func(context.Context, *httpclient.Request) (*httpclient.Response, error) {
				attempts++
				return &httpclient.Response{StatusCode: http.StatusOK}, nil
			}}
			baseOutbound := &mockOutbound{
				transformResponse: func(context.Context, *httpclient.Response) (*llm.Response, error) {
					if attempts == 1 {
						return test.response, nil
					}
					return semanticTextResponse("", "stop"), nil
				},
				hasMoreChannels: func() bool { return attempts < 2 },
				nextChannel:     func(context.Context) error { return nil },
			}
			outbound := &terminalObservingOutbound{mockOutbound: baseOutbound}
			middleware := &mockMiddleware{}
			p := NewFactory(executor).Pipeline(
				&mockInbound{},
				outbound,
				WithRetry(1, 0, 0),
				WithMiddlewares(middleware),
			)

			result, err := p.Process(context.Background(), &httpclient.Request{})
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, 2, attempts)
			require.Len(t, outbound.failures, 1, "rejected protocol terminal must notify the attempt observer")
			require.Equal(t, 1, outbound.successes)
			require.Equal(t, 1, middleware.llmResponseCalls,
				"failed protocol terminal must not reach affinity, persistence, or accounting success middlewares")
		})
	}
}

func TestPipeline_StreamIncompleteWithSemanticOutputFailsOverBeforeCommit(t *testing.T) {
	streaming := true
	attempts := 0
	executor := &mockExecutor{doStream: func(context.Context, *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
		attempts++
		return streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte("raw")}}), nil
	}}
	baseOutbound := &mockOutbound{
		transformStream: func(context.Context, *httpclient.Request, streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
			if attempts == 1 {
				failure := semanticTextResponse("incomplete", "length")
				failure.IncompleteReason = "max_output_tokens"
				return streams.SliceStream([]*llm.Response{failure}), nil
			}

			text := "recovered"
			return streams.SliceStream([]*llm.Response{
				{Choices: []llm.Choice{{Delta: &llm.Message{Content: llm.MessageContent{Content: &text}}}}},
				llm.DoneResponse,
			}), nil
		},
		hasMoreChannels: func() bool { return attempts < 2 },
		nextChannel:     func(context.Context) error { return nil },
	}
	outbound := &terminalObservingOutbound{mockOutbound: baseOutbound}
	inbound := &mockInbound{
		transformRequest: func(context.Context, *httpclient.Request) (*llm.Request, error) {
			return &llm.Request{Stream: &streaming}, nil
		},
		transformStream: transformLlmContentToEvents,
	}
	p := NewFactory(executor).Pipeline(inbound, outbound, WithRetry(1, 0, 0))

	result, err := p.Process(context.Background(), &httpclient.Request{})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, 2, attempts)
	require.Len(t, outbound.failures, 1)
	require.Zero(t, outbound.successes, "stream success is finalized only after the returned stream terminates")
	require.Equal(t, []bool{true}, baseOutbound.acceptedChanges,
		"incomplete route must fail before stream commitment; only recovered route is accepted")
}
