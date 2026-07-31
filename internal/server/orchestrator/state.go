package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// PersistenceState holds shared state with channel management and retry capabilities.
// TODO: move the dependencies out of the state to make it a real state.
type PersistenceState struct {
	APIKey *ent.APIKey

	RequestService      *biz.RequestService
	UsageLogService     *biz.UsageLogService
	ChannelService      *biz.ChannelService
	PromptProvider      PromptProvider
	PromptProtecter     PromptProtecter
	RetryPolicyProvider RetryPolicyProvider
	CandidateSelector   CandidateSelector
	LoadBalancer        *LoadBalancer
	SessionAffinity     *SessionAffinityTracker

	// Request state
	ModelMapper *ModelMapper
	// Proxy config, will be used to override channel's default proxy config.
	Proxy *httpclient.ProxyConfig

	// OriginalModel is the model after API key profile mapping, used for channel selection
	OriginalModel string
	RawRequest    *httpclient.Request
	LlmRequest    *llm.Request
	// SessionAffinityKey is a salted digest of the stable conversation prefix.
	// It is only populated when the request has no explicit trace/session ID.
	SessionAffinityKey string

	// OriginalRequestStream stores the client's original stream intent before any
	// candidate-specific forcing to provider-side streaming happens.
	OriginalRequestStream *bool

	// PromptPayloadMutated records that gateway prompt injection or protection
	// changed the normalized prompt. Raw request-body reuse must then stay off so
	// the transformed prompt, rather than the original client bytes, reaches upstream.
	PromptPayloadMutated bool

	// Persistence state
	Request     *ent.Request
	RequestExec *ent.RequestExecution

	// ChannelModelsCandidates is the primary state for channel selection
	ChannelModelsCandidates []*ChannelModelsCandidate
	// Candidate state - current candidate index of ChannelModelsCandidates
	CurrentCandidateIndex int
	// CurrentCandidate is the currently selected candidate of ChannelModelsCandidates
	CurrentCandidate *ChannelModelsCandidate
	// CurrentModelIndex is the current model index in CurrentCandidate.Models
	CurrentModelIndex int

	// Perf is the performance record for the current request.
	Perf *biz.PerformanceRecord

	// StreamCompleted tracks whether the client-facing stream completed. It is
	// intentionally separate from OutboundStreamCompleted: provider and client
	// protocols can have different terminal events (for example, Responses
	// response.incomplete must never become a successful client request).
	StreamCompleted bool

	// OutboundStreamCompleted tracks successful provider-protocol termination for
	// the current attempt. A raw sentinel alone is not sufficient for Responses.
	OutboundStreamCompleted bool

	// AttemptAccepted is set only after the pipeline has committed the current
	// streaming attempt (or successfully auto-aggregated it). AttemptSemanticOutput
	// uses the pipeline's shared content classifier, so normal text and pure tool
	// calls count while terminal/usage-only streams do not. Together they decide
	// whether a failed or partial attempt is allowed to settle usage.
	AttemptAccepted       bool
	AttemptSemanticOutput bool

	// RawProviderResponse stores the raw provider response for non-stream response pass-through.
	RawProviderResponse *httpclient.Response

	// RawProviderRequest stores the actual outbound provider request for pass-through checks.
	RawProviderRequest *httpclient.Request

	// RawStreamCh receives raw provider stream events for stream response pass-through.
	RawStreamCh chan *httpclient.StreamEvent

	// RawStreamErrRef points to the current attempt's local error variable used by the
	// captureRawProviderStream fan-out goroutine. Using a per-attempt pointer (instead of
	// a single shared field) prevents data races when retries spawn a new goroutine before
	// the previous one has exited.
	RawStreamErrRef *error

	// RawStreamCancel cancels the current attempt's fan-out goroutine started by
	// captureRawProviderStream. Must be called in PrepareForRetry and NextChannel so the
	// abandoned goroutine exits promptly and releases its upstream HTTP connection.
	RawStreamCancel context.CancelFunc

	// PassThroughApplied records whether the inbound request body was substituted during pass-through.
	PassThroughApplied bool
}
