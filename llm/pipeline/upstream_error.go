package pipeline

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// UpstreamError marks errors that originate from the upstream provider path.
type UpstreamError struct {
	Err error
}

func (e *UpstreamError) Error() string {
	if e == nil || e.Err == nil {
		return "upstream error"
	}

	return e.Err.Error()
}

func (e *UpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.Err
}

func WrapUpstreamError(err error) error {
	if err == nil {
		return nil
	}

	var upstreamErr *UpstreamError
	if errors.As(err, &upstreamErr) {
		return err
	}

	return &UpstreamError{Err: err}
}

func IsUpstreamError(err error) bool {
	var upstreamErr *UpstreamError
	return errors.As(err, &upstreamErr)
}

// UpstreamAttemptFailureCategory is a privacy-safe classification of one
// failed provider attempt. It intentionally contains no request/response body,
// channel credential, URL, or header data.
type UpstreamAttemptFailureCategory string

const (
	UpstreamAttemptIncompleteStream  UpstreamAttemptFailureCategory = "incomplete_stream"
	UpstreamAttemptAuthentication    UpstreamAttemptFailureCategory = "authentication"
	UpstreamAttemptQuota             UpstreamAttemptFailureCategory = "upstream_quota"
	UpstreamAttemptModelNotSupported UpstreamAttemptFailureCategory = "model_not_supported"
	UpstreamAttemptRateLimited       UpstreamAttemptFailureCategory = "rate_limited"
	UpstreamAttemptTimeout           UpstreamAttemptFailureCategory = "upstream_timeout"
	UpstreamAttemptUnavailable       UpstreamAttemptFailureCategory = "upstream_unavailable"
	UpstreamAttemptOther             UpstreamAttemptFailureCategory = "other"
)

// UpstreamCandidatesExhaustedError reports that more than one provider attempt
// failed before any response was committed. Only aggregate counters and the
// already-existing final error are retained; prior provider bodies are not kept.
type UpstreamCandidatesExhaustedError struct {
	AttemptCount   int
	CategoryCounts map[UpstreamAttemptFailureCategory]int
	LastErr        error
}

func (e *UpstreamCandidatesExhaustedError) Error() string {
	if e == nil {
		return "upstream candidates exhausted"
	}

	return fmt.Sprintf("all upstream candidates failed after %d attempts", e.AttemptCount)
}

// Unwrap preserves status/code inspection of the final upstream error without
// making it the only user-visible explanation.
func (e *UpstreamCandidatesExhaustedError) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.LastErr
}

func classifyUpstreamAttemptFailure(err error) UpstreamAttemptFailureCategory {
	if errors.Is(err, llm.ErrStreamIncomplete) ||
		errors.Is(err, ErrEmptyResponse) ||
		errors.Is(err, ErrEmptyStreamChunks) ||
		errors.Is(err, ErrEmptyAggregatedBody) {
		return UpstreamAttemptIncompleteStream
	}
	if errors.Is(err, ErrStreamFirstEventTimeout) || errors.Is(err, ErrNonStreamResponseTimeout) {
		return UpstreamAttemptTimeout
	}

	statusCode := upstreamAttemptStatusCode(err)
	switch statusCode {
	case http.StatusUnauthorized:
		return UpstreamAttemptAuthentication
	case http.StatusPaymentRequired:
		return UpstreamAttemptQuota
	case http.StatusTooManyRequests:
		return UpstreamAttemptRateLimited
	}

	if isUnsupportedModelAttempt(statusCode, upstreamAttemptEvidence(err)) {
		return UpstreamAttemptModelNotSupported
	}
	if statusCode >= http.StatusInternalServerError || (statusCode == 0 && IsUpstreamError(err)) {
		return UpstreamAttemptUnavailable
	}

	return UpstreamAttemptOther
}

func upstreamAttemptStatusCode(err error) int {
	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}

	var responseErr *llm.ResponseError
	if errors.As(err, &responseErr) {
		return responseErr.StatusCode
	}

	return 0
}

func upstreamAttemptEvidence(err error) string {
	var parts []string

	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) {
		parts = append(parts, string(httpErr.Body), httpErr.Status)
	}

	var responseErr *llm.ResponseError
	if errors.As(err, &responseErr) {
		parts = append(parts, responseErr.Detail.Message, responseErr.Detail.Code, responseErr.Detail.Type)
	}

	return strings.ToLower(strings.Join(parts, " "))
}

func isUnsupportedModelAttempt(statusCode int, evidence string) bool {
	switch statusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
	default:
		return false
	}

	if !strings.Contains(evidence, "model") {
		return false
	}
	for _, signal := range []string{
		"not supported",
		"not_supported",
		"unsupported",
		"not found",
		"does not exist",
		"unknown model",
		"invalid model",
		"model_not_found",
	} {
		if strings.Contains(evidence, signal) {
			return true
		}
	}

	return false
}
