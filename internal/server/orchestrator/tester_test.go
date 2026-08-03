package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func TestChannelHTTPErrorDoesNotInventUpstreamStatus(t *testing.T) {
	synthetic := &httpclient.Error{
		StatusCode: http.StatusInternalServerError,
		Body:       []byte(`{"error":{"message":"tls: failed to verify certificate"}}`),
	}

	status, message := testChannelHTTPError(synthetic, 0, context.DeadlineExceeded)
	require.Nil(t, status)
	require.Equal(t, "tls: failed to verify certificate", message)

	status, message = testChannelHTTPError(&httpclient.Error{
		StatusCode: http.StatusBadRequest,
		Body:       []byte(`{"detail":"The 'gpt-5' model is not supported."}`),
	}, http.StatusBadRequest, nil)
	require.Equal(t, http.StatusBadRequest, *status)
	require.Equal(t, "The 'gpt-5' model is not supported.", message)

	status, message = testChannelHTTPError(&httpclient.Error{
		StatusCode: http.StatusBadRequest,
		Body:       []byte(`{"errors":[{"message":"Requested model is unavailable"}]}`),
	}, http.StatusBadRequest, nil)
	require.Equal(t, http.StatusBadRequest, *status)
	require.Equal(t, "Requested model is unavailable", message)

	status, message = testChannelHTTPError(&httpclient.Error{
		StatusCode: http.StatusNotFound,
		Body:       []byte(`{"error":{"message":"Request rejected","code":"model_not_found","type":"invalid_request_error"}}`),
	}, http.StatusNotFound, nil)
	require.Equal(t, http.StatusNotFound, *status)
	require.Equal(t, "Request rejected (code: model_not_found, type: invalid_request_error)", message)
}

func TestChannelHTTPErrorNeverReturnsStructuredSecretsOrRawBody(t *testing.T) {
	status, message := testChannelHTTPError(&httpclient.Error{
		StatusCode: http.StatusBadRequest,
		Body: []byte(`{
			"error": {
				"access_token": "access-value",
				"client_secret": "client-value",
				"headers": {"Authorization": "Bearer auth-value"}
			}
		}`),
	}, http.StatusBadRequest, nil)

	require.Equal(t, http.StatusBadRequest, *status)
	require.Equal(t, "Upstream returned an error without a public diagnostic message", message)
	require.NotContains(t, message, "access-value")
	require.NotContains(t, message, "client-value")
	require.NotContains(t, message, "auth-value")

	status, message = testChannelHTTPError(&httpclient.Error{
		StatusCode: http.StatusBadGateway,
		Body:       []byte(`not-json access_token=access-value`),
	}, http.StatusBadGateway, errors.New("cause includes access-value"))
	require.Equal(t, http.StatusBadGateway, *status)
	require.Equal(t, "Upstream returned an error without a public diagnostic message", message)
}

func TestExplicitUnsupportedTestModelClassificationIsNarrow(t *testing.T) {
	require.True(t, isExplicitUnsupportedTestModel(
		http.StatusBadRequest,
		"The 'gpt-5' model is not supported when using Codex with a ChatGPT account.",
	))
	require.True(t, isExplicitUnsupportedTestModel(http.StatusNotFound, "model_not_found"))
	require.False(t, isExplicitUnsupportedTestModel(http.StatusUnauthorized, "model is not supported"))
	require.False(t, isExplicitUnsupportedTestModel(http.StatusBadRequest, "invalid authentication token"))
	require.False(t, isExplicitUnsupportedTestModel(0, "invalid model"))

	rawErr := &httpclient.Error{
		StatusCode: http.StatusNotFound,
		Body:       []byte(`{"error":{"message":"Request rejected","code":"model_not_found","type":"invalid_request_error"}}`),
	}
	require.True(t, isExplicitUnsupportedTestModel(
		http.StatusNotFound,
		testChannelModelErrorEvidence(rawErr, "Request rejected"),
	))
}

