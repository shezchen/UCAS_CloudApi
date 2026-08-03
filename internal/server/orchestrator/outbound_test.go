package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// mockTransformer is a simple mock transformer for testing.
type mockTransformer struct {
	aggregatedResponse []byte
	aggregatedMeta     llm.ResponseMeta
	aggregatedErr      error
	apiFormat          llm.APIFormat
}

func (m *mockTransformer) TransformRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	body, err := json.Marshal(map[string]any{
		"model":       req.Model,
		"messages":    req.Messages,
		"temperature": 0.5,
		"max_tokens":  1000,
	})
	if err != nil {
		return nil, err
	}

	return &httpclient.Request{
		Method: "POST",
		URL:    "https://api.example.com/v1/chat/completions",
		Body:   body,
	}, nil
}

func (m *mockTransformer) TransformResponse(ctx context.Context, resp *httpclient.Response) (*llm.Response, error) {
	return &llm.Response{}, nil
}

func (m *mockTransformer) TransformStream(ctx context.Context, req *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	return nil, nil
}

func (m *mockTransformer) TransformError(ctx context.Context, err *httpclient.Error) *llm.ResponseError {
	return nil
}

func (m *mockTransformer) AggregateStreamChunks(ctx context.Context, _ *httpclient.Request, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return m.aggregatedResponse, m.aggregatedMeta, m.aggregatedErr
}

func (m *mockTransformer) APIFormat() llm.APIFormat {
	if m.apiFormat != "" {
		return m.apiFormat
	}

	return llm.APIFormatOpenAIChatCompletion
}

func TestPersistentOutboundTransformer_TransformRequest_OriginalModelRestoration(t *testing.T) {
	tests := []struct {
		name               string
		originalModel      string
		inputModel         string
		actualModel        string
		expectedFinalModel string
	}{
		{
			name:               "no original model - should use candidate ActualModel",
			originalModel:      "",
			inputModel:         "gpt-4",
			actualModel:        "gpt-4",
			expectedFinalModel: "gpt-4",
		},
		{
			name:               "has original model - should use candidate ActualModel (not OriginalModel)",
			originalModel:      "gpt-3.5-turbo",
			inputModel:         "mapped-gpt-4",
			actualModel:        "gpt-4",
			expectedFinalModel: "gpt-4",
		},
		{
			name:               "candidate ActualModel different from input - should use ActualModel",
			originalModel:      "gpt-4",
			inputModel:         "mapped-gpt-4",
			actualModel:        "claude-3-opus",
			expectedFinalModel: "claude-3-opus",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup
			ctx := context.Background()

			channel := &biz.Channel{
				Channel: &ent.Channel{
					ID:              1,
					Name:            "test-channel",
					SupportedModels: []string{"gpt-4", "gpt-3.5-turbo"},
					Settings:        nil,
				},
				Outbound: &mockTransformer{},
			}

			processor := &PersistentOutboundTransformer{
				wrapped: &mockTransformer{},
				state: &PersistenceState{
					OriginalModel:    tt.originalModel,
					CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
					ChannelModelsCandidates: []*ChannelModelsCandidate{
						{Channel: channel, Priority: 0, Models: []biz.ChannelModelEntry{{RequestModel: tt.inputModel, ActualModel: tt.actualModel}}},
					},
					CurrentCandidateIndex: 0,
					RequestExec:           &ent.RequestExecution{ID: 1}, // Dummy to skip creation
				},
			}

			text := "Hello"
			llmRequest := &llm.Request{
				Model: tt.inputModel,
				Messages: []llm.Message{
					{
						Role: "user",
						Content: llm.MessageContent{
							Content: &text,
						},
					},
				},
			}

			// Execute
			channelRequest, err := processor.TransformRequest(ctx, llmRequest)

			// Assert
			require.NoError(t, err)
			require.NotNil(t, channelRequest)

			// Verify model restoration in the request body
			bodyStr := string(channelRequest.Body)
			model := gjson.Get(bodyStr, "model")
			require.Equal(t, tt.expectedFinalModel, model.String())

			// Also verify the llmRequest was modified
			require.Equal(t, tt.expectedFinalModel, llmRequest.Model)
		})
	}
}

func TestPersistentOutboundTransformer_PrepareForRetry(t *testing.T) {
	// Setup
	ctx := context.Background()

	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	t.Run("single model, retry should trigger 'reuse same model' logic", func(t *testing.T) {
		// Case: single model, retry should trigger "reuse same model" logic
		processor := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-4", ActualModel: "gpt-4"},
					},
				},
				CurrentModelIndex: 0,
				RequestExec:       &ent.RequestExecution{ID: 1},
			},
		}

		// Execute PrepareForRetry
		// It should reset RequestExec and do not increase the CurrentModelIndex
		err := processor.PrepareForRetry(ctx)

		// Assert
		require.NoError(t, err)
		require.Zero(t, processor.state.CurrentModelIndex)
		require.Nil(t, processor.state.RequestExec)
	})

	t.Run("multiple models, retry should trigger 'reuse same model' logic", func(t *testing.T) {
		// Case: multiple models, retry should trigger "reuse same model" logic
		processor := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-4", ActualModel: "gpt-4"},
						{RequestModel: "gpt-3.5-turbo", ActualModel: "gpt-3.5-turbo"},
					},
				},
				CurrentModelIndex: 0,
				RequestExec:       &ent.RequestExecution{ID: 1},
			},
		}

		// Execute PrepareForRetry
		// It should reset RequestExec and do increased the CurrentModelIndex
		err := processor.PrepareForRetry(ctx)

		// Assert
		require.NoError(t, err)
		require.Equal(t, 1, processor.state.CurrentModelIndex)
		require.Nil(t, processor.state.RequestExec)
	})
}

