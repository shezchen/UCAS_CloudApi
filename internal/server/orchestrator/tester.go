package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/errgroup"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xjson"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

const testChannelAPIKeysMaxConcurrency = 8

// TestChannelOrchestrator handles channel testing functionality.
// It is stateless and can be reused across multiple test requests.
type TestChannelOrchestrator struct {
	channelService              *biz.ChannelService
	requestService              *biz.RequestService
	systemService               *biz.SystemService
	usageLogService             *biz.UsageLogService
	promptProtectionRuleService *biz.PromptProtectionRuleService
	httpClient                  *httpclient.HttpClient
	modelMapper                 *ModelMapper
	channelLimiterManager       *ChannelLimiterManager
}

// NewTestChannelOrchestrator creates a new TestChannelOrchestrator.
func NewTestChannelOrchestrator(
	channelService *biz.ChannelService,
	requestService *biz.RequestService,
	systemService *biz.SystemService,
	usageLogService *biz.UsageLogService,
	promptProtectionRuleService *biz.PromptProtectionRuleService,
	httpClient *httpclient.HttpClient,
) *TestChannelOrchestrator {
	return &TestChannelOrchestrator{
		channelService:              channelService,
		requestService:              requestService,
		systemService:               systemService,
		usageLogService:             usageLogService,
		promptProtectionRuleService: promptProtectionRuleService,
		httpClient:                  httpClient,
		modelMapper:                 NewModelMapper(),
		channelLimiterManager:       NewChannelLimiterManager(),
	}
}

// TestChannelRequest represents a channel test request.
type TestChannelRequest struct {
	ChannelID objects.GUID
	ModelID   *string
}

// TestChannelResult represents the result of a channel test.
type TestChannelResult struct {
	Latency          float64
	Success          bool
	Message          *string
	Error            *string
	StatusCode       *int
	ModelUnsupported bool
	routeKey         *biz.RouteKey
}

// TestChannel tests a specific channel with a simple request.
func (processor *TestChannelOrchestrator) TestChannel(
	ctx context.Context,
	channelID objects.GUID,
	modelID *string,
	proxy *httpclient.ProxyConfig,
) (result *TestChannelResult, resultErr error) {
	return processor.testChannel(ctx, channelID, modelID, proxy, nil, true)
}

// TestChannelRoute exercises the exact production protocol/model route. It is
// intentionally separate from the manual UI test, whose normal default-model
// selection remains unchanged.
func (processor *TestChannelOrchestrator) TestChannelRoute(
	ctx context.Context,
	channelID objects.GUID,
	modelID *string,
	proxy *httpclient.ProxyConfig,
	route biz.RouteKey,
) (result *TestChannelResult, resultErr error) {
	if route.ChannelID != channelID.ID {
		return nil, fmt.Errorf("route channel %d does not match requested channel %d", route.ChannelID, channelID.ID)
	}
	// TriggerTest owns the only generation/write for programmatic exact-route
	// probes. Recording here as well would allocate a newer inner generation and
	// make TriggerTest's outer write lose as stale.
	return processor.testChannel(ctx, channelID, modelID, proxy, &route, false)
}

