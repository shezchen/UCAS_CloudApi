package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
)

type campusCatalogReader interface {
	GetResources(context.Context) (*biz.CampusResources, error)
	GetChannelModelCapabilities(context.Context) (*biz.CampusChannelModelCapabilities, error)
	UpdateChannelModelCapabilities(context.Context, biz.UpdateCampusChannelModelCapabilitiesInput) error
}

type CampusCatalogHandlersParams struct {
	fx.In

	CampusCatalogService *biz.CampusCatalogService
}

func NewCampusCatalogHandlers(params CampusCatalogHandlersParams) *CampusCatalogHandlers {
	return &CampusCatalogHandlers{catalog: params.CampusCatalogService}
}

type CampusCatalogHandlers struct {
	catalog campusCatalogReader
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