func TestPersistentOutboundTransformer_PrepareForRetry_UsesCandidateAPIFormatOutbound(t *testing.T) {
	ctx := context.Background()

	primaryOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIChatCompletion}
	embeddingOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIEmbedding}
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: primaryOutbound,
		Outbounds: map[string]transformer.Outbound{
			llm.APIFormatOpenAIEmbedding.String(): embeddingOutbound,
		},
	}

	processor := &PersistentOutboundTransformer{
		wrapped: primaryOutbound,
		state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{
				Channel:   channel,
				APIFormat: llm.APIFormatOpenAIEmbedding.String(),
				Models: []biz.ChannelModelEntry{
					{RequestModel: "text-embedding-3-small", ActualModel: "text-embedding-3-small"},
					{RequestModel: "text-embedding-3-large", ActualModel: "text-embedding-3-large"},
				},
			},
			CurrentModelIndex: 0,
			RequestExec:       &ent.RequestExecution{ID: 1},
		},
	}

	err := processor.PrepareForRetry(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processor.state.CurrentModelIndex)
	require.Same(t, embeddingOutbound, processor.wrapped)
}

func TestPersistentOutboundTransformer_NextChannel_UsesCandidateAPIFormatOutbound(t *testing.T) {
	ctx := context.Background()

	primaryOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIChatCompletion}
	embeddingOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIEmbedding}
	chatChannel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "chat-channel",
		},
		Outbound: primaryOutbound,
	}
	embeddingChannel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   2,
			Name: "embedding-channel",
		},
		Outbound: primaryOutbound,
		Outbounds: map[string]transformer.Outbound{
			llm.APIFormatOpenAIEmbedding.String(): embeddingOutbound,
		},
	}

	state := &PersistenceState{
		OriginalModel:         "gpt-4o-mini",
		UnifiedRoutes:         biz.NewUnifiedRouteState(),
		AttemptedRoutes:       make(map[biz.RouteKey]struct{}),
		CurrentCandidateIndex: 0,
		CurrentModelIndex:     0,
		ChannelModelsCandidates: []*ChannelModelsCandidate{
			{
				Channel: chatChannel,
				Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4o-mini", ActualModel: "gpt-4o-mini"}},
			},
			{
				Channel:   embeddingChannel,
				APIFormat: llm.APIFormatOpenAIEmbedding.String(),
				Models:    []biz.ChannelModelEntry{{RequestModel: "text-embedding-3-small", ActualModel: "text-embedding-3-small"}},
			},
		},
	}
	state.CurrentCandidate = state.ChannelModelsCandidates[0]
	processor := &PersistentOutboundTransformer{
		wrapped: primaryOutbound,
		state:   state,
	}
	processor.captureCurrentRoute(ctx)

	err := processor.NextChannel(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, processor.state.CurrentCandidateIndex)
	require.Same(t, embeddingChannel, processor.state.CurrentCandidate.Channel)
	require.Same(t, embeddingOutbound, processor.wrapped)
}

func TestSelectOutboundForCandidate(t *testing.T) {
	primaryOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIChatCompletion}
	embeddingOutbound := &mockTransformer{apiFormat: llm.APIFormatOpenAIEmbedding}

	t.Run("nil candidate returns nil", func(t *testing.T) {
		require.Nil(t, selectOutboundForCandidate(nil))
	})

	t.Run("candidate with nil channel returns nil", func(t *testing.T) {
		candidate := &ChannelModelsCandidate{APIFormat: llm.APIFormatOpenAIEmbedding.String()}
		require.Nil(t, selectOutboundForCandidate(candidate))
	})

	t.Run("api format set and found in outbounds returns matching outbound", func(t *testing.T) {
		channel := &biz.Channel{
			Channel:   &ent.Channel{ID: 1, Name: "test"},
			Outbound:  primaryOutbound,
			Outbounds: map[string]transformer.Outbound{llm.APIFormatOpenAIEmbedding.String(): embeddingOutbound},
		}
		candidate := &ChannelModelsCandidate{
			Channel:   channel,
			APIFormat: llm.APIFormatOpenAIEmbedding.String(),
		}
		require.Same(t, embeddingOutbound, selectOutboundForCandidate(candidate))
	})

	t.Run("api format set but not in outbounds falls back to channel outbound", func(t *testing.T) {
		channel := &biz.Channel{
			Channel:   &ent.Channel{ID: 1, Name: "test"},
			Outbound:  primaryOutbound,
			Outbounds: map[string]transformer.Outbound{},
		}
		candidate := &ChannelModelsCandidate{
			Channel:   channel,
			APIFormat: llm.APIFormatOpenAIEmbedding.String(),
		}
		require.Same(t, primaryOutbound, selectOutboundForCandidate(candidate))
	})

	t.Run("nil outbounds falls back to channel outbound", func(t *testing.T) {
		channel := &biz.Channel{
			Channel:  &ent.Channel{ID: 1, Name: "test"},
			Outbound: primaryOutbound,
		}
		candidate := &ChannelModelsCandidate{
			Channel:   channel,
			APIFormat: llm.APIFormatOpenAIEmbedding.String(),
		}
		require.Same(t, primaryOutbound, selectOutboundForCandidate(candidate))
	})

	t.Run("empty api format falls back to channel outbound", func(t *testing.T) {
		channel := &biz.Channel{
			Channel:   &ent.Channel{ID: 1, Name: "test"},
			Outbound:  primaryOutbound,
			Outbounds: map[string]transformer.Outbound{llm.APIFormatOpenAIEmbedding.String(): embeddingOutbound},
		}
		candidate := &ChannelModelsCandidate{
			Channel:   channel,
			APIFormat: "",
		}
		require.Same(t, primaryOutbound, selectOutboundForCandidate(candidate))
	})
}