func (processor *TestChannelOrchestrator) testChannel(
	ctx context.Context,
	channelID objects.GUID,
	modelID *string,
	proxy *httpclient.ProxyConfig,
	forcedRoute *biz.RouteKey,
	recordVerdict bool,
) (result *TestChannelResult, resultErr error) {
	ctx = contexts.WithIsolatedContainer(ctx)
	var testGeneration uint64
	if recordVerdict {
		testGeneration = processor.channelService.UnifiedRouteState().NextTestGeneration()
	}
	inbound := openai.NewInboundTransformer()
	streamVerdict := &testChannelStreamVerdict{}
	var promptProtecter PromptProtecter
	if processor.promptProtectionRuleService != nil {
		promptProtecter = processor.promptProtectionRuleService
	}
	selector := NewSpecifiedChannelSelector(processor.channelService, channelID)
	if forcedRoute != nil {
		selector.ForcedAPIFormat = forcedRoute.APIFormat
		selector.ForcedActualModel = forcedRoute.ActualModel
	}
	// Create ChatCompletionOrchestrator for this test request
	chatProcessor := &ChatCompletionOrchestrator{
		channelSelector: selector,
		RequestService:  processor.requestService,
		ChannelService:  processor.channelService,
		PromptProvider:  &stubPromptProvider{},
		PromptProtecter: promptProtecter,
		PipelineFactory: pipeline.NewFactory(processor.httpClient),
		Middlewares: []pipeline.Middleware{
			stream.EnsureUsage(),
			&testChannelStreamVerdictMiddleware{verdict: streamVerdict},
		},
		Inbound:               inbound,
		SystemService:         processor.systemService,
		UsageLogService:       processor.usageLogService,
		proxy:                 proxy,
		ModelMapper:           processor.modelMapper,
		channelLimiterManager: processor.channelLimiterManager,
		singleRouteOnly:       true,
	}

	channel, err := processor.channelService.GetChannel(ctx, channelID.ID)
	if err != nil {
		return nil, err
	}

	testModel := lo.FromPtr(modelID)
	if testModel == "" {
		testModel = channel.DefaultTestModel
	}
	var testRoute *biz.RouteKey
	defer func() {
		if result != nil {
			result.routeKey = testRoute
		}
		if recordVerdict {
			processor.recordCompletedTestVerdict(ctx, testGeneration, testRoute, result, resultErr)
		}
	}()

	// Check if the channel requires streaming
	useStream := channel != nil && channel.Policies.Stream == objects.CapabilityPolicyRequire

	// Create a simple test request
	llmRequest := &llm.Request{
		Model: testModel,
		Messages: []llm.Message{
			{
				Role: "system",
				Content: llm.MessageContent{
					Content: lo.ToPtr("You are a helpful assistant."),
				},
			},
			{
				Role: "user",
				Content: llm.MessageContent{
					MultipleContent: []llm.MessageContentPart{
						{
							Type: "text",
							Text: lo.ToPtr("Hello world, I'm AxonHub."),
						},
						{
							Type: "text",
							Text: lo.ToPtr("Please tell me who you are?"),
						},
					},
				},
			},
		},
		MaxCompletionTokens: lo.ToPtr(int64(256)),
		Stream:              lo.ToPtr(useStream),
	}

	body, err := json.Marshal(llmRequest)
	if err != nil {
		return nil, err
	}

	// Measure latency
	startTime := time.Now()
	rawResponse, err := chatProcessor.Process(ctx, &httpclient.Request{
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: body,
	})
	testRoute = rawResponse.RouteKey

	if err != nil {
		rawErr := inbound.TransformError(ctx, err)
		actualStatusCode := ExtractStatusCodeFromError(err)
		statusCode, message := testChannelHTTPError(rawErr, actualStatusCode, err)
		return &TestChannelResult{
			Latency:    time.Since(startTime).Seconds(),
			Success:    false,
			Message:    new(""),
			Error:      new(message),
			StatusCode: statusCode,
			ModelUnsupported: isExplicitUnsupportedTestModel(
				actualStatusCode,
				testChannelModelErrorEvidence(rawErr, message),
			),
		}, nil
	}

	statusCode := http.StatusOK
	if rawResponse.ChatCompletion != nil && rawResponse.ChatCompletion.StatusCode > 0 {
		statusCode = rawResponse.ChatCompletion.StatusCode
	}

	// Handle streaming response
	if rawResponse.ChatCompletionStream != nil {
		result, handleErr := processor.handleStreamResponse(ctx, rawResponse.ChatCompletionStream, startTime, streamVerdict)
		if result != nil && result.StatusCode == nil {
			result.StatusCode = &statusCode
		}
		return result, handleErr
	}

	latency := time.Since(startTime).Seconds()

	// Handle non-streaming response
	response, err := xjson.To[llm.Response](rawResponse.ChatCompletion.Body)
	if err != nil {
		return &TestChannelResult{
			Latency:    latency,
			Success:    false,
			Message:    new(""),
			Error:      new(err.Error()),
			StatusCode: &statusCode,
		}, nil
	}
	if terminal, successful := llmTerminalOutcome(&response); terminal && !successful {
		message, failureStatus := testChannelLLMFailure(&response)
		if failureStatus == nil {
			failureStatus = &statusCode
		}

		return &TestChannelResult{
			Latency:    latency,
			Success:    false,
			Message:    new(""),
			Error:      &message,
			StatusCode: failureStatus,
		}, nil
	}
	if len(response.Choices) == 0 {
		return &TestChannelResult{
			Latency:    latency,
			Success:    false,
			Message:    new(""),
			Error:      new("No message in response"),
			StatusCode: &statusCode,
		}, nil
	}

	message, hasMeaningfulOutput := testChannelNonStreamOutput(&response)
	if !hasMeaningfulOutput {
		return &TestChannelResult{
			Latency:    latency,
			Success:    false,
			Message:    new(""),
			Error:      new("No content in response"),
			StatusCode: &statusCode,
		}, nil
	}

	return &TestChannelResult{
		Latency:    latency,
		Success:    true,
		Message:    message,
		Error:      nil,
		StatusCode: &statusCode,
	}, nil
}

