package orchestrator

import (
	"context"
	"time"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/cc"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func NewChatCompletionOrchestrator(
	channelService *biz.ChannelService,
	defaultSelector *DefaultSelector,
	requestService *biz.RequestService,
	httpClient *httpclient.HttpClient,
	inbound transformer.Inbound,
	systemService *biz.SystemService,
	usageLogService *biz.UsageLogService,
	promptService *biz.PromptService,
	quotaService *biz.QuotaService,
	promptProtectionRuleService *biz.PromptProtectionRuleService,
	liveStreamRegistry *biz.LiveStreamRegistry,
	channelLimiterManager *ChannelLimiterManager,
	quotaProvider ProviderQuotaStatusProvider,
) *ChatCompletionOrchestrator {
	rateLimitTracker := NewChannelRequestTracker()

	channelService.SetChannelLimiterForgetter(channelLimiterManager)

	channelLimiterMetrics, err := NewChannelLimiterMetrics(metrics.Meter, channelLimiterManager)
	if err != nil {
		log.Warn(context.Background(), "failed to register channel limiter metrics, continuing without them", log.Cause(err))
		channelLimiterMetrics = nil
	}

	sessionAffinity := NewSessionAffinityTracker(systemService)

	processor := &ChatCompletionOrchestrator{
		Inbound:            inbound,
		RequestService:     requestService,
		ChannelService:     channelService,
		SystemService:      systemService,
		UsageLogService:    usageLogService,
		QuotaService:       quotaService,
		LiveStreamRegistry: liveStreamRegistry,
		PromptProvider:     promptService,
		PromptProtecter:    promptProtectionRuleService,
		Middlewares: []pipeline.Middleware{
			cc.StripBillingHeaderCCH(),
			stream.EnsureUsage(),
		},
		PipelineFactory:       pipeline.NewFactory(httpClient),
		ModelMapper:           NewModelMapper(),
		channelSelector:       defaultSelector,
		channelLimiterManager: channelLimiterManager,
		channelLimiterMetrics: channelLimiterMetrics,
		rateLimitTracker:      rateLimitTracker,
		sessionAffinity:       sessionAffinity,
		proxy:                 nil,
	}
	processor.programmaticTester = NewTestChannelOrchestrator(
		channelService,
		requestService,
		systemService,
		usageLogService,
		promptProtectionRuleService,
		httpClient,
	)
	return processor
}

type ChatCompletionOrchestrator struct {
	Inbound            transformer.Inbound
	RequestService     *biz.RequestService
	ChannelService     *biz.ChannelService
	SystemService      *biz.SystemService
	UsageLogService    *biz.UsageLogService
	QuotaService       *biz.QuotaService
	LiveStreamRegistry *biz.LiveStreamRegistry
	PromptProvider     PromptProvider
	PromptProtecter    PromptProtecter
	Middlewares        []pipeline.Middleware
	PipelineFactory    *pipeline.Factory
	ModelMapper        *ModelMapper

	// The runtime fields.

	// The default channel selector.
	channelSelector CandidateSelector
	// channelLimiterManager owns per-channel concurrency admission control and
	// supplies in-flight / queue stats to the rate-limit-aware load-balancer strategy.
	channelLimiterManager *ChannelLimiterManager
	// channelLimiterMetrics emits OTel metrics for the limiter (gauges + counters
	// + histogram). May be nil in test setups that skip metric registration.
	channelLimiterMetrics *ChannelLimiterMetrics
	// The rate limit tracker for rate limit aware load balancing.
	rateLimitTracker *ChannelRequestTracker
	// Fallback affinity for clients that do not provide an explicit trace/session ID.
	sessionAffinity *SessionAffinityTracker
	// programmaticTester runs the same TestChannel verdict path after an
	// abnormal production attempt. Its result never blocks failover.
	programmaticTester *TestChannelOrchestrator
	// singleRouteOnly is reserved for authoritative TestChannel probes. It keeps
	// the normal transform/timeout/empty-response path, but forbids the pipeline
	// from retrying another credential, model, or channel and accidentally
	// attributing that route's result to the route under test.
	singleRouteOnly bool

	// proxy is the proxy configuration for testing
	// If set, it will override the channel's default proxy configuration
	proxy *httpclient.ProxyConfig
}

