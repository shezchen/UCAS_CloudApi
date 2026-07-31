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
	modelCircuitBreaker         *biz.ModelCircuitBreaker
	modelMapper                 *ModelMapper
	loadBalancer                *LoadBalancer
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
		modelCircuitBreaker:         biz.NewModelCircuitBreaker(),
		modelMapper:                 NewModelMapper(),
		loadBalancer:                NewLoadBalancer(systemService, channelService, NewWeightStrategy()),
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
}

// TestChannel tests a specific channel with a simple request.
func (processor *TestChannelOrchestrator) TestChannel(
	ctx context.Context,
	channelID objects.GUID,
	modelID *string,
	proxy *httpclient.ProxyConfig,
) (*TestChannelResult, error) {
	inbound := openai.NewInboundTransformer()
	// Create ChatCompletionOrchestrator for this test request
	chatProcessor := &ChatCompletionOrchestrator{
		channelSelector: NewSpecifiedChannelSelector(processor.channelService, channelID),
		RequestService:  processor.requestService,
		ChannelService:  processor.channelService,
		PromptProvider:  &stubPromptProvider{},
		PromptProtecter: processor.promptProtectionRuleService,
		PipelineFactory: pipeline.NewFactory(processor.httpClient),
		Middlewares: []pipeline.Middleware{
			stream.EnsureUsage(),
		},
		Inbound:                    inbound,
		SystemService:              processor.systemService,
		UsageLogService:            processor.usageLogService,
		proxy:                      proxy,
		ModelMapper:                processor.modelMapper,
		adaptiveLoadBalancer:       processor.loadBalancer,
		failoverLoadBalancer:       processor.loadBalancer,
		circuitBreakerLoadBalancer: processor.loadBalancer,
		channelLimiterManager:      processor.channelLimiterManager,
		modelCircuitBreaker:        processor.modelCircuitBreaker,
	}

	channel, err := processor.channelService.GetChannel(ctx, channelID.ID)
	if err != nil {
		return nil, err
	}

	testModel := lo.FromPtr(modelID)
	if testModel == "" {
		testModel = channel.DefaultTestModel
	}

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
		result, handleErr := processor.handleStreamResponse(ctx, rawResponse.ChatCompletionStream, startTime)
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

// handleStreamResponse processes a streaming response and accumulates the content.
func (processor *TestChannelOrchestrator) handleStreamResponse(
	ctx context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	startTime time.Time,
) (*TestChannelResult, error) {
	defer func() {
		_ = stream.Close()
	}()

	// Accumulate stream chunks
	var accumulatedContent string
	hasMeaningfulOutput := false

	for stream.Next() {
		select {
		case <-ctx.Done():
			return &TestChannelResult{
				Latency: time.Since(startTime).Seconds(),
				Success: false,
				Message: lo.ToPtr(accumulatedContent),
				Error:   lo.ToPtr(ctx.Err().Error()),
			}, nil
		default:
		}

		event := stream.Current()
		if event == nil {
			continue
		}

		// The stream may end with a "[DONE]" message which is not valid JSON.
		if string(event.Data) == "[DONE]" {
			continue
		}

		// Parse the stream event data
		var chunk llm.Response
		if err := json.Unmarshal(event.Data, &chunk); err != nil {
			log.Warn(ctx, "failed to unmarshal stream event data", log.Cause(err), log.ByteString("data", event.Data))
			continue
		}

		// Accumulate content from the first choice
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil {
			delta := chunk.Choices[0].Delta
			if delta.Content.Content != nil {
				accumulatedContent += *delta.Content.Content
				if strings.TrimSpace(*delta.Content.Content) != "" {
					hasMeaningfulOutput = true
				}
			}
			if len(delta.ToolCalls) > 0 ||
				strings.TrimSpace(lo.FromPtr(delta.ReasoningContent)) != "" ||
				strings.TrimSpace(delta.Refusal) != "" {
				hasMeaningfulOutput = true
			}
		}
	}

	// Calculate latency after processing all stream events
	latency := time.Since(startTime).Seconds()

	if err := ctx.Err(); err != nil {
		return &TestChannelResult{
			Latency: latency,
			Success: false,
			Message: lo.ToPtr(accumulatedContent),
			Error:   lo.ToPtr(err.Error()),
		}, nil
	}

	if streamErr := stream.Err(); streamErr != nil {
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

	if !hasMeaningfulOutput {
		return &TestChannelResult{
			Latency: latency,
			Success: false,
			Message: lo.ToPtr(""),
			Error:   lo.ToPtr("No content in stream response"),
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

			result := processor.testSingleKey(groupCtx, channelID, apiKey, testModel, useStream, proxy)
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
) (*TestAPIKeyResult, error) {
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

	result := processor.testSingleKey(ctx, channelID, key, testModel, useStream, proxy)
	_, isDisabled := disabledSet[key]
	result.Disabled = isDisabled

	return result, nil
}

// testSingleKey tests a single API key by forcing the use of a specific key via SetAPIKey.
func (processor *TestChannelOrchestrator) testSingleKey(
	ctx context.Context,
	channelID objects.GUID,
	key string,
	testModel string,
	useStream bool,
	proxy *httpclient.ProxyConfig,
) *TestAPIKeyResult {
	keyPrefix := maskAPIKey(key)

	inbound := openai.NewInboundTransformer()

	chatProcessor := &ChatCompletionOrchestrator{
		channelSelector: &SpecifiedChannelSelector{
			ChannelService: processor.channelService,
			ChannelID:      channelID,
			SelectedAPIKey: key,
		},
		RequestService:  processor.requestService,
		ChannelService:  processor.channelService,
		PromptProvider:  &stubPromptProvider{},
		PromptProtecter: processor.promptProtectionRuleService,
		PipelineFactory: pipeline.NewFactory(processor.httpClient),
		Middlewares: []pipeline.Middleware{
			stream.EnsureUsage(),
		},
		Inbound:                    inbound,
		SystemService:              processor.systemService,
		UsageLogService:            processor.usageLogService,
		proxy:                      proxy,
		ModelMapper:                processor.modelMapper,
		adaptiveLoadBalancer:       processor.loadBalancer,
		failoverLoadBalancer:       processor.loadBalancer,
		circuitBreakerLoadBalancer: processor.loadBalancer,
		channelLimiterManager:      processor.channelLimiterManager,
		modelCircuitBreaker:        processor.modelCircuitBreaker,
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
	if err != nil {
		rawErr := inbound.TransformError(ctx, err)
		message := gjson.GetBytes(rawErr.Body, "error.message").String()

		return &TestAPIKeyResult{
			KeyPrefix: keyPrefix,
			Success:   false,
			Latency:   time.Since(startTime).Seconds(),
			Error:     new(message),
		}
	}

	// Handle streaming response
	if rawResponse.ChatCompletionStream != nil {
		streamResult, _ := processor.handleStreamResponse(ctx, rawResponse.ChatCompletionStream, startTime)

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

	if len(response.Choices) == 0 {
		errMsg := "No message in response"

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
