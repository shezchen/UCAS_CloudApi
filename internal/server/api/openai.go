package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/contexts"
	entprivacy "github.com/looplj/axonhub/internal/ent/privacy"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

type OpenAIHandlersParams struct {
	fx.In

	VideoService                *biz.VideoService
	ChannelService              *biz.ChannelService
	ModelService                *biz.ModelService
	DefaultSelector             *orchestrator.DefaultSelector
	RequestService              *biz.RequestService
	SystemService               *biz.SystemService
	UsageLogService             *biz.UsageLogService
	PromptService               *biz.PromptService
	PromptProtectionRuleService *biz.PromptProtectionRuleService
	QuotaService                *biz.QuotaService
	HttpClient                  *httpclient.HttpClient
	LiveStreamRegistry          *biz.LiveStreamRegistry
	ChannelLimiterManager       *orchestrator.ChannelLimiterManager
	ProviderQuotaStatusProvider orchestrator.ProviderQuotaStatusProvider
}

type OpenAIHandlers struct {
	ChannelService             *biz.ChannelService
	ModelService               *biz.ModelService
	SystemService              *biz.SystemService
	VideoService               *biz.VideoService
	ChatCompletionHandlers     *ChatCompletionHandlers
	CompletionHandlers         *ChatCompletionHandlers
	ResponseCompletionHandlers *ChatCompletionHandlers
	CompactHandlers            *ChatCompletionHandlers
	EmbeddingHandlers          *ChatCompletionHandlers
	ImageGenerationHandlers    *ChatCompletionHandlers
	ImageEditHandlers          *ChatCompletionHandlers
	ImageVariationHandlers     *ChatCompletionHandlers
	VideoHandlers              *ChatCompletionHandlers
	VideoInboundTransformer    *openai.VideoInboundTransformer
	SpeechHandlers             *ChatCompletionHandlers
	TranscriptionHandlers      *ChatCompletionHandlers
	TranslationHandlers        *ChatCompletionHandlers
	SpeechInboundTransformer   *openai.AudioInboundTransformer
}

type speechRouteRequestBody struct {
	StreamFormat string `json:"stream_format"`
}

func NewOpenAIHandlers(params OpenAIHandlersParams) *OpenAIHandlers {
	videoInbound := openai.NewVideoInboundTransformer()
	speechInbound := openai.NewSpeechInboundTransformer()

	return &OpenAIHandlers{
		ChatCompletionHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				openai.NewInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		CompletionHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				openai.NewCompletionInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		ResponseCompletionHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				responses.NewInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		CompactHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				responses.NewCompactInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		EmbeddingHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				openai.NewEmbeddingInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		ImageGenerationHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				openai.NewImageGenerationInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		ImageEditHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				openai.NewImageEditInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		ImageVariationHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				openai.NewImageVariationInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		VideoHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				videoInbound,
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		VideoInboundTransformer: videoInbound,
		VideoService:            params.VideoService,
		ChannelService:          params.ChannelService,
		ModelService:            params.ModelService,
		SystemService:           params.SystemService,
		SpeechHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				speechInbound,
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		SpeechInboundTransformer: speechInbound,
		TranscriptionHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				openai.NewTranscriptionInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
		TranslationHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				openai.NewTranslationInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		},
	}
}

func (handlers *OpenAIHandlers) ChatCompletion(c *gin.Context) {
	handlers.ChatCompletionHandlers.ChatCompletion(c)
}

func (handlers *OpenAIHandlers) Completion(c *gin.Context) {
	handlers.CompletionHandlers.ChatCompletion(c)
}

func (handlers *OpenAIHandlers) CreateResponse(c *gin.Context) {
	handlers.ResponseCompletionHandlers.ChatCompletion(c)
}

func (handlers *OpenAIHandlers) CompactResponse(c *gin.Context) {
	handlers.CompactHandlers.ChatCompletion(c)
}

func (handlers *OpenAIHandlers) CreateEmbedding(c *gin.Context) {
	handlers.EmbeddingHandlers.ChatCompletion(c)
}