func (processor *ChatCompletionOrchestrator) WithChannelSelector(selector CandidateSelector) *ChatCompletionOrchestrator {
	c := *processor
	c.channelSelector = selector

	return &c
}

func (processor *ChatCompletionOrchestrator) WithAllowedChannels(allowedChannelIDs []int) *ChatCompletionOrchestrator {
	c := *processor
	c.channelSelector = WithSelectedChannelsSelector(processor.channelSelector, allowedChannelIDs)

	return &c
}

func (processor *ChatCompletionOrchestrator) WithProxy(proxy *httpclient.ProxyConfig) *ChatCompletionOrchestrator {
	c := *processor
	c.proxy = proxy

	return &c
}

type ChatCompletionResult struct {
	ChatCompletion       *httpclient.Response
	ChatCompletionStream streams.Stream[*httpclient.StreamEvent]
	// RouteKey is diagnostic metadata for TestChannel. CredentialID is a
	// fingerprint; API callers never receive the raw provider credential.
	RouteKey *biz.RouteKey
}

func (processor *ChatCompletionOrchestrator) Process(ctx context.Context, request *httpclient.Request) (ChatCompletionResult, error) {
	// The context is system bypassed to allow the orchestrator to access the system settings.
	ctx = authz.WithSystemBypass(ctx, "process-chat-completion")
	// Install the mutable request-local context container before an API-key
	// provider records its exact selected credential. Providers cannot replace
	// the caller's context through their Get interface, but can safely update the
	// already-attached container.
	ctx = contexts.WithChannelAPIKey(ctx, "")

	apiKey, _ := contexts.GetAPIKey(ctx)

	// Get retry policy from system settings
	retryPolicy := processor.SystemService.RetryPolicyOrDefault(ctx)

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "chat request received",
			log.Int("request_body_bytes", len(request.Body)),
			log.Int("request_header_count", len(request.Headers)),
			log.String("routing_strategy", "unified_fair_ring"),
		)
	}

	state := &PersistenceState{
		APIKey:                apiKey,
		RequestService:        processor.RequestService,
		UsageLogService:       processor.UsageLogService,
		ChannelService:        processor.ChannelService,
		PromptProvider:        processor.PromptProvider,
		PromptProtecter:       processor.PromptProtecter,
		CandidateSelector:     processor.channelSelector,
		SessionAffinity:       processor.sessionAffinity,
		UnifiedRoutes:         processor.ChannelService.UnifiedRouteState(),
		ProgrammaticTester:    processor.programmaticTester,
		ModelMapper:           processor.ModelMapper,
		Proxy:                 processor.proxy,
		CurrentCandidateIndex: 0,
	}

	var pipelineOpts []pipeline.Option

	// Only production requests may consume the global cross-route retry budget.
	// An authoritative TestChannel probe must yield one verdict for one concrete
	// route, regardless of the Owner's production retry settings.
	if retryPolicy.Enabled && !processor.singleRouteOnly {
		pipelineOpts = append(pipelineOpts, pipeline.WithRetry(
			retryPolicy.MaxChannelRetries,
			0,
			0,
		))
	}
	if retryPolicy.Enabled || processor.singleRouteOnly {
		pipelineOpts = append(pipelineOpts, pipeline.WithResponseTimeouts(
			time.Duration(retryPolicy.StreamFirstEventTimeoutSeconds)*time.Second,
			time.Duration(retryPolicy.NonStreamResponseTimeoutSeconds)*time.Second,
		))
	}
	// Unified routing treats every empty/incomplete upstream attempt as a failed
	// route even when retries are disabled. Legacy persisted settings may not
	// turn an invalid upstream response into success.
	pipelineOpts = append(pipelineOpts, pipeline.WithEmptyResponseDetection())

	var middlewares []pipeline.Middleware

	// Add global middlewares
	middlewares = append(middlewares, processor.Middlewares...)

	inbound, outbound := NewPersistentTransformers(state, processor.Inbound)

	// Add inbound middlewares (executed after inbound.TransformRequest)
	middlewares = append(middlewares,
		enforceQuota(inbound, processor.QuotaService),
		applyAutoReasoningEffort(processor.SystemService),
		checkApiKeyModelAccess(inbound),
		applyModelMapping(inbound),
		selectCandidates(inbound),
		injectPrompts(inbound),
		protectPrompts(inbound),
		// Response pass-through middlewares run before persistRequest so the raw provider
		// response is saved when pass-through is enabled.
		applyPassThroughResponse(outbound, processor.SystemService),
		persistRequest(inbound),
	)

	// Add outbound middlewares (executed after outbound.TransformRequest)
	middlewares = append(middlewares,
		// applyPassThroughBody runs first so that override operations can still modify the pass-through body.
		applyPassThroughRequestBody(outbound, processor.SystemService),
		applyOverrideRequestBody(outbound),
		// applyUserAgentPassThrough runs before header overrides to set the initial
		// User-Agent value (either from client pass-through or default "axonhub/1.0").
		// This allows override headers to modify the User-Agent if configured.
		applyUserAgentPassThrough(outbound, processor.SystemService),
		applyOverrideRequestHeaders(outbound),
		enforceCodexResponsesLiteInvariant(outbound),

		// Unified performance tracking middleware.
		withPerformanceRecording(outbound),

		withSessionAffinity(outbound, processor.sessionAffinity),

		// The request execution middleware must be the final middleware
		// to ensure that the request execution is created with the correct request bodys.
		persistRequestExecution(outbound),

		// Forward the events to the live streaming.
		withLivePreview(state, processor.SystemService, processor.LiveStreamRegistry),

		// Per-channel concurrency admission runs before RPM admission so a locally
		// rejected queue attempt does not consume RPM for a request that never
		// reached upstream.
		withChannelLimiter(outbound, processor.channelLimiterManager, processor.channelLimiterMetrics),
		// Strict single-instance RPM admission for every outbound attempt.
		withRateLimitAdmission(outbound, processor.rateLimitTracker),
		// Rate limit tracking middleware for TPM and provider cooldown signals.
		withRateLimitTracking(outbound, processor.rateLimitTracker),

		// Response pass-through capture middlewares must be last in the outbound list
		// so they run first in reverse order (before any other OnOutboundRawResponse/OnOutboundRawStream handlers).
		captureRawProviderResponse(outbound, processor.SystemService),
		captureRawProviderStream(outbound, processor.SystemService),
		// Must remain last: once direct streaming is accepted this middleware may
		// start a drain goroutine immediately. No later inbound stream middleware
		// may fail and roll the accepted attempt back underneath that goroutine.
		applyPassThroughStream(outbound, processor.SystemService),
	)

	pipelineOpts = append(pipelineOpts, pipeline.WithMiddlewares(middlewares...))

	pipe := processor.PipelineFactory.Pipeline(
		inbound,
		outbound,
		pipelineOpts...,
	)

	result, err := pipe.Process(ctx, request)
	if err != nil {
		clientErr, lastExecutionErr := finalizeUpstreamCandidatesExhaustedError(err)

		persistCtx, cancel := xcontext.DetachWithTimeout(ctx, time.Second*10)
		defer cancel()

		// Update the last request execution status based on error if it exists
		// This ensures that when retry fails completely, the last execution is properly marked
		if requestExec := outbound.GetRequestExecution(); requestExec != nil {
			if updateErr := processor.RequestService.UpdateRequestExecutionStatusFromError(
				persistCtx,
				requestExec.ID,
				lastExecutionErr,
			); updateErr != nil {
				log.Warn(persistCtx, "Failed to update request execution status from error", log.Cause(updateErr))
			}
		}

		// Update the main request status based on error
		if request := outbound.GetRequest(); request != nil {
			if updateErr := processor.RequestService.UpdateRequestStatusFromError(
				persistCtx,
				request.ID,
				clientErr,
			); updateErr != nil {
				log.Warn(persistCtx, "Failed to update request status from error", log.Cause(updateErr))
			}
		}

		return ChatCompletionResult{RouteKey: outbound.resultRouteKey(ctx)}, clientErr
	}

	// Return result based on stream type
	if result.Stream {
		return ChatCompletionResult{
			ChatCompletion:       nil,
			ChatCompletionStream: result.EventStream,
			RouteKey:             outbound.resultRouteKey(ctx),
		}, nil
	}

	return ChatCompletionResult{
		ChatCompletion:       result.Response,
		ChatCompletionStream: nil,
		RouteKey:             outbound.resultRouteKey(ctx),
	}, nil
}
