package orchestrator

import (
	"errors"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	openairesponses "github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestDeriveLoadBalancerStrategy(t *testing.T) {
	defaultStrategy := "adaptive"
	retryPolicy := &biz.RetryPolicy{
		LoadBalancerStrategy: defaultStrategy,
	}

	tests := []struct {
		name     string
		apiKey   *ent.APIKey
		expected string
	}{
		{
			name:     "apiKey is nil",
			apiKey:   nil,
			expected: defaultStrategy,
		},
		{
			name: "active profile is nil",
			apiKey: &ent.APIKey{
				Profiles: nil,
			},
			expected: defaultStrategy,
		},
		{
			name: "active profile name is empty",
			apiKey: &ent.APIKey{
				Profiles: &objects.APIKeyProfiles{
					ActiveProfile: "",
				},
			},
			expected: defaultStrategy,
		},
		{
			name: "active profile not found in profiles list",
			apiKey: &ent.APIKey{
				Profiles: &objects.APIKeyProfiles{
					ActiveProfile: "non-existent",
					Profiles: []objects.APIKeyProfile{
						{Name: "other"},
					},
				},
			},
			expected: defaultStrategy,
		},
		{
			name: "load balance strategy is nil in active profile",
			apiKey: &ent.APIKey{
				Profiles: &objects.APIKeyProfiles{
					ActiveProfile: "default",
					Profiles: []objects.APIKeyProfile{
						{
							Name:                "default",
							LoadBalanceStrategy: nil,
						},
					},
				},
			},
			expected: defaultStrategy,
		},
		{
			name: "load balance strategy is empty in active profile",
			apiKey: &ent.APIKey{
				Profiles: &objects.APIKeyProfiles{
					ActiveProfile: "default",
					Profiles: []objects.APIKeyProfile{
						{
							Name:                "default",
							LoadBalanceStrategy: lo.ToPtr(""),
						},
					},
				},
			},
			expected: defaultStrategy,
		},
		{
			name: "load balance strategy is system_default in active profile",
			apiKey: &ent.APIKey{
				Profiles: &objects.APIKeyProfiles{
					ActiveProfile: "default",
					Profiles: []objects.APIKeyProfile{
						{
							Name:                "default",
							LoadBalanceStrategy: lo.ToPtr("system_default"),
						},
					},
				},
			},
			expected: defaultStrategy,
		},
		{
			name: "load balance strategy is set to specific value in active profile",
			apiKey: &ent.APIKey{
				Profiles: &objects.APIKeyProfiles{
					ActiveProfile: "default",
					Profiles: []objects.APIKeyProfile{
						{
							Name:                "default",
							LoadBalanceStrategy: lo.ToPtr("failover"),
						},
					},
				},
			},
			expected: "failover",
		},
		{
			name: "load balance strategy is round-robin in active profile",
			apiKey: &ent.APIKey{
				Profiles: &objects.APIKeyProfiles{
					ActiveProfile: "default",
					Profiles: []objects.APIKeyProfile{
						{
							Name:                "default",
							LoadBalanceStrategy: lo.ToPtr(biz.LoadBalancerStrategyRoundRobin),
						},
					},
				},
			},
			expected: biz.LoadBalancerStrategyRoundRobin,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deriveLoadBalancerStrategy(retryPolicy, tt.apiKey)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractStatusCodeFromError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected int
	}{
		{
			name:     "error is nil",
			err:      nil,
			expected: 0,
		},
		{
			name: "httpclient.Error",
			err: &httpclient.Error{
				StatusCode: http.StatusTooManyRequests,
			},
			expected: http.StatusTooManyRequests,
		},
		{
			name: "llm.ResponseError",
			err: &llm.ResponseError{
				StatusCode: http.StatusInternalServerError,
			},
			expected: http.StatusInternalServerError,
		},
		{
			name:     "wrapped httpclient.Error",
			err:      errors.New("wrapped: " + (&httpclient.Error{StatusCode: 401}).Error()), // This won't work with errors.As unless we use fmt.Errorf with %w
			expected: 0,
		},
		{
			name:     "wrapped httpclient.Error with %w",
			err:      errors.Join(errors.New("error"), &httpclient.Error{StatusCode: 401}),
			expected: 401,
		},
		{
			name:     "generic error",
			err:      errors.New("generic error"),
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExtractStatusCodeFromError(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsExplicitUnsupportedModelError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name: "http body model not supported",
			err: &httpclient.Error{
				StatusCode: http.StatusBadRequest,
				Body:       []byte(`The requested model is not supported.`),
			},
			expected: true,
		},
		{
			name: "llm response error model_not_found",
			err: &llm.ResponseError{
				StatusCode: http.StatusNotFound,
				Detail: llm.ErrorDetail{
					Message: "Request rejected",
					Code:    "model_not_found",
				},
			},
			expected: true,
		},
		{
			name: "auth error is not unsupported model",
			err: &httpclient.Error{
				StatusCode: http.StatusUnauthorized,
				Body:       []byte(`model is not supported`),
			},
			expected: false,
		},
		{
			name: "generic 400 is not unsupported model",
			err: &httpclient.Error{
				StatusCode: http.StatusBadRequest,
				Body:       []byte(`invalid authentication token`),
			},
			expected: false,
		},
		{
			name: "model path with generic 404 is not unsupported model",
			err: &httpclient.Error{
				Method:     http.MethodGet,
				URL:        "https://provider.example/v1/models/gpt-5.6-sol",
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Body:       []byte(`resource not found`),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isExplicitUnsupportedModelError(tt.err))
		})
	}
}

func TestFinalizeUpstreamCandidatesExhaustedError_TerraChainEndingIn402(t *testing.T) {
	lastErr := &llm.ResponseError{
		StatusCode: http.StatusPaymentRequired,
		Detail: llm.ErrorDetail{
			Message: "You have exceeded your monthly quota; access_token=sk-sensitive-token-123456; contact person@example.test",
			Type:    "insufficient_quota",
		},
	}
	err := &pipeline.UpstreamCandidatesExhaustedError{
		AttemptCount: 5,
		CategoryCounts: map[pipeline.UpstreamAttemptFailureCategory]int{
			pipeline.UpstreamAttemptIncompleteStream: 3,
			pipeline.UpstreamAttemptAuthentication:   1,
			pipeline.UpstreamAttemptQuota:            1,
		},
		LastErr: lastErr,
	}

	clientErr, lastExecutionErr := finalizeUpstreamCandidatesExhaustedError(err)

	require.ErrorIs(t, lastExecutionErr, lastErr)
	assert.Equal(t, http.StatusPaymentRequired, ExtractStatusCodeFromError(lastExecutionErr))

	var responseErr *llm.ResponseError
	require.ErrorAs(t, clientErr, &responseErr)
	assert.Equal(t, http.StatusServiceUnavailable, responseErr.StatusCode)
	assert.Equal(t, upstreamCandidatesExhausted, responseErr.Detail.Type)
	assert.Equal(t, upstreamCandidatesExhausted, responseErr.Detail.Code)
	assert.Contains(t, responseErr.Detail.Message, "3 incomplete streams")
	assert.Contains(t, responseErr.Detail.Message, "1 authentication failure")
	assert.Contains(t, responseErr.Detail.Message, "1 upstream quota exhaustion")
	assert.Contains(t, responseErr.Detail.Message, "Last upstream error (HTTP 402): You have exceeded your monthly quota")
	assert.Contains(t, responseErr.Detail.Message, "shared provider channel's allowance")
	assert.Contains(t, responseErr.Detail.Message, "not the caller's campus daily/weekly quota or billing")
	assert.NotContains(t, responseErr.Detail.Message, "sk-sensitive-token-123456")
	assert.NotContains(t, responseErr.Detail.Message, "person@example.test")
	assert.Contains(t, responseErr.Detail.Message, "[REDACTED]")
	assert.Contains(t, responseErr.Detail.Message, "[EMAIL]")

	httpErr := openairesponses.NewInboundTransformer().TransformError(t.Context(), clientErr)
	assert.Equal(t, http.StatusServiceUnavailable, httpErr.StatusCode)
	assert.Equal(t, upstreamCandidatesExhausted, gjson.GetBytes(httpErr.Body, "error.type").String())
	assert.Equal(t, upstreamCandidatesExhausted, gjson.GetBytes(httpErr.Body, "error.code").String())
}

func TestIsRetryableError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "error is nil",
			err:      nil,
			expected: false,
		},
		{
			name: "429 Too Many Requests is retryable",
			err: &httpclient.Error{
				StatusCode: http.StatusTooManyRequests,
			},
			expected: true,
		},
		{
			name: "400 Bad Request is not retryable",
			err: &httpclient.Error{
				StatusCode: http.StatusBadRequest,
			},
			expected: false,
		},
		{
			name: "500 Internal Server Error is retryable",
			err: &llm.ResponseError{
				StatusCode: http.StatusInternalServerError,
			},
			expected: true,
		},
		{
			name:     "generic error is not retryable",
			err:      errors.New("generic error"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isRetryableError(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsRetryableErrorForChannel(t *testing.T) {
	channel := &biz.Channel{
		Channel: &ent.Channel{
			Settings: &objects.ChannelSettings{
				RetryableStatusCodes: []int{400, 403},
				RetryableErrorPatterns: []objects.RetryableErrorPattern{
					{Pattern: "Console API returned 403"},
					{Pattern: `Console API returned \d+`, Regex: true},
				},
			},
		},
	}

	tests := []struct {
		name     string
		err      error
		channel  *biz.Channel
		expected bool
	}{
		{
			name:     "error is nil",
			err:      nil,
			channel:  channel,
			expected: false,
		},
		{
			name: "default retryable status remains retryable",
			err: &httpclient.Error{
				StatusCode: http.StatusInternalServerError,
			},
			channel:  nil,
			expected: true,
		},
		{
			name: "configured 400 status is retryable",
			err: &httpclient.Error{
				StatusCode: http.StatusBadRequest,
			},
			channel:  channel,
			expected: true,
		},
		{
			name: "unconfigured 401 status is not retryable",
			err: &httpclient.Error{
				StatusCode: http.StatusUnauthorized,
			},
			channel:  channel,
			expected: false,
		},
		{
			name:     "configured error text is retryable",
			err:      errors.New("failed to stream request: error: Console API returned 403, code: upstream_error, type: upstream_error"),
			channel:  channel,
			expected: true,
		},
		{
			name:     "configured error regex is retryable",
			err:      errors.New("failed to stream request: error: Console API returned 502, code: upstream_error, type: upstream_error"),
			channel:  channel,
			expected: true,
		},
		{
			name:     "status-less unmatched error is retryable once by policy",
			err:      errors.New("failed to stream request: error: credentials rejected"),
			channel:  channel,
			expected: true,
		},
		{
			name: "configured status is not retryable without channel settings",
			err: &httpclient.Error{
				StatusCode: http.StatusBadRequest,
			},
			channel:  &biz.Channel{Channel: &ent.Channel{}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isRetryableErrorForChannel(tt.err, tt.channel)
			assert.Equal(t, tt.expected, result)
		})
	}
}