// CreateSpeech handles POST /v1/audio/speech (text-to-speech). The response is binary audio.
func (handlers *OpenAIHandlers) CreateSpeech(c *gin.Context) {
	ctx := c.Request.Context()

	genericReq, err := httpclient.ReadHTTPRequest(c.Request)
	if err != nil {
		httpErr := handlers.SpeechHandlers.ChatCompletionOrchestrator.Inbound.TransformError(ctx, err)
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))
		return
	}

	useBinaryStream, err := shouldUseBinarySpeechStream(genericReq)
	if err != nil {
		httpErr := handlers.SpeechHandlers.ChatCompletionOrchestrator.Inbound.TransformError(ctx, err)
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))
		return
	}

	if !useBinaryStream {
		handlers.SpeechHandlers.ChatCompletionWithRequest(c, genericReq)
		return
	}

	handlers.SpeechHandlers.WithStreamWriter(WriteBinaryStream).ChatCompletionWithRequest(c, genericReq)
}

func shouldUseBinarySpeechStream(genericReq *httpclient.Request) (bool, error) {
	if genericReq == nil {
		return false, fmt.Errorf("%w: http request is nil", transformer.ErrInvalidRequest)
	}

	if len(genericReq.Body) == 0 {
		return false, fmt.Errorf("%w: request body is empty", transformer.ErrInvalidRequest)
	}

	contentType := strings.ToLower(genericReq.Headers.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "application/json") {
		return false, nil
	}

	var body speechRouteRequestBody
	if err := json.Unmarshal(genericReq.Body, &body); err != nil {
		return false, fmt.Errorf("%w: failed to decode speech request: %w", transformer.ErrInvalidRequest, err)
	}

	streamFormat := strings.ToLower(strings.TrimSpace(body.StreamFormat))

	return streamFormat != "" && streamFormat != "sse", nil
}

// CreateTranscription handles POST /v1/audio/transcriptions (speech-to-text).
func (handlers *OpenAIHandlers) CreateTranscription(c *gin.Context) {
	handlers.TranscriptionHandlers.ChatCompletion(c)
}

// CreateTranslation handles POST /v1/audio/translations (speech-to-text translation).
func (handlers *OpenAIHandlers) CreateTranslation(c *gin.Context) {
	handlers.TranslationHandlers.ChatCompletion(c)
}

func (handlers *OpenAIHandlers) CreateImage(c *gin.Context) {
	handlers.ImageGenerationHandlers.ChatCompletion(c)
}

func (handlers *OpenAIHandlers) CreateImageEdit(c *gin.Context) {
	handlers.ImageEditHandlers.ChatCompletion(c)
}

func (handlers *OpenAIHandlers) CreateImageVariation(c *gin.Context) {
	handlers.ImageVariationHandlers.ChatCompletion(c)
}

func (handlers *OpenAIHandlers) CreateVideo(c *gin.Context) {
	ctx := c.Request.Context()

	genericReq, err := httpclient.ReadHTTPRequest(c.Request)
	if err != nil {
		httpErr := handlers.VideoHandlers.ChatCompletionOrchestrator.Inbound.TransformError(ctx, err)
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))
		return
	}

	if len(genericReq.Body) == 0 {
		JSONError(c, http.StatusBadRequest, errors.New("Request body is empty"))
		return
	}

	result, err := handlers.VideoHandlers.ChatCompletionOrchestrator.Process(ctx, genericReq)
	if err != nil {
		log.Error(ctx, "Error processing openai video create", log.Cause(err))

		httpErr := transformOrchestratorError(ctx, err, handlers.VideoHandlers.ChatCompletionOrchestrator)
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))
		return
	}

	if result.ChatCompletion == nil {
		JSONError(c, http.StatusInternalServerError, biz.ErrInternal)
		return
	}

	resp := result.ChatCompletion
	contentType := "application/json"
	if ct := resp.Headers.Get("Content-Type"); ct != "" {
		contentType = ct
	}
	c.Data(resp.StatusCode, contentType, resp.Body)
}

