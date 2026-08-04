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

func TestIsExplicitUnsupportedModel(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		message  string
		expected bool
	}{
		{
			name:     "http body model not supported",
			status:   http.StatusBadRequest,
			message:  "The requested model is not supported.",
			expected: true,
		},
		{
			name:     "llm response error model_not_found",
			status:   http.StatusNotFound,
			message:  "Request rejected model_not_found",
			expected: true,
		},
		{
			name:     "auth error is not unsupported model",
			status:   http.StatusUnauthorized,
			message:  "model is not supported",
			expected: false,
		},
		{
			name:     "generic 400 is not unsupported model",
			status:   http.StatusBadRequest,
			message:  "invalid authentication token",
			expected: false,
		},
		{
			name:     "model path with generic 404 is not unsupported model",
			status:   http.StatusNotFound,
			message:  "resource not found",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, isExplicitUnsupportedModel(tt.status, tt.message))
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

func TestFinalizeUpstreamCandidatesExhaustedError_Single402ClarifiesQuotaScope(t *testing.T) {
	upstreamErr := &llm.ResponseError{
		StatusCode: http.StatusPaymentRequired,
		Detail: llm.ErrorDetail{
			Message: "You have exceeded your monthly quota; access_token=sk-sensitive-token-123456",
		},
	}

	clientErr, lastExecutionErr := finalizeUpstreamCandidatesExhaustedError(upstreamErr)
	require.ErrorIs(t, lastExecutionErr, upstreamErr)
	assert.Equal(t, http.StatusPaymentRequired, ExtractStatusCodeFromError(lastExecutionErr))

	var responseErr *llm.ResponseError
	require.ErrorAs(t, clientErr, &responseErr)
	assert.Equal(t, http.StatusServiceUnavailable, responseErr.StatusCode)
	assert.Equal(t, upstreamSharedQuotaExhausted, responseErr.Detail.Type)
	assert.Equal(t, upstreamSharedQuotaExhausted, responseErr.Detail.Code)
	assert.Contains(t, responseErr.Detail.Message, "shared upstream provider channel")
	assert.Contains(t, responseErr.Detail.Message, "not your campus daily/weekly quota or billing")
	assert.Contains(t, responseErr.Detail.Message, "You have exceeded your monthly quota")
	assert.NotContains(t, responseErr.Detail.Message, "sk-sensitive-token-123456")
	assert.Contains(t, responseErr.Detail.Message, "[REDACTED]")
}
