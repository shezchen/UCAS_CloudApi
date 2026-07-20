package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/biz"
)

type stubCampusCatalogReader struct {
	resources       *biz.CampusResources
	capabilities    *biz.CampusChannelModelCapabilities
	resourceErr     error
	capabilitiesErr error
	updateErr       error
	updatedInput    *biz.UpdateCampusChannelModelCapabilitiesInput
}

func (s *stubCampusCatalogReader) GetResources(context.Context) (*biz.CampusResources, error) {
	return s.resources, s.resourceErr
}

func (s *stubCampusCatalogReader) GetChannelModelCapabilities(context.Context) (*biz.CampusChannelModelCapabilities, error) {
	return s.capabilities, s.capabilitiesErr
}

func (s *stubCampusCatalogReader) UpdateChannelModelCapabilities(_ context.Context, input biz.UpdateCampusChannelModelCapabilitiesInput) error {
	s.updatedInput = &input
	return s.updateErr
}

func TestCampusCatalogHandlerSuccessIsPrivateAndNoStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/admin/campus/resources", nil)

	handler := &CampusCatalogHandlers{catalog: &stubCampusCatalogReader{resources: &biz.CampusResources{
		Models:       []string{"kimi-k2.5"},
		ModelDetails: []biz.CampusModelDetail{{ID: "kimi-k2.5", Source: "catalog", Vision: true, ToolCall: true, Reasoning: true, ContextLength: 262144}},
		APIKeys: []biz.CampusAPIKeyResources{{
			Name:         "默认密钥",
			Models:       []string{"kimi-k2.5"},
			ModelDetails: []biz.CampusModelDetail{{ID: "kimi-k2.5", Source: "catalog", Vision: true, ToolCall: true, Reasoning: true, ContextLength: 262144}},
		}},
		Channels: []biz.CampusChannelResource{},
	}}}
	handler.GetResources(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
	require.JSONEq(t, `{
		"models":["kimi-k2.5"],
		"modelDetails":[{"id":"kimi-k2.5","source":"catalog","vision":true,"toolCall":true,"reasoning":true,"contextLength":262144}],
		"apiKeys":[{"name":"默认密钥","models":["kimi-k2.5"],"modelDetails":[{"id":"kimi-k2.5","source":"catalog","vision":true,"toolCall":true,"reasoning":true,"contextLength":262144}]}],
		"channels":[]
	}`, recorder.Body.String())
}

func TestCampusCatalogHandlerErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "missing project", err: biz.ErrCampusCatalogProjectRequired, status: http.StatusBadRequest},
		{name: "invalid", err: biz.ErrCampusCatalogInvalidInput, status: http.StatusBadRequest},
		{name: "unauthorized", err: biz.ErrCampusCatalogUnauthorized, status: http.StatusUnauthorized},
		{name: "forbidden", err: biz.ErrCampusCatalogForbidden, status: http.StatusForbidden},
		{name: "owner override", err: biz.ErrCampusOwnerOverrideForbidden, status: http.StatusForbidden},
		{name: "hidden channel", err: biz.ErrCampusChannelNotFound, status: http.StatusNotFound},
		{name: "internal", err: errors.New("database secret detail"), status: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/admin/campus/resources", nil)
			handler := &CampusCatalogHandlers{catalog: &stubCampusCatalogReader{resourceErr: tt.err}}

			handler.GetResources(ctx)

			require.Equal(t, tt.status, recorder.Code)
			if tt.status == http.StatusInternalServerError {
				require.NotContains(t, recorder.Body.String(), "database secret detail")
			}
		})
	}
}

func TestCampusCatalogChannelCapabilitiesHandlerIsPrivateAndSafe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/admin/campus/channel-model-capabilities", nil)

	stub := &stubCampusCatalogReader{capabilities: &biz.CampusChannelModelCapabilities{
		Channels: []biz.CampusOwnedChannelCapabilities{{
			ID:   "gid://axonhub/Channel/7",
			Name: "我的公益渠道",
			Models: []biz.CampusModelDetail{{
				ID: "kimi-k3", Source: "override", Vision: true, ToolCall: true,
				Reasoning: true, ContextLength: 1_000_000, Overridden: true,
			}},
		}},
	}}
	handler := &CampusCatalogHandlers{catalog: stub}
	handler.GetChannelModelCapabilities(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
	require.JSONEq(t, `{"channels":[{"id":"gid://axonhub/Channel/7","name":"我的公益渠道","models":[{"id":"kimi-k3","source":"override","vision":true,"toolCall":true,"reasoning":true,"contextLength":1000000,"overridden":true}]}]}`, recorder.Body.String())
	for _, forbidden := range []string{"credentials", "baseURL", "apiKey", "email", "userId", "settings"} {
		require.NotContains(t, recorder.Body.String(), forbidden)
	}
}