func (handlers *OpenAIHandlers) GetVideo(c *gin.Context) {
	ctx := c.Request.Context()

	externalID := c.Param("id")
	if externalID == "" {
		JSONError(c, http.StatusBadRequest, errors.New("invalid id"))
		return
	}

	resp, err := handlers.VideoService.GetTaskByExternalID(ctx, externalID)
	if err != nil {
		JSONError(c, http.StatusInternalServerError, err)
		return
	}

	resp.Object = "video"
	resp.APIFormat = llm.APIFormatOpenAIVideo
	resp.Choices = []llm.Choice{}

	httpResp, err := handlers.VideoInboundTransformer.TransformResponse(ctx, resp)
	if err != nil {
		JSONError(c, http.StatusInternalServerError, err)
		return
	}

	contentType := "application/json"
	if ct := httpResp.Headers.Get("Content-Type"); ct != "" {
		contentType = ct
	}
	c.Data(httpResp.StatusCode, contentType, httpResp.Body)
}

func (handlers *OpenAIHandlers) DeleteVideo(c *gin.Context) {
	ctx := c.Request.Context()

	externalID := c.Param("id")
	if externalID == "" {
		JSONError(c, http.StatusBadRequest, errors.New("invalid id"))
		return
	}

	if err := handlers.VideoService.DeleteTaskByExternalID(ctx, externalID); err != nil {
		JSONError(c, http.StatusInternalServerError, err)
		return
	}

	c.Status(http.StatusNoContent)
}

type Modalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type Capabilities struct {
	Vision    bool `json:"vision"`
	ToolCall  bool `json:"tool_call"`
	Reasoning bool `json:"reasoning"`
}

type ReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values,omitempty"`
}

type Pricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Unit       string  `json:"unit"`
	Currency   string  `json:"currency"`
}

type OpenAIModel struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Created          int64             `json:"created"`
	OwnedBy          string            `json:"owned_by"`
	Name             string            `json:"name,omitempty"`
	Description      string            `json:"description,omitempty"`
	ContextLength    int               `json:"context_length,omitempty"`
	MaxOutputTokens  int               `json:"max_output_tokens,omitempty"`
	Modalities       *Modalities       `json:"modalities,omitempty"`
	Capabilities     *Capabilities     `json:"capabilities,omitempty"`
	ReasoningOptions []ReasoningOption `json:"reasoning_options,omitempty"`
	Pricing          *Pricing          `json:"pricing,omitempty"`
	Icon             string            `json:"icon,omitempty"`
	Type             string            `json:"type,omitempty"`
}

const (
	openAIModelObjectType         = "model"
	openAIErrorCodeInternalServer = "internal_server_error"
	openAIErrorCodeModelNotFound  = "model_not_found"
	openAIErrorTypeServer         = "server_error"
	openAIErrorTypeInvalidRequest = "invalid_request_error"
	openAIErrorParamModel         = "model"
)

var advertisedReasoningEfforts = [...]string{"low", "medium", "high", "xhigh", "max"}

func parseOpenAIModelInclude(includeParam string, defaultIncludeAll bool) (map[string]bool, bool) {
	var (
		include      map[string]bool
		needFullData bool
	)

	if includeParam == "" {
		return nil, defaultIncludeAll
	}

	if includeParam == "all" {
		return nil, true
	}

	fields := strings.Split(includeParam, ",")
	include = make(map[string]bool)
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			include[field] = true
		}
	}

	extendedFields := []string{"name", "description", "context_length", "max_output_tokens", "modalities", "capabilities", "reasoning_options", "pricing", "icon", "type"}
	for _, field := range extendedFields {
		if include[field] {
			needFullData = true
			break
		}
	}

	return include, needFullData
}

func convertModelFacadeToOpenAIModel(m biz.ModelFacade) OpenAIModel {
	return OpenAIModel{
		ID:      m.ID,
		Object:  openAIModelObjectType,
		Created: m.Created,
		OwnedBy: m.OwnedBy,
	}
}