func TestChannelNonStreamOutputRequiresMeaningfulContent(t *testing.T) {
	toolOnly := &llm.Response{Choices: []llm.Choice{{
		Message: &llm.Message{ToolCalls: []llm.ToolCall{{ID: "call_1"}}},
	}}}
	message, ok := testChannelNonStreamOutput(toolOnly)
	require.True(t, ok)
	require.Nil(t, message)

	emptyChoice := &llm.Response{Choices: []llm.Choice{{
		Message: &llm.Message{Content: llm.MessageContent{Content: new("   ")}},
	}}}
	message, ok = testChannelNonStreamOutput(emptyChoice)
	require.False(t, ok)
	require.Nil(t, message)
}

func TestChannelStreamToolCallWithSuccessfulTerminalPasses(t *testing.T) {
	stream := streams.SliceStream([]*httpclient.StreamEvent{
		{Data: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"ping","arguments":"{}"}}]}}]}`)},
		{Data: []byte(`{"choices":[{"finish_reason":"tool_calls"}]}`)},
	})

	result, err := (&TestChannelOrchestrator{}).handleStreamResponse(context.Background(), stream, time.Now())
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Empty(t, result.Error)
}

func TestChannelStreamToolCallWithoutTerminalFails(t *testing.T) {
	stream := streams.SliceStream([]*httpclient.StreamEvent{{
		Data: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"ping","arguments":"{}"}}]}}]}`),
	}})

	result, err := (&TestChannelOrchestrator{}).handleStreamResponse(context.Background(), stream, time.Now())
	require.NoError(t, err)
	require.False(t, result.Success)
	require.NotNil(t, result.Error)
	require.Contains(t, *result.Error, "without a successful terminal event")
}

func TestChannelStreamTextWithoutTerminalFails(t *testing.T) {
	stream := streams.SliceStream([]*httpclient.StreamEvent{{
		Data: []byte(`{"choices":[{"delta":{"content":"partial"}}]}`),
	}})

	result, err := (&TestChannelOrchestrator{}).handleStreamResponse(context.Background(), stream, time.Now())
	require.NoError(t, err)
	require.False(t, result.Success)
	require.NotNil(t, result.Error)
	require.Contains(t, *result.Error, "without a successful terminal event")
}

func TestChannelStreamLengthWithContentPassesHealthTest(t *testing.T) {
	stream := streams.SliceStream([]*httpclient.StreamEvent{
		{Data: []byte(`{"choices":[{"delta":{"content":"partial"}}]}`)},
		{Data: []byte(`{"choices":[{"finish_reason":"length"}]}`)},
	})

	result, err := (&TestChannelOrchestrator{}).handleStreamResponse(context.Background(), stream, time.Now())
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Nil(t, result.Error)
}

func TestChannelStreamProviderIncompleteCannotBeHiddenBySyntheticDone(t *testing.T) {
	providerVerdict := &testChannelStreamVerdict{}
	providerStream := &testChannelVerdictStream{
		stream: streams.SliceStream([]*llm.Response{
			{Choices: []llm.Choice{{Delta: &llm.Message{Content: llm.MessageContent{Content: new("partial")}}}}},
			{ProtocolStatus: "incomplete", IncompleteReason: "max_output_tokens", Choices: []llm.Choice{{FinishReason: new("length")}}},
			llm.DoneResponse,
		}),
		verdict: providerVerdict,
	}
	clientStream, err := openai.NewInboundTransformer().TransformStream(context.Background(), providerStream)
	require.NoError(t, err)

	result, err := (&TestChannelOrchestrator{}).handleStreamResponse(
		context.Background(),
		clientStream,
		time.Now(),
		providerVerdict,
	)
	require.NoError(t, err)
	require.False(t, result.Success)
	require.NotNil(t, result.Error)
	require.Contains(t, *result.Error, "incomplete")
}