func TestPersistentOutboundTransformer_TransformRequest_ResetsStreamCompletedForNewAttempt(t *testing.T) {
	ctx := context.Background()

	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:              1,
			Name:            "test-channel",
			SupportedModels: []string{"gpt-4"},
		},
		Outbound: &mockTransformer{},
	}

	processor := &PersistentOutboundTransformer{
		wrapped: &mockTransformer{},
		state: &PersistenceState{
			StreamCompleted: true,
			ChannelModelsCandidates: []*ChannelModelsCandidate{
				{Channel: channel, Priority: 0, Models: []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}}},
			},
			CurrentCandidateIndex: 0,
			RequestExec:           &ent.RequestExecution{ID: 1},
		},
	}

	text := "Hello"
	llmRequest := &llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{{
			Role: "user",
			Content: llm.MessageContent{
				Content: &text,
			},
		}},
	}

	_, err := processor.TransformRequest(ctx, llmRequest)
	require.NoError(t, err)
	require.False(t, processor.state.StreamCompleted)
}

func TestPersistentOutboundTransformer_CanRetry(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	retryableErr := &httpclient.Error{StatusCode: http.StatusTooManyRequests}
	nonRetryableErr := &httpclient.Error{StatusCode: http.StatusBadRequest}

	t.Run("no current candidate", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: nil,
			},
		}

		require.False(t, outbound.CanRetry(retryableErr))
	})

	t.Run("nil error", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
			},
		}

		require.False(t, outbound.CanRetry(nil))
	})

	t.Run("non-retryable error", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
			},
		}

		require.False(t, outbound.CanRetry(nonRetryableErr))
	})

	t.Run("skip-by-circuit-breaker should not trigger same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-4", ActualModel: "gpt-4"},
						{RequestModel: "gpt-3.5-turbo", ActualModel: "gpt-3.5-turbo"},
					},
				},
				CurrentModelIndex: 0,
			},
		}

		require.False(t, outbound.CanRetry(errSkipCandidateByCircuitBreaker))
	})

	t.Run("all failures are handled by exact-route rescue, not same-route retry", func(t *testing.T) {
		for _, retryErr := range []error{
			fmt.Errorf("failed to auto-aggregate streaming response: %w", pipeline.ErrEmptyResponse),
			fmt.Errorf("failed to auto-aggregate streaming response: %w", pipeline.ErrEmptyStreamChunks),
			fmt.Errorf("failed to auto-aggregate streaming response: %w", pipeline.ErrEmptyAggregatedBody),
			fmt.Errorf("upstream stream failed: %w", pipeline.ErrStreamIncomplete),
			fmt.Errorf("upstream stream failed: %w", llm.ErrStreamIncomplete),
		} {
			outbound := &PersistentOutboundTransformer{
				wrapped: &mockTransformer{},
				state: &PersistenceState{
					CurrentCandidate: &ChannelModelsCandidate{
						Channel: channel,
						Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
					},
					CurrentModelIndex: 0,
				},
			}

			require.False(t, outbound.CanRetry(retryErr), "same-route retry must stay disabled: %v", retryErr)
		}
	})

	t.Run("unsupported model skips same actual model and allows different model", func(t *testing.T) {
		unsupportedErr := &httpclient.Error{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(`The requested model is not supported.`),
		}

		sameOnly := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-5.6-sol", ActualModel: "gpt-5.6-sol"},
						{RequestModel: "gpt-5.6-sol", ActualModel: "gpt-5.6-sol"},
					},
				},
				CurrentModelIndex: 0,
			},
		}
		require.False(t, sameOnly.CanRetry(unsupportedErr))

		withFallback := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-5.6-sol", ActualModel: "gpt-5.6-sol"},
						{RequestModel: "gpt-5.6-sol", ActualModel: "gpt-5.6-sol-fallback"},
					},
				},
				CurrentModelIndex: 0,
			},
		}
		require.False(t, withFallback.CanRetry(unsupportedErr), "alternate actual models are exact rescue routes, not same-route retries")
	})
}

func TestOutboundPersistentStream_NotifiesOnlyFinalSemanticOutcome(t *testing.T) {
	tests := []struct {
		name     string
		events   []*httpclient.StreamEvent
		meta     llm.ResponseMeta
		expected bool
	}{
		{
			name: "terminal stream succeeds",
			events: []*httpclient.StreamEvent{
				{Data: []byte(`{"choices":[{"delta":{"content":"ok"}}]}`)},
				{Data: []byte(`[DONE]`)},
			},
			meta:     llm.ResponseMeta{Terminal: true, Completed: true},
			expected: true,
		},
		{
			name:     "semantic content without terminal fails",
			events:   []*httpclient.StreamEvent{{Data: []byte(`{"choices":[{"delta":{"content":"partial"}}]}`)}},
			meta:     llm.ResponseMeta{},
			expected: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &PersistenceState{AttemptAccepted: true, AttemptSemanticOutput: true}
			var outcomes []bool
			persistent := NewOutboundPersistentStream(
				context.Background(),
				streams.SliceStream(test.events),
				nil,
				nil,
				nil,
				nil,
				&mockTransformer{aggregatedResponse: []byte(`{}`), aggregatedMeta: test.meta},
				nil,
				state,
				func(_ context.Context, success bool) { outcomes = append(outcomes, success) },
			)
			for persistent.Next() {
				_ = persistent.Current()
			}
			require.NoError(t, persistent.Close())
			require.Equal(t, []bool{test.expected}, outcomes, "attempt observer must fire exactly once at Close")
		})
	}
}