func testChannelNonStreamOutput(response *llm.Response) (*string, bool) {
	if response == nil {
		return nil, false
	}

	for _, choice := range response.Choices {
		message := choice.Message
		if message == nil {
			continue
		}
		if content := message.Content.Content; content != nil && strings.TrimSpace(*content) != "" {
			return content, true
		}
		if len(message.Content.MultipleContent) > 0 ||
			len(message.ToolCalls) > 0 ||
			strings.TrimSpace(lo.FromPtr(message.ReasoningContent)) != "" ||
			strings.TrimSpace(lo.FromPtr(message.Reasoning)) != "" ||
			strings.TrimSpace(lo.FromPtr(message.ReasoningSignature)) != "" ||
			strings.TrimSpace(message.Refusal) != "" ||
			message.Audio != nil {
			return message.Content.Content, true
		}
	}
	return nil, false
}

func testChannelLLMFailure(response *llm.Response) (string, *int) {
	if response != nil && response.Error != nil {
		statusCode := response.Error.StatusCode
		if statusCode == 0 {
			statusCode = http.StatusBadGateway
		}

		message := strings.TrimSpace(response.Error.Detail.Message)
		if message == "" {
			message = "Upstream response failed"
		}
		metadata := make([]string, 0, 2)
		if response.Error.Detail.Code != "" {
			metadata = append(metadata, "code: "+response.Error.Detail.Code)
		}
		if response.Error.Detail.Type != "" {
			metadata = append(metadata, "type: "+response.Error.Detail.Type)
		}
		if len(metadata) > 0 {
			message += " (" + strings.Join(metadata, ", ") + ")"
		}

		return message, &statusCode
	}

	if response != nil {
		for _, choice := range response.Choices {
			if choice.FinishReason != nil {
				statusCode := http.StatusBadGateway
				return "Upstream ended with finish_reason: " + *choice.FinishReason, &statusCode
			}
		}
	}

	statusCode := http.StatusBadGateway
	return "Upstream response failed", &statusCode
}

func testChannelHTTPError(rawErr *httpclient.Error, actualStatusCode int, cause error) (*int, string) {
	var statusCode *int
	if actualStatusCode > 0 {
		statusCode = new(actualStatusCode)
	}

	if rawErr != nil {
		for _, path := range []string{"error.message", "errors.0.message", "errors.message", "detail", "message", "error"} {
			if message, ok := testChannelSafeScalar(rawErr.Body, path, false); ok {
				if metadata := testChannelErrorMetadata(rawErr); len(metadata) > 0 {
					message += " (" + strings.Join(metadata, ", ") + ")"
				}
				return statusCode, message
			}
		}

		if metadata := testChannelErrorMetadata(rawErr); len(metadata) > 0 {
			return statusCode, "Upstream error (" + strings.Join(metadata, ", ") + ")"
		}
		if len(strings.TrimSpace(string(rawErr.Body))) > 0 {
			return statusCode, "Upstream returned an error without a public diagnostic message"
		}
	}
	if cause != nil {
		return statusCode, cause.Error()
	}

	return statusCode, "No upstream error detail was returned"
}

func testChannelSafeScalar(body []byte, path string, allowNumber bool) (string, bool) {
	result := gjson.GetBytes(body, path)
	if !result.Exists() {
		return "", false
	}
	if result.Type != gjson.String && !(allowNumber && result.Type == gjson.Number) {
		return "", false
	}

	value := strings.TrimSpace(result.String())
	return value, value != ""
}