func TestChannelStreamCloseErrorFailsVerdict(t *testing.T) {
	stream := &testChannelHTTPEventStream{
		events: []*httpclient.StreamEvent{
			{Data: []byte(`{"choices":[{"delta":{"content":"ok"}}]}`)},
			{Data: []byte(`{"choices":[{"finish_reason":"stop"}]}`)},
		},
		closeErr: errors.New("provider close failed"),
	}

	result, err := (&TestChannelOrchestrator{}).handleStreamResponse(context.Background(), stream, time.Now())
	require.NoError(t, err)
	require.False(t, result.Success)
	require.NotNil(t, result.Error)
	require.Equal(t, "provider close failed", *result.Error)
}

func TestSingleAPIKeyRouteUsesOneAttemptAndLeavesVerdictToTriggerTest(t *testing.T) {
	var requestCount atomic.Int64
	var unexpectedCredential atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount.Add(1)
		if request.Header.Get("Authorization") != "Bearer bad-key" {
			unexpectedCredential.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"forced bad route"}}`))
	}))
	defer server.Close()

	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:tester-exact-route?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	channelRow, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Exact route channel").
		SetBaseURL(server.URL + "/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"bad-key", "good-key"}}).
		SetSupportedModels([]string{"gpt-test"}).
		SetDefaultTestModel("gpt-test").
		Save(ctx)
	require.NoError(t, err)

	channelService, requestService, systemService, usageLogService := setupTestServices(t, client)
	require.NoError(t, systemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
		Enabled:                 true,
		MaxChannelRetries:       4,
		MaxSingleChannelRetries: 4,
		RetryDelayMs:            0,
	}))
	tester := NewTestChannelOrchestrator(
		channelService,
		requestService,
		systemService,
		usageLogService,
		nil,
		httpclient.NewHttpClientWithClient(server.Client()),
	)
	routeChannel, err := channelService.GetChannel(ctx, channelRow.ID)
	require.NoError(t, err)
	route := biz.RouteKey{
		ChannelID:      channelRow.ID,
		CredentialID:   biz.RouteCredentialFingerprint(routeChannel, "bad-key"),
		ActualModel:    "gpt-test",
		APIFormat:      string(llm.APIFormatOpenAIChatCompletion),
		ConfigRevision: biz.RouteConfigRevision(routeChannel, string(llm.APIFormatOpenAIChatCompletion), nil),
	}

	result, err := tester.TestSingleAPIKeyRoute(
		contexts.WithSource(ctx, "test"),
		objects.GUID{ID: channelRow.ID},
		"bad-key",
		new("gpt-test"),
		nil,
		route,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Success)
	require.NotNil(t, result.routeKey)
	require.Equal(t, route, *result.routeKey)
	require.EqualValues(t, 1, requestCount.Load(), "TestSingleAPIKey must never spend production retry budget")
	require.False(t, unexpectedCredential.Load(), "TestSingleAPIKey must never switch to another configured key")
	require.False(t, channelService.UnifiedRouteState().Availability(route).Known,
		"exact Route probes return a verdict to TriggerTest instead of allocating/writing an inner generation")
}

type testChannelHTTPEventStream struct {
	events   []*httpclient.StreamEvent
	index    int
	closeErr error
}

func (s *testChannelHTTPEventStream) Next() bool {
	return s.index < len(s.events)
}

func (s *testChannelHTTPEventStream) Current() *httpclient.StreamEvent {
	if s.index >= len(s.events) {
		return nil
	}
	event := s.events[s.index]
	s.index++
	return event
}

func (s *testChannelHTTPEventStream) Err() error {
	return nil
}

func (s *testChannelHTTPEventStream) Close() error {
	return s.closeErr
}

func TestChannelLLMFailurePreservesStructuredDiagnostic(t *testing.T) {
	response := &llm.Response{Error: &llm.ResponseError{
		StatusCode: http.StatusBadGateway,
		Detail: llm.ErrorDetail{
			Type:    "invalid_request_error",
			Code:    "unsupported_value",
			Message: "reasoning.context must be all_turns",
		},
	}}

	message, statusCode := testChannelLLMFailure(response)
	require.Equal(t, "reasoning.context must be all_turns (code: unsupported_value, type: invalid_request_error)", message)
	require.NotNil(t, statusCode)
	require.Equal(t, http.StatusBadGateway, *statusCode)
}