func TestShouldForceStreamingForCandidate(t *testing.T) {
	newCandidate := func(policy objects.CapabilityPolicy, apiFormat llm.APIFormat) *ChannelModelsCandidate {
		return &ChannelModelsCandidate{
			APIFormat: apiFormat.String(),
			Channel: &biz.Channel{
				Channel: &ent.Channel{
					Policies: objects.ChannelPolicies{Stream: policy},
				},
			},
		}
	}

	t.Run("supported require-stream fallback request forces streaming", func(t *testing.T) {
		require.True(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyRequire, llm.APIFormatOpenAIChatCompletion),
			&llm.Request{RequestType: llm.RequestTypeChat, APIFormat: llm.APIFormatOpenAIChatCompletion},
		))
	})

	t.Run("native non-stream candidate does not force streaming", func(t *testing.T) {
		require.False(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyUnlimited, llm.APIFormatOpenAIChatCompletion),
			&llm.Request{RequestType: llm.RequestTypeChat, APIFormat: llm.APIFormatOpenAIChatCompletion},
		))
	})

	t.Run("unsupported embedding request does not force streaming", func(t *testing.T) {
		require.False(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyRequire, llm.APIFormatOpenAIEmbedding),
			&llm.Request{RequestType: llm.RequestTypeEmbedding, APIFormat: llm.APIFormatOpenAIEmbedding},
		))
	})

	t.Run("unsupported compact request does not force streaming", func(t *testing.T) {
		require.False(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyRequire, llm.APIFormatOpenAIResponseCompact),
			&llm.Request{RequestType: llm.RequestTypeCompact, APIFormat: llm.APIFormatOpenAIResponseCompact},
		))
	})

	t.Run("client requested stream keeps existing streaming path", func(t *testing.T) {
		require.False(t, shouldForceStreamingForCandidate(
			newCandidate(objects.CapabilityPolicyRequire, llm.APIFormatOpenAIChatCompletion),
			&llm.Request{Stream: lo.ToPtr(true), RequestType: llm.RequestTypeChat, APIFormat: llm.APIFormatOpenAIChatCompletion},
		))
	})
}

func TestIsTerminalAggregatedOutboundResponse(t *testing.T) {
	t.Run("usage with completion tokens is not a terminal signal", func(t *testing.T) {
		require.False(t, isTerminalAggregated(llm.ResponseMeta{Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}))
	})

	t.Run("usage with zero completion tokens is not completed", func(t *testing.T) {
		require.False(t, isTerminalAggregated(llm.ResponseMeta{Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 0, TotalTokens: 10}}))
	})

	t.Run("response id without usage is not completed", func(t *testing.T) {
		require.False(t, isTerminalAggregated(llm.ResponseMeta{ID: "resp_123"}))
	})

	t.Run("explicit completed flag is completed", func(t *testing.T) {
		require.True(t, isTerminalAggregated(llm.ResponseMeta{ID: llm.SpeechStreamResponseID, Completed: true}))
	})

	t.Run("incomplete response is terminal without being completed", func(t *testing.T) {
		require.True(t, isTerminalAggregated(llm.ResponseMeta{ID: "resp_incomplete", Terminal: true}))
	})

	t.Run("speech stream aggregate id alone is not completed", func(t *testing.T) {
		require.False(t, isTerminalAggregated(llm.ResponseMeta{ID: llm.SpeechStreamResponseID}))
	})

	t.Run("missing usage and id is not completed", func(t *testing.T) {
		require.False(t, isTerminalAggregated(llm.ResponseMeta{}))
	})
}

func TestIsSuccessfulOutboundTerminalStreamEvent(t *testing.T) {
	require.True(t, isSuccessfulOutboundTerminalStreamEvent(
		llm.APIFormatOpenAIChatCompletion,
		&httpclient.StreamEvent{Data: []byte("  [DONE]\n")},
	))
	require.False(t, isSuccessfulOutboundTerminalStreamEvent(
		llm.APIFormatOpenAIResponse,
		&httpclient.StreamEvent{Data: []byte("[DONE]")},
	))
	require.False(t, isSuccessfulOutboundTerminalStreamEvent(
		llm.APIFormatOpenAIResponse,
		&httpclient.StreamEvent{Type: "response.completed"},
	))
	require.True(t, isSuccessfulOutboundTerminalStreamEvent(
		llm.APIFormatOpenAIResponse,
		&httpclient.StreamEvent{
			Type: "response.completed",
			Data: []byte(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed"}}`),
		},
	))
	require.False(t, isSuccessfulOutboundTerminalStreamEvent(
		llm.APIFormatOpenAIResponse,
		&httpclient.StreamEvent{
			Type: "response.completed",
			Data: []byte(`{"type":"response.completed","response":{"id":"resp_partial","status":"incomplete"}}`),
		},
	))
}

type sliceEventStream struct {
	events []*httpclient.StreamEvent
	index  int
	err    error
	closed bool
}

func (s *sliceEventStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}

	s.index++
	return true
}

func (s *sliceEventStream) Current() *httpclient.StreamEvent {
	if s.index == 0 || s.index > len(s.events) {
		return nil
	}

	return s.events[s.index-1]
}

func (s *sliceEventStream) Err() error {
	return s.err
}

func (s *sliceEventStream) Close() error {
	s.closed = true
	return nil
}