func TestCampusCatalogPatchChannelCapabilitiesStrictBody(t *testing.T) {
	maxOutput := 128000
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantReset  bool
	}{
		{
			name:       "set override",
			body:       `{"channelID":"gid://axonhub/Channel/7","modelID":"kimi-k3","override":{"vision":true,"toolCall":true,"reasoning":true,"contextLength":1000000,"maxOutputTokens":128000}}`,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "reset override",
			body:       `{"channelID":"gid://axonhub/Channel/7","modelID":"kimi-k3","override":null}`,
			wantStatus: http.StatusNoContent,
			wantReset:  true,
		},
		{
			name:       "missing required capability",
			body:       `{"channelID":"gid://axonhub/Channel/7","modelID":"kimi-k3","override":{"vision":true,"toolCall":true,"contextLength":1000000}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown top level field",
			body:       `{"channelID":"gid://axonhub/Channel/7","modelID":"kimi-k3","override":null,"credentials":"secret"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown override field",
			body:       `{"channelID":"gid://axonhub/Channel/7","modelID":"kimi-k3","override":{"vision":true,"toolCall":true,"reasoning":true,"contextLength":1000000,"routing":"other"}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "valid prefix with oversized trailing whitespace",
			body:       `{"channelID":"gid://axonhub/Channel/7","modelID":"kimi-k3","override":null}` + strings.Repeat(" ", campusChannelModelCapabilityBodyLimit),
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/admin/campus/channel-model-capabilities", strings.NewReader(tt.body))
			stub := &stubCampusCatalogReader{}
			handler := &CampusCatalogHandlers{catalog: stub}

			handler.PatchChannelModelCapabilities(ctx)

			require.Equal(t, tt.wantStatus, recorder.Code)
			require.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
			if tt.wantStatus != http.StatusNoContent {
				require.Nil(t, stub.updatedInput)
				require.NotContains(t, recorder.Body.String(), "secret")
				return
			}

			require.NotNil(t, stub.updatedInput)
			require.Equal(t, "gid://axonhub/Channel/7", stub.updatedInput.ChannelID)
			require.Equal(t, "kimi-k3", stub.updatedInput.ModelID)
			if tt.wantReset {
				require.Nil(t, stub.updatedInput.Override)
				return
			}
			require.Equal(t, &maxOutput, stub.updatedInput.Override.MaxOutputTokens)
			require.Equal(t, 1_000_000, stub.updatedInput.Override.ContextLength)
		})
	}
}

func TestCampusCatalogPatchChannelCapabilitiesErrorMappingAndRedaction(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "invalid", err: biz.ErrCampusCatalogInvalidInput, status: http.StatusBadRequest},
		{name: "unauthorized", err: biz.ErrCampusCatalogUnauthorized, status: http.StatusUnauthorized},
		{name: "forbidden", err: biz.ErrCampusOwnerOverrideForbidden, status: http.StatusForbidden},
		{name: "hidden", err: biz.ErrCampusChannelNotFound, status: http.StatusNotFound},
		{name: "internal", err: errors.New("database contains provider-secret"), status: http.StatusInternalServerError},
	}
	body := `{"channelID":"gid://axonhub/Channel/7","modelID":"kimi-k3","override":{"vision":true,"toolCall":true,"reasoning":true,"contextLength":1000000}}`

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/admin/campus/channel-model-capabilities", strings.NewReader(body))
			stub := &stubCampusCatalogReader{updateErr: tt.err}
			handler := &CampusCatalogHandlers{catalog: stub}

			handler.PatchChannelModelCapabilities(ctx)

			require.Equal(t, tt.status, recorder.Code)
			require.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
			if tt.status == http.StatusInternalServerError {
				require.NotContains(t, recorder.Body.String(), "provider-secret")
			}
		})
	}
}
