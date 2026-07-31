package orchestrator

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	return httpclient.IsHTTPStatusCodeRetryable(ExtractStatusCodeFromError(err))
}

func isExplicitUnsupportedModelError(err error) bool {
	if err == nil {
		return false
	}

	statusCode := ExtractStatusCodeFromError(err)
	return isExplicitUnsupportedModel(statusCode, unsupportedModelErrorEvidence(err))
}

func unsupportedModelErrorEvidence(err error) string {
	if err == nil {
		return ""
	}

	var parts []string

	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) {
		parts = append(parts, string(httpErr.Body), httpErr.Status)
	}

	var llmErr *llm.ResponseError
	if errors.As(err, &llmErr) {
		parts = append(parts, llmErr.Detail.Message, llmErr.Detail.Code, llmErr.Detail.Type)
	}
	if len(parts) == 0 {
		parts = append(parts, err.Error())
	}

	return strings.ToLower(strings.Join(parts, " "))
}

func isExplicitUnsupportedModel(statusCode int, message string) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
	default:
		return false
	}

	message = strings.ToLower(message)
	if !strings.Contains(message, "model") {
		return false
	}
	for _, signal := range []string{
		"not supported",
		"unsupported",
		"not found",
		"does not exist",
		"unknown model",
		"invalid model",
		"model_not_found",
	} {
		if strings.Contains(message, signal) {
			return true
		}
	}

	return false
}

func isRetryableErrorForChannel(err error, ch *biz.Channel) bool {
	if err == nil {
		return false
	}

	statusCode := ExtractStatusCodeFromError(err)
	if httpclient.IsHTTPStatusCodeRetryable(statusCode) {
		return true
	}
	// A status-less failure means no valid HTTP/provider response was obtained
	// (for example TLS, connection reset, decode failure, or response timeout).
	// Local admission/circuit errors are filtered by CanRetry before this helper.
	// Give the same channel exactly the configured one retry before failover.
	if statusCode == 0 {
		return true
	}

	if ch == nil || ch.Channel == nil || ch.Settings == nil {
		return false
	}

	return slices.Contains(ch.Settings.RetryableStatusCodes, statusCode) ||
		matchesRetryableErrorPattern(err, ch.Settings.RetryableErrorPatterns)
}

func matchesRetryableErrorPattern(err error, patterns []objects.RetryableErrorPattern) bool {
	if err == nil || len(patterns) == 0 {
		return false
	}

	message := err.Error()
	if message == "" {
		return false
	}

	for _, pattern := range patterns {
		if pattern.Pattern == "" {
			continue
		}

		if pattern.Regex {
			matched, regexErr := regexp.MatchString(pattern.Pattern, message)
			if regexErr == nil && matched {
				return true
			}

			continue
		}

		if strings.Contains(message, pattern.Pattern) {
			return true
		}
	}

	return false
}

// ExtractStatusCodeFromError attempts to extract HTTP status code from various error types.
func ExtractStatusCodeFromError(err error) int {
	if err == nil {
		return 0
	}

	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}

	var llmErr *llm.ResponseError
	if errors.As(err, &llmErr) {
		return llmErr.StatusCode
	}

	return 0
}

func deriveLoadBalancerStrategy(retryPolicy *biz.RetryPolicy, apiKey *ent.APIKey) string {
	strategy := retryPolicy.LoadBalancerStrategy
	if apiKey == nil {
		return strategy
	}
	if apiKey.Type == apikey.TypePersonal {
		return strategy
	}

	activeProfile := apiKey.GetActiveProfile()
	if activeProfile == nil {
		return strategy
	}

	if activeProfile.LoadBalanceStrategy == nil ||
		*activeProfile.LoadBalanceStrategy == "" ||
		*activeProfile.LoadBalanceStrategy == "system_default" {
		return strategy
	}

	return *activeProfile.LoadBalanceStrategy
}
