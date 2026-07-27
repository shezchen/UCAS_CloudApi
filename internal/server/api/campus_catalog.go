package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
)

type campusCatalogReader interface {
	GetResources(context.Context) (*biz.CampusResources, error)
	GetChannelModelCapabilities(context.Context) (*biz.CampusChannelModelCapabilities, error)
	UpdateChannelModelCapabilities(context.Context, biz.UpdateCampusChannelModelCapabilitiesInput) error
}

type campusAPIActivityReader interface {
	GetAPIActivity(context.Context, *int) (*biz.CampusAPIActivity, error)
}

type campusChannelProbeReader interface {
	PrepareChannelProbe(context.Context, int) (objects.GUID, string, error)
	GetChannelHealth(context.Context, int) (*biz.CampusChannelHealth, error)
}

type CampusCatalogHandlersParams struct {
	fx.In

	CampusCatalogService        *biz.CampusCatalogService
	ChannelService              *biz.ChannelService
	RequestService              *biz.RequestService
	SystemService               *biz.SystemService
	UsageLogService             *biz.UsageLogService
	PromptProtectionRuleService *biz.PromptProtectionRuleService
	HTTPClient                  *httpclient.HttpClient
	ChannelProbeService         *biz.ChannelProbeService
}

func NewCampusCatalogHandlers(params CampusCatalogHandlersParams) *CampusCatalogHandlers {
	return &CampusCatalogHandlers{
		catalog: params.CampusCatalogService,
		probe: orchestrator.NewTestChannelOrchestrator(
			params.ChannelService,
			params.RequestService,
			params.SystemService,
			params.UsageLogService,
			params.PromptProtectionRuleService,
			params.HTTPClient,
		),
		channelProbes: params.ChannelProbeService,
		probeLast:     make(map[string]time.Time),
	}
}

type CampusCatalogHandlers struct {
	catalog       campusCatalogReader
	probe         *orchestrator.TestChannelOrchestrator
	channelProbes *biz.ChannelProbeService
	probeMu       sync.Mutex
	probeLast     map[string]time.Time
}

const campusChannelModelCapabilityBodyLimit = 64 << 10

func (h *CampusCatalogHandlers) GetResources(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	resources, err := h.catalog.GetResources(c.Request.Context())
	if err != nil {
		h.writeCampusCatalogError(c, err, "failed to load campus resource catalog")
		return
	}

	c.JSON(http.StatusOK, resources)
}

func (h *CampusCatalogHandlers) GetAPIActivity(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	reader, ok := h.catalog.(campusAPIActivityReader)
	if !ok {
		JSONError(c, http.StatusNotImplemented, errors.New("campus API activity is unavailable"))
		return
	}

	var selectedAPIKeyID *int
	if rawID := c.Query("apiKeyID"); rawID != "" {
		id, err := strconv.Atoi(rawID)
		if err != nil || id <= 0 {
			JSONError(c, http.StatusBadRequest, biz.ErrCampusCatalogInvalidInput)
			return
		}
		selectedAPIKeyID = &id
	}

	activity, err := reader.GetAPIActivity(c.Request.Context(), selectedAPIKeyID)
	if err != nil {
		h.writeCampusCatalogError(c, err, "failed to load own campus API activity")
		return
	}
	c.JSON(http.StatusOK, activity)
}

const campusPublicProbeCooldown = 30 * time.Second

func (h *CampusCatalogHandlers) PostChannelProbe(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	reader, ok := h.catalog.(campusChannelProbeReader)
	if !ok || h.probe == nil || h.channelProbes == nil {
		JSONError(c, http.StatusNotImplemented, errors.New("campus channel probe is unavailable"))
		return
	}

	channelID, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelID <= 0 {
		JSONError(c, http.StatusBadRequest, biz.ErrCampusCatalogInvalidInput)
		return
	}

	channelGUID, modelID, err := reader.PrepareChannelProbe(c.Request.Context(), channelID)
	if err != nil {
		h.writeCampusCatalogError(c, err, "failed to prepare campus channel probe")
		return
	}

	currentUser, ok := contexts.GetUser(c.Request.Context())
	if !ok || currentUser == nil {
		JSONError(c, http.StatusUnauthorized, biz.ErrCampusCatalogUnauthorized)
		return
	}
	if retryAfter, allowed := h.reservePublicProbe(currentUser.ID, channelID, time.Now()); !allowed {
		c.Header("Retry-After", strconv.Itoa(max(int(retryAfter.Seconds()), 1)))
		JSONError(c, http.StatusTooManyRequests, errors.New("please wait before testing this channel again"))
		return
	}

	probeCtx := contexts.WithSource(c.Request.Context(), request.SourceTest)
	result, err := authz.RunWithSystemBypass(probeCtx, "campus-public-channel-probe-execution", func(bypassCtx context.Context) (*orchestrator.TestChannelResult, error) {
		return h.probe.TestChannel(bypassCtx, channelGUID, &modelID, nil)
	})
	if err != nil {
		h.recordPublicProbeResult(probeCtx, channelID, false)
		log.Error(
			probeCtx,
			"campus public channel probe failed",
			log.Int("channel_id", channelID),
			log.String("error_type", fmt.Sprintf("%T", err)),
		)
		JSONError(c, http.StatusBadGateway, errors.New("channel test failed"))
		return
	}

	h.recordPublicProbeResult(probeCtx, channelID, result.Success)
	health, healthErr := reader.GetChannelHealth(c.Request.Context(), channelID)
	if healthErr != nil {
		log.Error(c.Request.Context(), "failed to reload channel health after probe", log.Int("channel_id", channelID), log.Cause(healthErr))
	}

	response := gin.H{
		"success": result.Success,
		"latency": result.Latency,
		"health":  health,
	}
	if !result.Success {
		response["errorCategory"] = "probe_failed"
	}
	c.JSON(http.StatusOK, response)
}

