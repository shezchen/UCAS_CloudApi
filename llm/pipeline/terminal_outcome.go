package pipeline

import (
	"net/http"
	"strings"

	"github.com/looplj/axonhub/llm"
)

// TerminalOutcome is the single semantic classification used for unified LLM
// responses and aggregated stream metadata. A terminal failure always carries
// an actionable upstream error.
type TerminalOutcome struct {
	Terminal   bool
	Successful bool
	Err        error
}

// ResponseTerminalOutcome classifies one unified LLM response event.
//
// Explicit Responses lifecycle state takes precedence over generic Chat
// Completions finish_reason values. Therefore response.incomplete with a
// finish_reason of length is a failure, while an ordinary Chat Completions
// length/tool_calls/content_filter finish remains successful.
func ResponseTerminalOutcome(response *llm.Response) TerminalOutcome {
	if response == nil {
		return TerminalOutcome{}
	}
	if response.Error != nil {
		return TerminalOutcome{Terminal: true, Err: response.Error}
	}

	if outcome := protocolStatusOutcome(response.ProtocolStatus, response.IncompleteReason); outcome.Terminal {
		return outcome
	}

	if response == llm.DoneResponse || response.Object == "[DONE]" {
		return TerminalOutcome{Terminal: true, Successful: true}
	}
	for _, choice := range response.Choices {
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			return TerminalOutcome{Terminal: true, Successful: true}
		}
	}

	return TerminalOutcome{}
}

// ResponseMetaTerminalOutcome classifies the terminal state produced by a
// stream aggregator. Explicit protocol status wins over legacy boolean fields
// so contradictory metadata can never turn response.incomplete into success.
func ResponseMetaTerminalOutcome(meta llm.ResponseMeta) TerminalOutcome {
	if outcome := protocolStatusOutcome(meta.ProtocolStatus, meta.IncompleteReason); outcome.Terminal {
		return outcome
	}
	if meta.Completed {
		return TerminalOutcome{Terminal: true, Successful: true}
	}
	if meta.Terminal {
		return TerminalOutcome{
			Terminal: true,
			Err:      newProtocolTerminalError("not_completed", meta.IncompleteReason),
		}
	}

	return TerminalOutcome{}
}

func protocolStatusOutcome(protocolStatus, incompleteReason string) TerminalOutcome {
	status := strings.ToLower(strings.TrimSpace(protocolStatus))
	switch status {
	case "completed":
		return TerminalOutcome{Terminal: true, Successful: true}
	case "incomplete", "failed", "error", "canceled", "cancelled":
		return TerminalOutcome{
			Terminal: true,
			Err:      newProtocolTerminalError(status, incompleteReason),
		}
	default:
		// States such as queued or in_progress are not terminal stream events.
		return TerminalOutcome{}
	}
}

func newProtocolTerminalError(protocolStatus, incompleteReason string) error {
	status := strings.ToLower(strings.TrimSpace(protocolStatus))
	if status == "" {
		status = "not_completed"
	}
	codeStatus := status
	if codeStatus == "cancelled" {
		codeStatus = "canceled"
	}

	message := "upstream response ended with protocol status " + status
	if status == "incomplete" {
		message = "upstream response incomplete"
	}
	if reason := strings.TrimSpace(incompleteReason); reason != "" {
		message += ": " + reason
	}

	return &llm.ResponseError{
		StatusCode: http.StatusBadGateway,
		Detail: llm.ErrorDetail{
			Type:    "api_error",
			Code:    "response_" + codeStatus,
			Message: message,
		},
	}
}