// convertModelFacadeToOpenAIExtended renders the already-resolved effective
// metadata. This works for both explicit owner Model entities and dynamic
// channel models, so API handlers do not need a second database query.
func convertModelFacadeToOpenAIExtended(m biz.ModelFacade, include map[string]bool) OpenAIModel {
	result := convertModelFacadeToOpenAIModel(m)
	metadata := m.Metadata
	if metadata == nil {
		return result
	}

	shouldInclude := func(field string) bool {
		return include == nil || include[field]
	}
	if metadata.Developer != nil {
		result.OwnedBy = *metadata.Developer
	}
	if shouldInclude("name") && metadata.Name != nil {
		result.Name = *metadata.Name
	}
	if shouldInclude("description") && metadata.Description != nil {
		result.Description = *metadata.Description
	}
	if shouldInclude("icon") && metadata.Icon != nil {
		result.Icon = *metadata.Icon
	}
	if shouldInclude("type") && metadata.Type != nil {
		result.Type = *metadata.Type
	}

	if shouldInclude("modalities") && metadata.Modalities != nil {
		input := []string{}
		if metadata.Modalities.Input != nil {
			input = append(input, (*metadata.Modalities.Input)...)
		}
		output := []string{}
		if metadata.Modalities.Output != nil {
			output = append(output, (*metadata.Modalities.Output)...)
		}
		result.Modalities = &Modalities{Input: input, Output: output}
	}
	if shouldInclude("capabilities") {
		hasCapabilities := metadata.Vision != nil || metadata.ToolCall != nil ||
			(metadata.Reasoning != nil && metadata.Reasoning.Supported != nil)
		if hasCapabilities {
			capabilities := &Capabilities{}
			if metadata.Vision != nil {
				capabilities.Vision = *metadata.Vision
			}
			if metadata.ToolCall != nil {
				capabilities.ToolCall = *metadata.ToolCall
			}
			if metadata.Reasoning != nil && metadata.Reasoning.Supported != nil {
				capabilities.Reasoning = *metadata.Reasoning.Supported
			}
			result.Capabilities = capabilities
		}
	}
	if shouldInclude("reasoning_options") && metadata.Reasoning != nil &&
		metadata.Reasoning.Supported != nil && *metadata.Reasoning.Supported {
		result.ReasoningOptions = []ReasoningOption{{
			Type:   "effort",
			Values: append([]string(nil), advertisedReasoningEfforts[:]...),
		}}
	}
	if metadata.Limit != nil {
		if shouldInclude("context_length") && metadata.Limit.Context != nil {
			result.ContextLength = *metadata.Limit.Context
		}
		if shouldInclude("max_output_tokens") && metadata.Limit.Output != nil {
			result.MaxOutputTokens = *metadata.Limit.Output
		}
	}
	if shouldInclude("pricing") && metadata.Cost != nil {
		pricing := &Pricing{Unit: "per_1m_tokens", Currency: "USD"}
		if metadata.Cost.Input != nil {
			pricing.Input = *metadata.Cost.Input
		}
		if metadata.Cost.Output != nil {
			pricing.Output = *metadata.Cost.Output
		}
		if metadata.Cost.CacheRead != nil {
			pricing.CacheRead = *metadata.Cost.CacheRead
		}
		if metadata.Cost.CacheWrite != nil {
			pricing.CacheWrite = *metadata.Cost.CacheWrite
		}
		result.Pricing = pricing
	}

	return result
}

func (handlers *OpenAIHandlers) writeOpenAIInternalError(c *gin.Context, requestID string, err error) {
	_ = c.Error(err)

	c.JSON(http.StatusInternalServerError, openai.OpenAIError{
		StatusCode: http.StatusInternalServerError,
		Detail: llm.ErrorDetail{
			Code:      openAIErrorCodeInternalServer,
			Message:   err.Error(),
			Type:      openAIErrorTypeServer,
			RequestID: requestID,
		},
	})
}

func (handlers *OpenAIHandlers) writeOpenAIPermissionDeniedError(c *gin.Context, requestID string, err error) {
	_ = c.Error(err)

	c.JSON(http.StatusForbidden, openai.OpenAIError{
		StatusCode: http.StatusForbidden,
		Detail: llm.ErrorDetail{
			Code:      "permission_denied",
			Message:   "insufficient permissions to access this resource",
			Type:      "authentication_error",
			RequestID: requestID,
		},
	})
}

