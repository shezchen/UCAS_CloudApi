package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
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

func TestChannelStreamToolCallCountsAsHealthyOutput(t *testing.T) {
	stream := streams.SliceStream([]*httpclient.StreamEvent{{
		Data: []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"ping","arguments":"{}"}}]}}]}`),
	}})

	result, err := (&TestChannelOrchestrator{}).handleStreamResponse(context.Background(), stream, time.Now())
	require.NoError(t, err)
	require.True(t, result.Success)
	require.Empty(t, result.Error)
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
