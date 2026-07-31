package llm

import "errors"

// ErrStreamIncomplete indicates a provider stream ended without a terminal
// event (for example [DONE], response.completed, or an equivalent finish
// reason). Callers that see this before committing output to the client should
// treat it as a retryable upstream failure.
var ErrStreamIncomplete = errors.New("stream ended without terminal event")