func testChannelErrorMetadata(rawErr *httpclient.Error) []string {
	if rawErr == nil {
		return nil
	}

	fields := []struct {
		label string
		paths []string
	}{
		{label: "code", paths: []string{"error.code", "code", "detail.code"}},
		{label: "type", paths: []string{"error.type", "type", "detail.type"}},
		{label: "param", paths: []string{"error.param", "param", "detail.param"}},
	}
	metadata := make([]string, 0, len(fields))
	for _, field := range fields {
		for _, path := range field.paths {
			if value, ok := testChannelSafeScalar(rawErr.Body, path, true); ok {
				metadata = append(metadata, field.label+": "+value)
				break
			}
		}
	}
	return metadata
}

func testChannelModelErrorEvidence(rawErr *httpclient.Error, message string) string {
	evidence := []string{message}
	if rawErr != nil {
		for _, path := range []string{
			"error.code",
			"error.type",
			"error.param",
			"code",
			"type",
			"detail.code",
			"detail.type",
		} {
			if value, ok := testChannelSafeScalar(rawErr.Body, path, true); ok {
				evidence = append(evidence, value)
			}
		}
	}

	return strings.Join(evidence, " ")
}

func isExplicitUnsupportedTestModel(statusCode int, message string) bool {
	return isExplicitUnsupportedModel(statusCode, message)
}

// testChannelStreamVerdict captures the provider-side unified stream semantics
// before the inbound transformer can erase protocol-only fields such as a
// Responses API response.incomplete status.
type testChannelStreamVerdict struct {
	hasMeaningfulOutput bool
	terminal            bool
	successful          bool
	err                 error
}

type testChannelStreamVerdictMiddleware struct {
	pipeline.DummyMiddleware
	verdict *testChannelStreamVerdict
}

func (m *testChannelStreamVerdictMiddleware) Name() string {
	return "test-channel-stream-verdict"
}

func (m *testChannelStreamVerdictMiddleware) OnOutboundLlmStream(
	_ context.Context,
	stream streams.Stream[*llm.Response],
) (streams.Stream[*llm.Response], error) {
	return &testChannelVerdictStream{stream: stream, verdict: m.verdict}, nil
}

type testChannelVerdictStream struct {
	stream  streams.Stream[*llm.Response]
	verdict *testChannelStreamVerdict
	current *llm.Response
	err     error
}

func (s *testChannelVerdictStream) Next() bool {
	if s.err != nil {
		return false
	}
	if !s.stream.Next() {
		if sourceErr := s.stream.Err(); sourceErr != nil {
			s.err = sourceErr
			if s.verdict != nil {
				s.verdict.err = sourceErr
				s.verdict.successful = false
			}
			return false
		}
		if s.verdict == nil || !s.verdict.terminal || !s.verdict.successful {
			s.err = pipeline.ErrStreamIncomplete
			if s.verdict != nil {
				s.verdict.err = s.err
				s.verdict.successful = false
			}
		}
		return false
	}

	s.current = s.stream.Current()
	if s.verdict != nil && pipeline.HasResponseContent(s.current) {
		s.verdict.hasMeaningfulOutput = true
	}
	if outcome := pipeline.ResponseTerminalOutcome(s.current); outcome.Terminal {
		if s.verdict != nil {
			s.verdict.terminal = true
			s.verdict.successful = outcome.Successful
			s.verdict.err = outcome.Err
		}
		if !outcome.Successful {
			s.err = outcome.Err
			if s.err == nil {
				s.err = fmt.Errorf("upstream stream reached an unsuccessful terminal state")
			}
			return false
		}
	}

	return true
}

func (s *testChannelVerdictStream) Current() *llm.Response {
	return s.current
}

func (s *testChannelVerdictStream) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.stream.Err()
}

func (s *testChannelVerdictStream) Close() error {
	err := s.stream.Close()
	if err != nil && s.verdict != nil {
		s.verdict.err = err
		s.verdict.successful = false
	}
	return err
}