func TestOutboundPersistentStream_Close_AggregatedResponsesCompletionHandling(t *testing.T) {
	ctx := context.Background()
	ctx = authz.WithTestBypass(ctx)

	t.Run("response in_progress without terminal event is not completed", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		ctx := ent.NewContext(ctx, client)
		project := createTestProject(t, ctx, client)
		ch := createTestChannel(t, ctx, client)
		_, requestService, _, usageLogService := setupTestServices(t, client)

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(ctx)
		require.NoError(t, err)

		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusPending).
			SetStream(true).
			Save(ctx)
		require.NoError(t, err)

		stream := &sliceEventStream{
			events: []*httpclient.StreamEvent{{Type: "response.in_progress", Data: []byte(`{"type":"response.in_progress"}`)}},
		}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: []byte(`{"id":"resp_123","status":"in_progress"}`),
		}
		state := &PersistenceState{}

		persistentStream := NewOutboundPersistentStream(ctx, stream, req, exec, requestService, usageLogService, transformer, nil, state)
		for persistentStream.Next() {
			_ = persistentStream.Current()
		}
		require.NoError(t, persistentStream.Close())

		dbExec, err := client.RequestExecution.Get(ctx, exec.ID)
		require.NoError(t, err)
		require.NotEqual(t, requestexecution.StatusCompleted, dbExec.Status)
		require.Equal(t, requestexecution.StatusFailed, dbExec.Status)
		require.Contains(t, dbExec.ErrorMessage, "empty response detected")
		require.False(t, state.OutboundStreamCompleted)
	})

	t.Run("response incomplete is terminal but execution remains failed", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		ctx := ent.NewContext(ctx, client)
		project := createTestProject(t, ctx, client)
		ch := createTestChannel(t, ctx, client)
		_, requestService, _, usageLogService := setupTestServices(t, client)

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-5.6-sol").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(ctx)
		require.NoError(t, err)

		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-5.6-sol").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusPending).
			SetStream(true).
			Save(ctx)
		require.NoError(t, err)

		stream := &sliceEventStream{events: []*httpclient.StreamEvent{
			{
				Type: "response.output_text.delta",
				Data: []byte(`{"type":"response.output_text.delta","delta":"partial"}`),
			},
			{
				Type: "response.incomplete",
				Data: []byte(`{"type":"response.incomplete","response":{"id":"resp_partial","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`),
			},
		}}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: []byte(`{"id":"resp_partial","status":"incomplete"}`),
			aggregatedMeta: llm.ResponseMeta{
				ID:               "resp_partial",
				Terminal:         true,
				Completed:        false,
				ProtocolStatus:   "incomplete",
				IncompleteReason: "max_output_tokens",
				Usage: &llm.Usage{
					PromptTokens:     10,
					CompletionTokens: 2,
					TotalTokens:      12,
				},
			},
		}
		state := &PersistenceState{AttemptAccepted: true, AttemptSemanticOutput: true}
		persistent := NewOutboundPersistentStream(ctx, stream, req, exec, requestService, usageLogService, transformer, nil, state)
		for persistent.Next() {
			_ = persistent.Current()
		}
		require.NoError(t, persistent.Close())

		stored, err := client.RequestExecution.Get(ctx, exec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, stored.Status)
		require.Contains(t, stored.ErrorMessage, "upstream response incomplete: max_output_tokens")
		require.Contains(t, stored.ErrorMessage, "response_incomplete")
		require.False(t, state.OutboundStreamCompleted)
		usageCount, err := client.UsageLog.Query().Count(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, usageCount, "accepted partial output must still settle its real upstream usage")
	})

	t.Run("responses transport done without protocol terminal remains failed", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		ctx := ent.NewContext(ctx, client)
		project := createTestProject(t, ctx, client)
		ch := createTestChannel(t, ctx, client)
		_, requestService, _, usageLogService := setupTestServices(t, client)

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-5.6-sol").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(ctx)
		require.NoError(t, err)
		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-5.6-sol").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusPending).
			SetStream(true).
			Save(ctx)
		require.NoError(t, err)

		stream := &sliceEventStream{events: []*httpclient.StreamEvent{
			{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_cut","status":"in_progress"}}`)},
			{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"partial"}`)},
			{Data: []byte("[DONE]")},
		}}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: []byte(`{"id":"resp_cut","status":"in_progress"}`),
			aggregatedMeta: llm.ResponseMeta{
				ID:             "resp_cut",
				ProtocolStatus: "in_progress",
				Usage: &llm.Usage{
					PromptTokens:     20,
					CompletionTokens: 1,
					TotalTokens:      21,
				},
			},
		}
		state := &PersistenceState{AttemptAccepted: true, AttemptSemanticOutput: true}
		persistent := NewOutboundPersistentStream(ctx, stream, req, exec, requestService, usageLogService, transformer, nil, state)
		for persistent.Next() {
			_ = persistent.Current()
		}
		require.NoError(t, persistent.Close())

		stored, err := client.RequestExecution.Get(ctx, exec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, stored.Status)
		require.Contains(t, stored.ErrorMessage, llm.ErrStreamIncomplete.Error())
		require.False(t, state.OutboundStreamCompleted)
		usageCount, err := client.UsageLog.Query().Count(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, usageCount, "already-delivered partial output remains metered")
	})

	t.Run("terminal and usage without semantic output do not settle", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		ctx := ent.NewContext(ctx, client)
		project := createTestProject(t, ctx, client)
		ch := createTestChannel(t, ctx, client)
		_, requestService, _, usageLogService := setupTestServices(t, client)

		req, err := client.Request.Create().SetProjectID(project.ID).SetChannelID(ch.ID).
			SetModelID("gpt-4.1").SetStatus(request.StatusPending).SetRequestBody([]byte(`{"stream":true}`)).Save(ctx)
		require.NoError(t, err)
		exec, err := client.RequestExecution.Create().SetRequestID(req.ID).SetProjectID(project.ID).
			SetChannelID(ch.ID).SetModelID("gpt-4.1").SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/chat_completions").SetStatus(requestexecution.StatusPending).SetStream(true).Save(ctx)
		require.NoError(t, err)

		stream := &sliceEventStream{events: []*httpclient.StreamEvent{{Data: []byte("[DONE]")}}}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIChatCompletion,
			aggregatedResponse: []byte(`{"id":"chat_empty","choices":[]}`),
			aggregatedMeta: llm.ResponseMeta{
				ID:        "chat_empty",
				Completed: true,
				Usage:     &llm.Usage{PromptTokens: 10, TotalTokens: 10},
			},
		}
		state := &PersistenceState{AttemptAccepted: true}
		persistent := NewOutboundPersistentStream(ctx, stream, req, exec, requestService, usageLogService, transformer, nil, state)
		for persistent.Next() {
			_ = persistent.Current()
		}
		require.NoError(t, persistent.Close())

		stored, err := client.RequestExecution.Get(ctx, exec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, stored.Status)
		require.Contains(t, stored.ErrorMessage, pipeline.ErrEmptyResponse.Error())
		usageCount, err := client.UsageLog.Query().Count(ctx)
		require.NoError(t, err)
		require.Zero(t, usageCount, "empty pre-commit attempt must leave usage settlement to failover")
	})

	t.Run("aggregated completed response without terminal event is completed", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		ctx := ent.NewContext(ctx, client)
		project := createTestProject(t, ctx, client)
		ch := createTestChannel(t, ctx, client)
		_, requestService, _, usageLogService := setupTestServices(t, client)

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(ctx)
		require.NoError(t, err)

		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusPending).
			SetStream(true).
			Save(ctx)
		require.NoError(t, err)

		aggregated := []byte(`{"id":"resp_456","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
		stream := &sliceEventStream{
			events: []*httpclient.StreamEvent{{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"hi"}`)}},
		}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: aggregated,
			aggregatedMeta: llm.ResponseMeta{
				ID:        "resp_456",
				Terminal:  true,
				Completed: true,
				Usage: &llm.Usage{
					PromptTokens:     10,
					CompletionTokens: 2,
					TotalTokens:      12,
				},
			},
		}
		state := &PersistenceState{AttemptAccepted: true, AttemptSemanticOutput: true}

		persistentStream := NewOutboundPersistentStream(ctx, stream, req, exec, requestService, usageLogService, transformer, nil, state)
		for persistentStream.Next() {
			_ = persistentStream.Current()
		}
		require.NoError(t, persistentStream.Close())

		dbExec, err := client.RequestExecution.Get(ctx, exec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusCompleted, dbExec.Status)
		require.JSONEq(t, string(aggregated), string(dbExec.ResponseBody))
		require.Equal(t, "resp_456", dbExec.ExternalID)
		require.True(t, state.OutboundStreamCompleted)
	})

	t.Run("canceled client with aggregated completed response is still completed", func(t *testing.T) {
		client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
		defer client.Close()

		baseCtx := ent.NewContext(ctx, client)
		project := createTestProject(t, baseCtx, client)
		ch := createTestChannel(t, baseCtx, client)
		_, requestService, _, usageLogService := setupTestServices(t, client)

		req, err := client.Request.Create().
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetStatus(request.StatusPending).
			SetRequestBody([]byte(`{"stream":true}`)).
			Save(baseCtx)
		require.NoError(t, err)

		exec, err := client.RequestExecution.Create().
			SetRequestID(req.ID).
			SetProjectID(project.ID).
			SetChannelID(ch.ID).
			SetModelID("gpt-4.1").
			SetRequestBody([]byte(`{"stream":true}`)).
			SetFormat("openai/responses").
			SetStatus(requestexecution.StatusPending).
			SetStream(true).
			Save(baseCtx)
		require.NoError(t, err)

		aggregated := []byte(`{"id":"resp_codex_like","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
		stream := &sliceEventStream{
			events: []*httpclient.StreamEvent{{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"done"}`)}},
			err:    context.Canceled,
		}
		transformer := &mockTransformer{
			apiFormat:          llm.APIFormatOpenAIResponse,
			aggregatedResponse: aggregated,
			aggregatedMeta: llm.ResponseMeta{
				ID:        "resp_codex_like",
				Terminal:  true,
				Completed: true,
				Usage: &llm.Usage{
					PromptTokens:     20,
					CompletionTokens: 1,
					TotalTokens:      21,
				},
			},
		}
		state := &PersistenceState{AttemptAccepted: true, AttemptSemanticOutput: true}

		requestCtx, cancel := context.WithCancel(baseCtx)
		cancel()

		persistentStream := NewOutboundPersistentStream(requestCtx, stream, req, exec, requestService, usageLogService, transformer, nil, state)
		for persistentStream.Next() {
			_ = persistentStream.Current()
		}
		require.NoError(t, persistentStream.Close())

		dbExec, err := client.RequestExecution.Get(baseCtx, exec.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusCompleted, dbExec.Status)
		require.JSONEq(t, string(aggregated), string(dbExec.ResponseBody))
		require.Equal(t, "resp_codex_like", dbExec.ExternalID)
		require.True(t, state.OutboundStreamCompleted)
	})
}

func TestPersistRequestExecutionMiddleware_IncompleteProtocolResponseRemainsFailed(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)
	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	_, requestService, _, _ := setupTestServices(t, client)

	req, err := client.Request.Create().
		SetProjectID(project.ID).
		SetChannelID(channel.ID).
		SetModelID("gpt-5.6-sol").
		SetStatus(request.StatusPending).
		SetRequestBody([]byte(`{"stream":false}`)).
		Save(ctx)
	require.NoError(t, err)
	exec, err := client.RequestExecution.Create().
		SetRequestID(req.ID).
		SetProjectID(project.ID).
		SetChannelID(channel.ID).
		SetModelID("gpt-5.6-sol").
		SetRequestBody([]byte(`{"stream":false}`)).
		SetFormat("openai/responses").
		SetStatus(requestexecution.StatusPending).
		SetStream(false).
		Save(ctx)
	require.NoError(t, err)

	state := &PersistenceState{Request: req, RequestExec: exec, RequestService: requestService}
	middleware := &persistRequestExecutionMiddleware{
		outbound:    &PersistentOutboundTransformer{state: state},
		rawResponse: &httpclient.Response{StatusCode: 200, Body: []byte(`{"id":"resp_partial","status":"incomplete"}`)},
	}
	_, err = middleware.OnOutboundLlmResponse(ctx, &llm.Response{
		ID:               "resp_partial",
		ProtocolStatus:   "incomplete",
		IncompleteReason: "max_output_tokens",
	})
	require.NoError(t, err)

	stored, err := client.RequestExecution.Get(ctx, exec.ID)
	require.NoError(t, err)
	require.Equal(t, requestexecution.StatusFailed, stored.Status)
	require.Contains(t, stored.ErrorMessage, "upstream response incomplete: max_output_tokens")
}

func TestPersistentOutboundTransformer_TransformRequest_WithPrepopulatedState(t *testing.T) {
	// Setup
	ctx := context.Background()

	// Pre-populate channels (now done by inbound transformer)
	testChannel := &biz.Channel{
		Channel: &ent.Channel{
			ID:              1,
			Name:            "test-channel",
			SupportedModels: []string{"gpt-4", "gpt-3.5-turbo"}, // Add gpt-3.5-turbo
			Settings:        nil,
		},
		Outbound: &mockTransformer{},
	}

	processor := &PersistentOutboundTransformer{
		wrapped: &mockTransformer{},
		state: &PersistenceState{
			OriginalModel: "gpt-3.5-turbo",
			ChannelModelsCandidates: []*ChannelModelsCandidate{
				{Channel: testChannel, Priority: 0, Models: []biz.ChannelModelEntry{{RequestModel: "gpt-3.5-turbo", ActualModel: "gpt-3.5-turbo"}}},
			}, // Pre-populated by inbound
			CurrentCandidateIndex: 0,
			RequestExec:           &ent.RequestExecution{ID: 1}, // Dummy to skip creation
		},
	}

	text := "Hello"
	llmRequest := &llm.Request{
		Model: "mapped-gpt-4", // This was mapped by inbound transformer
		Messages: []llm.Message{
			{
				Role: "user",
				Content: llm.MessageContent{
					Content: &text,
				},
			},
		},
	}

	// Execute
	channelRequest, err := processor.TransformRequest(ctx, llmRequest)

	// Assert
	require.NoError(t, err)
	require.NotNil(t, channelRequest)

	// Verify original model was restored
	require.Equal(t, "gpt-3.5-turbo", llmRequest.Model)

	// Verify channel was used
	require.Equal(t, testChannel, processor.state.CurrentCandidate.Channel)
}

func TestFilterResponseCustomToolMessagesForNonResponsesOutbound(t *testing.T) {
	baseRequest := &llm.Request{
		APIFormat: llm.APIFormatOpenAIResponse,
		Messages: []llm.Message{
			{
				Role: "assistant",
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call_custom_1",
						Type: llm.ToolTypeResponsesCustomTool,
						ResponseCustomToolCall: &llm.ResponseCustomToolCall{
							CallID: "call_custom_1",
							Name:   "apply_patch",
							Input:  "*** Begin Patch\n*** End Patch\n",
						},
					},
					{
						ID:   "call_function_1",
						Type: llm.ToolTypeFunction,
						Function: llm.FunctionCall{
							Name:      "get_weather",
							Arguments: "{}",
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: func() *string { v := "call_custom_1"; return &v }(),
				Content: llm.MessageContent{
					Content: func() *string { v := "custom"; return &v }(),
				},
			},
			{
				Role:       "tool",
				ToolCallID: func() *string { v := "call_function_1"; return &v }(),
				Content: llm.MessageContent{
					Content: func() *string { v := "function"; return &v }(),
				},
			},
		},
	}

	t.Run("filters when inbound is responses and outbound is not", func(t *testing.T) {
		got := filterResponseCustomToolMessagesForNonResponsesOutbound(baseRequest, llm.APIFormatOpenAIChatCompletion)
		require.NotSame(t, baseRequest, got)
		require.Len(t, got.Messages, 2)
		require.Len(t, got.Messages[0].ToolCalls, 1)
		require.Equal(t, llm.ToolTypeFunction, got.Messages[0].ToolCalls[0].Type)
		require.NotNil(t, got.Messages[1].ToolCallID)
		require.Equal(t, "call_function_1", *got.Messages[1].ToolCallID)
	})

	t.Run("does not filter when outbound is responses", func(t *testing.T) {
		got := filterResponseCustomToolMessagesForNonResponsesOutbound(baseRequest, llm.APIFormatOpenAIResponse)
		require.Same(t, baseRequest, got)
	})

	t.Run("does not filter when inbound is not responses", func(t *testing.T) {
		nonResponsesReq := *baseRequest
		nonResponsesReq.APIFormat = llm.APIFormatOpenAIChatCompletion
		got := filterResponseCustomToolMessagesForNonResponsesOutbound(&nonResponsesReq, llm.APIFormatOpenAIChatCompletion)
		require.Same(t, &nonResponsesReq, got)
	})
}

// ========== 429 Retry-After Tests ==========

func TestPersistentOutboundTransformer_CanRetry_429_WithRetryAfter(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	t.Run("429 with Retry-After should not retry same channel", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with Retry-After header
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{"30"}},
		}

		require.False(t, outbound.CanRetry(httpErr))
	})

	t.Run("429 with multiple headers including Retry-After should not retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with multiple headers
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers: http.Header{
				"Retry-After":  []string{"60"},
				"Content-Type": []string{"application/json"},
			},
		}

		require.False(t, outbound.CanRetry(httpErr))
	})
}

func TestPersistentOutboundTransformer_CanRetry_429_WithoutRetryAfter(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	t.Run("429 without Retry-After (nil headers) should skip same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error without headers
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers:    nil,
		}

		require.False(t, outbound.CanRetry(httpErr))
	})

	t.Run("429 without Retry-After (empty headers) should skip same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with empty headers
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{},
		}

		require.False(t, outbound.CanRetry(httpErr))
	})

	t.Run("429 without Retry-After (headers but no Retry-After key) should skip same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with headers but no Retry-After
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers: http.Header{
				"Content-Type": []string{"application/json"},
			},
		}

		require.False(t, outbound.CanRetry(httpErr))
	})

	t.Run("transformed 429 response error should skip same-channel retry", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
				},
				CurrentModelIndex: 0,
			},
		}

		responseErr := &llm.ResponseError{StatusCode: http.StatusTooManyRequests}

		require.False(t, outbound.CanRetry(responseErr))
	})
}

func TestPersistentOutboundTransformer_CanRetry_ChannelRetryableStatusCodes(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
			Settings: &objects.ChannelSettings{
				RetryableStatusCodes: []int{http.StatusBadRequest, http.StatusForbidden},
			},
		},
		Outbound: &mockTransformer{},
	}

	outbound := &PersistentOutboundTransformer{
		wrapped: &mockTransformer{},
		state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{
				Channel: channel,
				Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
			},
			CurrentModelIndex: 0,
		},
	}

	// Per-channel status lists no longer form a second retry policy. Every
	// abnormal attempt is handed to the unified cross-route rescue queue.
	require.False(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusBadRequest}))
	require.False(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusForbidden}))
	require.False(t, outbound.CanRetry(&httpclient.Error{StatusCode: http.StatusUnauthorized}))
}

func TestPersistentOutboundTransformer_CanRetry_ChannelGlobalErrorsSkipModelFallback(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "shared-codex-channel",
			Settings: &objects.ChannelSettings{
				RetryableStatusCodes: []int{
					http.StatusUnauthorized,
					http.StatusPaymentRequired,
					http.StatusForbidden,
					http.StatusTooManyRequests,
				},
			},
		},
		Outbound: &mockTransformer{},
	}

	tests := []struct {
		name string
		err  error
	}{
		{name: "http 401", err: &httpclient.Error{StatusCode: http.StatusUnauthorized}},
		{name: "http 402", err: &httpclient.Error{StatusCode: http.StatusPaymentRequired}},
		{name: "http 403", err: &httpclient.Error{StatusCode: http.StatusForbidden}},
		{name: "http 429", err: &httpclient.Error{StatusCode: http.StatusTooManyRequests}},
		{name: "transformed 401", err: &llm.ResponseError{StatusCode: http.StatusUnauthorized}},
		{name: "transformed 402", err: &llm.ResponseError{StatusCode: http.StatusPaymentRequired}},
		{name: "transformed 403", err: &llm.ResponseError{StatusCode: http.StatusForbidden}},
		{name: "transformed 429", err: &llm.ResponseError{StatusCode: http.StatusTooManyRequests}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outbound := &PersistentOutboundTransformer{
				wrapped: &mockTransformer{},
				state: &PersistenceState{
					CurrentCandidate: &ChannelModelsCandidate{
						Channel: channel,
						Models: []biz.ChannelModelEntry{
							{RequestModel: "gpt-5.6-sol", ActualModel: "gpt-5.6-sol"},
							{RequestModel: "gpt-5.6-sol", ActualModel: "gpt-5.6-sol-fallback"},
						},
					},
					CurrentModelIndex: 0,
				},
			}

			require.False(t, outbound.CanRetry(tt.err))
			require.Zero(t, outbound.state.CurrentModelIndex)
		})
	}
}

func TestPersistentOutboundTransformer_CanRetry_429_WithMultipleModels(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			ID:   1,
			Name: "test-channel",
		},
		Outbound: &mockTransformer{},
	}

	t.Run("429 with Retry-After should not retry even with multiple models", func(t *testing.T) {
		outbound := &PersistentOutboundTransformer{
			wrapped: &mockTransformer{},
			state: &PersistenceState{
				CurrentCandidate: &ChannelModelsCandidate{
					Channel: channel,
					Models: []biz.ChannelModelEntry{
						{RequestModel: "gpt-4", ActualModel: "gpt-4"},
						{RequestModel: "gpt-3.5-turbo", ActualModel: "gpt-3.5-turbo"},
					},
				},
				CurrentModelIndex: 0,
			},
		}

		// 429 error with Retry-After header
		httpErr := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Headers:    http.Header{"Retry-After": []string{"30"}},
		}

		// Should skip retry even though there are more models
		require.False(t, outbound.CanRetry(httpErr))
	})
}