func (h *CampusCatalogHandlers) reservePublicProbe(userID, channelID int, now time.Time) (time.Duration, bool) {
	key := fmt.Sprintf("%d:%d", userID, channelID)
	h.probeMu.Lock()
	defer h.probeMu.Unlock()

	if h.probeLast == nil {
		h.probeLast = make(map[string]time.Time)
	}
	if last := h.probeLast[key]; !last.IsZero() {
		elapsed := now.Sub(last)
		if elapsed < campusPublicProbeCooldown {
			return campusPublicProbeCooldown - elapsed, false
		}
	}
	h.probeLast[key] = now

	return 0, true
}

func (h *CampusCatalogHandlers) recordPublicProbeResult(ctx context.Context, channelID int, success bool) {
	if err := authz.RunWithSystemBypassVoid(ctx, "campus-record-public-probe", func(bypassCtx context.Context) error {
		return h.channelProbes.RecordManualProbeResult(bypassCtx, channelID, success)
	}); err != nil {
		log.Error(ctx, "failed to record campus public channel probe result", log.Int("channel_id", channelID), log.Cause(err))
	}
}

func (h *CampusCatalogHandlers) GetChannelModelCapabilities(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	capabilities, err := h.catalog.GetChannelModelCapabilities(c.Request.Context())
	if err != nil {
		h.writeCampusCatalogError(c, err, "failed to load contributor model capabilities")
		return
	}

	c.JSON(http.StatusOK, capabilities)
}

func (h *CampusCatalogHandlers) PatchChannelModelCapabilities(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	input, err := decodeCampusChannelModelCapabilityRequest(c.Request.Body)
	if err != nil {
		JSONError(c, http.StatusBadRequest, biz.ErrCampusCatalogInvalidInput)
		return
	}
	if err := h.catalog.UpdateChannelModelCapabilities(c.Request.Context(), input); err != nil {
		h.writeCampusCatalogError(c, err, "failed to update contributor model capabilities")
		return
	}

	c.Status(http.StatusNoContent)
	c.Writer.WriteHeaderNow()
}

func (h *CampusCatalogHandlers) writeCampusCatalogError(c *gin.Context, err error, logMessage string) {
	switch {
	case errors.Is(err, biz.ErrCampusCatalogProjectRequired), errors.Is(err, biz.ErrCampusCatalogInvalidInput):
		JSONError(c, http.StatusBadRequest, err)
	case errors.Is(err, biz.ErrCampusCatalogUnauthorized):
		JSONError(c, http.StatusUnauthorized, err)
	case errors.Is(err, biz.ErrCampusCatalogForbidden), errors.Is(err, biz.ErrCampusOwnerOverrideForbidden):
		JSONError(c, http.StatusForbidden, err)
	case errors.Is(err, biz.ErrCampusChannelNotFound):
		JSONError(c, http.StatusNotFound, err)
	default:
		log.Error(c.Request.Context(), logMessage, log.Cause(err))
		JSONError(c, http.StatusInternalServerError, errors.New("campus resource request failed"))
	}
}

func decodeCampusChannelModelCapabilityRequest(body io.Reader) (biz.UpdateCampusChannelModelCapabilitiesInput, error) {
	rawBody, err := io.ReadAll(io.LimitReader(body, campusChannelModelCapabilityBodyLimit+1))
	if err != nil {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, err
	}
	if len(rawBody) > campusChannelModelCapabilityBodyLimit {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, errors.New("request body is too large")
	}

	decoder := json.NewDecoder(bytes.NewReader(rawBody))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, err
	}
	for key := range fields {
		switch key {
		case "channelID", "modelID", "override":
		default:
			return biz.UpdateCampusChannelModelCapabilitiesInput{}, errors.New("unknown request field")
		}
	}

	channelJSON, hasChannel := fields["channelID"]
	modelJSON, hasModel := fields["modelID"]
	overrideJSON, hasOverride := fields["override"]
	if !hasChannel || !hasModel || !hasOverride {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, errors.New("missing request field")
	}

	var input biz.UpdateCampusChannelModelCapabilitiesInput
	if err := json.Unmarshal(channelJSON, &input.ChannelID); err != nil {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, err
	}
	if err := json.Unmarshal(modelJSON, &input.ModelID); err != nil {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, err
	}
	if bytes.Equal(bytes.TrimSpace(overrideJSON), []byte("null")) {
		return input, nil
	}

	var override struct {
		Vision          *bool `json:"vision"`
		ToolCall        *bool `json:"toolCall"`
		Reasoning       *bool `json:"reasoning"`
		ContextLength   *int  `json:"contextLength"`
		MaxOutputTokens *int  `json:"maxOutputTokens"`
	}
	overrideDecoder := json.NewDecoder(bytes.NewReader(overrideJSON))
	overrideDecoder.DisallowUnknownFields()
	if err := overrideDecoder.Decode(&override); err != nil {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, err
	}
	if err := ensureJSONEOF(overrideDecoder); err != nil {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, err
	}
	if override.Vision == nil || override.ToolCall == nil || override.Reasoning == nil || override.ContextLength == nil {
		return biz.UpdateCampusChannelModelCapabilitiesInput{}, errors.New("incomplete model capability override")
	}

	input.Override = &biz.CampusModelCapabilityOverride{
		Vision:          *override.Vision,
		ToolCall:        *override.ToolCall,
		Reasoning:       *override.Reasoning,
		ContextLength:   *override.ContextLength,
		MaxOutputTokens: override.MaxOutputTokens,
	}

	return input, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}

	return nil
}