// handleStreamResponse processes a streaming response and accumulates the content.
func (processor *TestChannelOrchestrator) handleStreamResponse(
	ctx context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	startTime time.Time,
	providerVerdicts ...*testChannelStreamVerdict,
) (*TestChannelResult, error) {
	var accumulatedContent string
	hasMeaningfulOutput := false
	hasSuccessfulTerminal := false
	var terminalFailure *llm.Response
	var malformedEventErr error
	var canceledErr error

	for stream.Next() {
		select {
		case <-ctx.Done():
			canceledErr = ctx.Err()
			break
		default:
		}
		if canceledErr != nil {
			break
		}

		event := stream.Current()
		if event == nil {
			continue
		}

		// [DONE] is a valid Chat Completions terminal, but never sufficient by
		// itself: meaningful output is required too, and the provider-side verdict
		// wrapper above prevents a synthetic Responses [DONE] from hiding an
		// earlier response.incomplete/failed event.
		if strings.TrimSpace(string(event.Data)) == "[DONE]" {
			hasSuccessfulTerminal = true
			continue
		}

		var chunk llm.Response
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			log.Warn(ctx, "failed to unmarshal stream event data", log.Cause(err), log.ByteString("data", event.Data))
			if malformedEventErr == nil {
				malformedEventErr = fmt.Errorf("malformed stream event: %w", err)
			}
			continue
		}
		if outcome := pipeline.ResponseTerminalOutcome(&chunk); outcome.Terminal {
			if outcome.Successful {
				hasSuccessfulTerminal = true
			} else {
				terminalFailure = &chunk
			}
		}

		if pipeline.HasResponseContent(&chunk) {
			hasMeaningfulOutput = true
		}
		for _, choice := range chunk.Choices {
			delta := choice.Delta
			if delta == nil {
				continue
			}
			if delta.Content.Content != nil {
				accumulatedContent += *delta.Content.Content
			}
		}
	}

	latency := time.Since(startTime).Seconds()
	streamErr := stream.Err()
	closeErr := stream.Close()

	if canceledErr == nil {
		canceledErr = ctx.Err()
	}
	if canceledErr != nil {
		return &TestChannelResult{
			Latency: latency,
			Success: false,
			Message: lo.ToPtr(accumulatedContent),
			Error:   lo.ToPtr(canceledErr.Error()),
		}, nil
	}

	if streamErr != nil {
		actualStatusCode := ExtractStatusCodeFromError(streamErr)
		rawErr := openai.NewInboundTransformer().TransformError(ctx, streamErr)
		statusCode, message := testChannelHTTPError(rawErr, actualStatusCode, streamErr)
		return &TestChannelResult{
			Latency:    latency,
			Success:    false,
			Message:    lo.ToPtr(""),
			Error:      lo.ToPtr(message),
			StatusCode: statusCode,
			ModelUnsupported: isExplicitUnsupportedTestModel(
				actualStatusCode,
				testChannelModelErrorEvidence(rawErr, message),
			),
		}, nil
	}
	if closeErr != nil {
		return &TestChannelResult{
			Latency: latency,
			Success: false,
			Message: lo.ToPtr(accumulatedContent),
			Error:   lo.ToPtr(closeErr.Error()),
		}, nil
	}
	if len(providerVerdicts) > 0 && providerVerdicts[0] != nil {
		providerVerdict := providerVerdicts[0]
		if providerVerdict.err != nil {
			return &TestChannelResult{
				Latency: latency,
				Success: false,
				Message: lo.ToPtr(accumulatedContent),
				Error:   lo.ToPtr(providerVerdict.err.Error()),
			}, nil
		}
		if !providerVerdict.hasMeaningfulOutput || !providerVerdict.terminal || !providerVerdict.successful {
			return &TestChannelResult{
				Latency: latency,
				Success: false,
				Message: lo.ToPtr(accumulatedContent),
				Error:   lo.ToPtr("Provider stream did not produce meaningful output and a successful terminal event"),
			}, nil
		}
	}
	if terminalFailure != nil {
		message, statusCode := testChannelLLMFailure(terminalFailure)
		return &TestChannelResult{
			Latency:    latency,
			Success:    false,
			Message:    lo.ToPtr(accumulatedContent),
			Error:      &message,
			StatusCode: statusCode,
		}, nil
	}
	if malformedEventErr != nil {
		return &TestChannelResult{
			Latency: latency,
			Success: false,
			Message: lo.ToPtr(accumulatedContent),
			Error:   lo.ToPtr(malformedEventErr.Error()),
		}, nil
	}

	if !hasMeaningfulOutput {
		return &TestChannelResult{
			Latency: latency,
			Success: false,
			Message: lo.ToPtr(""),
			Error:   lo.ToPtr("No content in stream response"),
		}, nil
	}
	if !hasSuccessfulTerminal {
		return &TestChannelResult{
			Latency: latency,
			Success: false,
			Message: lo.ToPtr(accumulatedContent),
			Error:   lo.ToPtr("Stream ended without a successful terminal event"),
		}, nil
	}

	return &TestChannelResult{
		Latency: latency,
		Success: true,
		Message: lo.ToPtr(accumulatedContent),
		Error:   nil,
	}, nil
}