func (handlers *OpenAIHandlers) writeOpenAIModelNotFoundError(c *gin.Context, requestID, modelID string) {
	message := "The model does not exist or you do not have access to it."
	if modelID != "" {
		message = fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", modelID)
	}

	c.JSON(http.StatusNotFound, openai.OpenAIError{
		StatusCode: http.StatusNotFound,
		Detail: llm.ErrorDetail{
			Code:      openAIErrorCodeModelNotFound,
			Message:   message,
			Type:      openAIErrorTypeInvalidRequest,
			Param:     openAIErrorParamModel,
			RequestID: requestID,
		},
	})
}

// RetrieveModel returns a single available model.
// This endpoint is compatible with OpenAI's /v1/models/{model} API.
func (handlers *OpenAIHandlers) RetrieveModel(c *gin.Context) {
	ctx := c.Request.Context()

	requestID, _ := contexts.GetRequestID(ctx)
	modelID := strings.TrimPrefix(c.Param("model"), "/")
	if modelID == "" {
		handlers.writeOpenAIModelNotFoundError(c, requestID, "")
		return
	}

	include, needFullData := parseOpenAIModelInclude(c.Query("include"), false)

	models, err := handlers.ModelService.ListEnabledModels(ctx)
	if err != nil {
		if errors.Is(err, entprivacy.Deny) {
			handlers.writeOpenAIPermissionDeniedError(c, requestID, err)
		} else {
			handlers.writeOpenAIInternalError(c, requestID, err)
		}
		return
	}

	visibleModel, found := lo.Find(models, func(m biz.ModelFacade) bool {
		return m.ID == modelID
	})
	if !found {
		handlers.writeOpenAIModelNotFoundError(c, requestID, modelID)
		return
	}

	if !needFullData {
		c.JSON(http.StatusOK, convertModelFacadeToOpenAIModel(visibleModel))
		return
	}
	displayModels, err := handlers.ModelService.OverlayConfiguredModelFacadesForDisplay(ctx, []biz.ModelFacade{visibleModel})
	if err != nil {
		handlers.writeOpenAIInternalError(c, requestID, err)
		return
	}

	c.JSON(http.StatusOK, convertModelFacadeToOpenAIExtended(displayModels[0], include))
}

// ListModels returns all available models.
// This endpoint is compatible with OpenAI's /v1/models API.
// It uses QueryAllChannelModels setting from system config to determine model source.
func (handlers *OpenAIHandlers) ListModels(c *gin.Context) {
	ctx := c.Request.Context()

	requestID, _ := contexts.GetRequestID(ctx)

	include, needFullData := parseOpenAIModelInclude(c.Query("include"), handlers.SystemService.ModelSettingsOrDefault(ctx).DefaultModelAPIIncludeAll)

	var openaiModels []OpenAIModel

	visibleModels, err := handlers.ModelService.ListEnabledModels(ctx)
	if err != nil {
		if errors.Is(err, entprivacy.Deny) {
			handlers.writeOpenAIPermissionDeniedError(c, requestID, err)
		} else {
			handlers.writeOpenAIInternalError(c, requestID, err)
		}
		return
	}

	if len(visibleModels) == 0 {
		c.JSON(http.StatusOK, gin.H{
			"object": "list",
			"data":   []OpenAIModel{},
		})

		return
	}

	if !needFullData {
		openaiModels = lo.Map(visibleModels, func(m biz.ModelFacade, _ int) OpenAIModel {
			return convertModelFacadeToOpenAIModel(m)
		})
	} else {
		displayModels, err := handlers.ModelService.OverlayConfiguredModelFacadesForDisplay(ctx, visibleModels)
		if err != nil {
			handlers.writeOpenAIInternalError(c, requestID, err)
			return
		}
		openaiModels = lo.Map(displayModels, func(m biz.ModelFacade, _ int) OpenAIModel {
			return convertModelFacadeToOpenAIExtended(m, include)
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   openaiModels,
	})
}
