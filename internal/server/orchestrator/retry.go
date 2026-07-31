package orchestrator

import (
	"errors"
	"fmt"
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
	"github.com/looplj/axonhub/llm/pipeline"
)

const upstreamCandidatesExhausted = "upstream_candidates_exhausted"

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

// finalizeUpstreamCandidatesExhaustedError converts the pipeline's private,
// in-memory attempt counters into a safe client-facing 503. The final provider
// detail is sanitized with the same policy used by the six-hour activity view.
// The second return value remains the real final error so the last execution
// keeps its actual provider status and diagnostic instead of being overwritten
// by the aggregate summary.
func finalizeUpstreamCandidatesExhaustedError(err error) (clientErr, lastExecutionErr error) {
	var exhausted *pipeline.UpstreamCandidatesExhaustedError
	if !errors.As(err, &exhausted) {
		return err, err
	}

	lastErr := exhausted.LastErr
	if lastErr == nil {
		lastErr = err
	}

	counts := exhausted.CategoryCounts
	orderedCategories := []struct {
		category pipeline.UpstreamAttemptFailureCategory
		singular string
		plural   string
	}{
		{pipeline.UpstreamAttemptIncompleteStream, "incomplete stream", "incomplete streams"},
		{pipeline.UpstreamAttemptAuthentication, "authentication failure", "authentication failures"},
		{pipeline.UpstreamAttemptQuota, "upstream quota exhaustion", "upstream quota exhaustions"},
		{pipeline.UpstreamAttemptModelNotSupported, "model-not-supported rejection", "model-not-supported rejections"},
		{pipeline.UpstreamAttemptRateLimited, "rate-limit response", "rate-limit responses"},
		{pipeline.UpstreamAttemptTimeout, "upstream timeout", "upstream timeouts"},
		{pipeline.UpstreamAttemptUnavailable, "upstream unavailable error", "upstream unavailable errors"},
		{pipeline.UpstreamAttemptOther, "other upstream error", "other upstream errors"},
	}
	parts := make([]string, 0, len(orderedCategories))
	for _, entry := range orderedCategories {
		if count := counts[entry.category]; count > 0 {
			label := entry.plural
			if count == 1 {
				label = entry.singular
			}
			parts = append(parts, fmt.Sprintf("%d %s", count, label))
		}
	}

	message := fmt.Sprintf(
		"All upstream candidates failed after %d attempts: %s.",
		exhausted.AttemptCount,
		strings.Join(parts, ", "),
	)
	if detail := biz.SanitizeCampusDiagnosticError(upstreamFailureDetail(lastErr)); detail != "" {
		lastLabel := "Last upstream error"
		if statusCode := ExtractStatusCodeFromError(lastErr); statusCode > 0 {
			lastLabel = fmt.Sprintf("Last upstream error (HTTP %d)", statusCode)
		}
		message += " " + lastLabel + ": " + detail
	}
	if counts[pipeline.UpstreamAttemptQuota] > 0 {
		message += " Upstream quota here means the shared provider channel's allowance, not the caller's campus daily/weekly quota or billing."
	}

	return &llm.ResponseError{
		StatusCode: http.StatusServiceUnavailable,
		Detail: llm.ErrorDetail{
			Message: message,
			Type:    upstreamCandidatesExhausted,
			Code:    upstreamCandidatesExhausted,
		},
	}, lastErr
}

func upstreamFailureDetail(err error) string {
	var responseErr *llm.ResponseError
	if errors.As(err, &responseErr) && strings.TrimSpace(responseErr.Detail.Message) != "" {
		return responseErr.Detail.Message
	}

	return ExtractErrorMessage(err)
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