// TestAPIKeyResult represents the result of testing a single API key.
type TestAPIKeyResult struct {
	KeyPrefix string
	Success   bool
	Latency   float64
	Error     *string
	Disabled  bool
	routeKey  *biz.RouteKey
}

// TestChannelAPIKeysResult represents the aggregated result of testing all API keys.
type TestChannelAPIKeysResult struct {
	ChannelID    objects.GUID
	Total        int
	SuccessCount int
	FailedCount  int
	Results      []*TestAPIKeyResult
}

// TestChannelAPIKeys tests all API keys for a specific channel individually.
func (processor *TestChannelOrchestrator) TestChannelAPIKeys(
	ctx context.Context,
	channelID objects.GUID,
	modelID *string,
	proxy *httpclient.ProxyConfig,
) (*TestChannelAPIKeysResult, error) {
	ch, err := processor.channelService.GetChannel(ctx, channelID.ID)
	if err != nil {
		return nil, err
	}

	allKeys := ch.Credentials.GetAllAPIKeys()
	if len(allKeys) == 0 {
		return nil, fmt.Errorf("no API keys configured for channel")
	}

	// Build disabled set
	disabledSet := make(map[string]struct{}, len(ch.DisabledAPIKeys))
	for _, dk := range ch.DisabledAPIKeys {
		disabledSet[dk.Key] = struct{}{}
	}

	testModel := lo.FromPtr(modelID)
	if testModel == "" {
		testModel = ch.DefaultTestModel
	}

	useStream := ch.Policies.Stream == objects.CapabilityPolicyRequire

	results := make([]*TestAPIKeyResult, len(allKeys))

	var (
		successCount int32
		failedCount  int32
	)

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(min(testChannelAPIKeysMaxConcurrency, len(allKeys)))

	for i, key := range allKeys {
		index := i
		apiKey := key

		group.Go(func() error {
			select {
			case <-groupCtx.Done():
				errMsg := groupCtx.Err().Error()
				results[index] = &TestAPIKeyResult{
					KeyPrefix: maskAPIKey(apiKey),
					Success:   false,
					Error:     &errMsg,
				}

				atomic.AddInt32(&failedCount, 1)

				return nil
			default:
			}

			result := processor.testSingleKey(groupCtx, channelID, apiKey, testModel, useStream, proxy, nil, true)
			_, isDisabled := disabledSet[apiKey]
			result.Disabled = isDisabled
			results[index] = result

			if result.Success {
				atomic.AddInt32(&successCount, 1)
				return nil
			}

			atomic.AddInt32(&failedCount, 1)

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return &TestChannelAPIKeysResult{
		ChannelID:    channelID,
		Total:        len(allKeys),
		SuccessCount: int(successCount),
		FailedCount:  int(failedCount),
		Results:      results,
	}, nil
}

// TestSingleAPIKey tests a single API key for a channel.
// It verifies that the provided key belongs to the channel before testing.
func (processor *TestChannelOrchestrator) TestSingleAPIKey(
	ctx context.Context,
	channelID objects.GUID,
	key string,
	modelID *string,
	proxy *httpclient.ProxyConfig,
) (result *TestAPIKeyResult, resultErr error) {
	return processor.testSingleAPIKey(ctx, channelID, key, modelID, proxy, nil, true)
}

// TestSingleAPIKeyRoute verifies one exact credential/model/protocol route.
func (processor *TestChannelOrchestrator) TestSingleAPIKeyRoute(
	ctx context.Context,
	channelID objects.GUID,
	key string,
	modelID *string,
	proxy *httpclient.ProxyConfig,
	route biz.RouteKey,
) (result *TestAPIKeyResult, resultErr error) {
	channel, err := processor.channelService.GetChannel(ctx, channelID.ID)
	if err != nil {
		return nil, err
	}
	if route.ChannelID != channelID.ID || route.CredentialID != biz.RouteCredentialFingerprint(channel, key) {
		return nil, fmt.Errorf("credential route does not match requested channel/key")
	}
	// The surrounding TriggerTest call owns the route verdict generation/write.
	return processor.testSingleAPIKey(ctx, channelID, key, modelID, proxy, &route, false)
}

func (processor *TestChannelOrchestrator) testSingleAPIKey(
	ctx context.Context,
	channelID objects.GUID,
	key string,
	modelID *string,
	proxy *httpclient.ProxyConfig,
	forcedRoute *biz.RouteKey,
	recordVerdict bool,
) (result *TestAPIKeyResult, resultErr error) {
	ch, err := processor.channelService.GetChannel(ctx, channelID.ID)
	if err != nil {
		return nil, err
	}

	// Verify the provided key is actually configured for this channel.
	channelKeys := ch.Credentials.GetAllAPIKeys()
	if len(channelKeys) == 0 {
		return nil, fmt.Errorf("no API keys configured for channel")
	}

	keyBelongsToChannel := lo.Contains(channelKeys, key)
	if !keyBelongsToChannel {
		return nil, fmt.Errorf("the provided API key is not configured for this channel")
	}

	testModel := lo.FromPtr(modelID)
	if testModel == "" {
		testModel = ch.DefaultTestModel
	}
	useStream := ch.Policies.Stream == objects.CapabilityPolicyRequire

	disabledSet := make(map[string]struct{}, len(ch.DisabledAPIKeys))
	for _, dk := range ch.DisabledAPIKeys {
		disabledSet[dk.Key] = struct{}{}
	}

	result = processor.testSingleKey(ctx, channelID, key, testModel, useStream, proxy, forcedRoute, recordVerdict)
	_, isDisabled := disabledSet[key]
	result.Disabled = isDisabled

	return result, nil
}

func (processor *TestChannelOrchestrator) recordCompletedTestVerdict(
	ctx context.Context,
	generation uint64,
	key *biz.RouteKey,
	result *TestChannelResult,
	err error,
) {
	if err != nil || result == nil || ctx.Err() != nil || key == nil || key.ChannelID <= 0 {
		return
	}
	detail := ""
	if result.Error != nil {
		detail = biz.SanitizeCampusDiagnosticError(*result.Error)
	}
	processor.channelService.UnifiedRouteState().RecordTestVerdictGeneration(*key, generation, biz.RouteTestVerdict{
		Completed: true,
		Pass:      result.Success,
		Error:     detail,
	})
}

// testSingleKey tests a single API key by forcing the use of a specific key via SetAPIKey.
func (processor *TestChannelOrchestrator) testSingleKey(
	ctx context.Context,
	channelID objects.GUID,
	key string,
	testModel string,
	useStream bool,
	proxy *httpclient.ProxyConfig,
	forcedRoute *biz.RouteKey,
	recordVerdict bool,
) (result *TestAPIKeyResult) {
	ctx = contexts.WithIsolatedContainer(ctx)
	var testGeneration uint64
	if recordVerdict {
		testGeneration = processor.channelService.UnifiedRouteState().NextTestGeneration()
	}
	keyPrefix := maskAPIKey(key)
	var routeKey *biz.RouteKey
	defer func() {
		if result != nil {
			result.routeKey = routeKey
		}
		if !recordVerdict || result == nil || routeKey == nil || ctx.Err() != nil {
			return
		}
		detail := ""
		if result.Error != nil {
			detail = biz.SanitizeCampusDiagnosticError(*result.Error)
		}
		processor.channelService.UnifiedRouteState().RecordTestVerdictGeneration(*routeKey, testGeneration, biz.RouteTestVerdict{
			Completed: true,
			Pass:      result.Success,
			Error:     detail,
		})
	}()

	inbound := openai.NewInboundTransformer()
	streamVerdict := &testChannelStreamVerdict{}
	var promptProtecter PromptProtecter
	if processor.promptProtectionRuleService != nil {
		promptProtecter = processor.promptProtectionRuleService
	}

	selector := &SpecifiedChannelSelector{
		ChannelService: processor.channelService,
		ChannelID:      channelID,
		SelectedAPIKey: key,
	}
	if forcedRoute != nil {
		selector.ForcedAPIFormat = forcedRoute.APIFormat
		selector.ForcedActualModel = forcedRoute.ActualModel
	}

	chatProcessor := &ChatCompletionOrchestrator{
		channelSelector: selector,
		RequestService:  processor.requestService,
		ChannelService:  processor.channelService,
		PromptProvider:  &stubPromptProvider{},
		PromptProtecter: promptProtecter,
		PipelineFactory: pipeline.NewFactory(processor.httpClient),
		Middlewares: []pipeline.Middleware{
			stream.EnsureUsage(),
			&testChannelStreamVerdictMiddleware{verdict: streamVerdict},
		},
		Inbound:               inbound,
		SystemService:         processor.systemService,
		UsageLogService:       processor.usageLogService,
		proxy:                 proxy,
		ModelMapper:           processor.modelMapper,
		channelLimiterManager: processor.channelLimiterManager,
		singleRouteOnly:       true,
	}

	llmRequest := &llm.Request{
		Model: testModel,
		Messages: []llm.Message{
			{
				Role: "system",
				Content: llm.MessageContent{
					Content: new("You are a helpful assistant."),
				},
			},
			{
				Role: "user",
				Content: llm.MessageContent{
					MultipleContent: []llm.MessageContentPart{
						{
							Type: "text",
							Text: new("Hello world, I'm AxonHub."),
						},
						{
							Type: "text",
							Text: new("Please tell me who you are?"),
						},
					},
				},
			},
		},
		MaxCompletionTokens: new(int64(256)),
		Stream:              new(useStream),
	}

	body, err := json.Marshal(llmRequest)
	if err != nil {
		errMsg := err.Error()

		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   false,
			Error:     &errMsg,
		}
	}

	startTime := time.Now()

	rawResponse, err := chatProcessor.Process(ctx, &httpclient.Request{
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: body,
	})
	routeKey = rawResponse.RouteKey
	if err != nil {
		rawErr := inbound.TransformError(ctx, err)
		_, message := testChannelHTTPError(rawErr, ExtractStatusCodeFromError(err), err)

		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   false,
			Latency:   time.Since(startTime).Seconds(),
			Error:     new(message),
		}
	}

	// Handle streaming response
	if rawResponse.ChatCompletionStream != nil {
		streamResult, _ := processor.handleStreamResponse(ctx, rawResponse.ChatCompletionStream, startTime, streamVerdict)

		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   streamResult.Success,
			Latency:   streamResult.Latency,
			Error:     streamResult.Error,
		}
	}

	latency := time.Since(startTime).Seconds()

	// Handle non-streaming response
	response, err := xjson.To[llm.Response](rawResponse.ChatCompletion.Body)
	if err != nil {
		errMsg := err.Error()

		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   false,
			Latency:   latency,
			Error:     &errMsg,
		}
	}
	if terminal, successful := llmTerminalOutcome(&response); terminal && !successful {
		message, _ := testChannelLLMFailure(&response)
		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   false,
			Latency:   latency,
			Error:     &message,
		}
	}

	if len(response.Choices) == 0 {
		errMsg := "No message in response"

		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   false,
			Latency:   latency,
			Error:     &errMsg,
		}
	}
	if _, meaningful := testChannelNonStreamOutput(&response); !meaningful {
		errMsg := "No content in response"
		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   false,
			Latency:   latency,
			Error:     &errMsg,
		}
	}

	return &TestAPIKeyResult{
		KeyPrefix: keyPrefix,
		Success:   true,
		Latency:   latency,
	}
}

// maskAPIKey returns a masked version of the API key for display.
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}

	return key[:4] + "****" + key[len(key)-4:]
}
