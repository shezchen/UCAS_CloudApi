package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
)

type stubCampusCatalogReader struct {
	resources       *biz.CampusResources
	capabilities    *biz.CampusChannelModelCapabilities
	resourceErr     error
	capabilitiesErr error
	updateErr       error
	updatedInput    *biz.UpdateCampusChannelModelCapabilitiesInput
	probeGUID       objects.GUID
	probeModels     []string
	probeErr        error
	health          *biz.CampusChannelHealth
	healthErr       error
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

func (s *stubCampusCatalogReader) PrepareChannelProbe(context.Context, int) (objects.GUID, []string, error) {
	return s.probeGUID, s.probeModels, s.probeErr
}

func (s *stubCampusCatalogReader) GetChannelHealth(context.Context, int) (*biz.CampusChannelHealth, error) {
	return s.health, s.healthErr
}

type stubCampusChannelTester struct {
	results map[string]*orchestrator.TestChannelResult
	errs    map[string]error
	calls   []string
}

func (s *stubCampusChannelTester) TestChannel(
	_ context.Context,
	_ objects.GUID,
	modelID *string,
	_ *httpclient.ProxyConfig,
) (*orchestrator.TestChannelResult, error) {
	model := ""
	if modelID != nil {
		model = *modelID
	}
	s.calls = append(s.calls, model)
	return s.results[model], s.errs[model]
}

type stubCampusChannelProbeRecorder struct {
	results []bool
}

func (s *stubCampusChannelProbeRecorder) RecordManualProbeResult(_ context.Context, _ int, success bool) error {
	s.results = append(s.results, success)
	return nil
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

func TestCampusChannelProbeFallsBackOnlyForExplicitUnsupportedModelAndReturnsTruth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	badStatus := http.StatusBadRequest
	okStatus := http.StatusOK
	upstreamError := "The 'gpt-5' model is not supported when using Codex with a ChatGPT account. Authorization: Bearer upstream-secret student@example.com"
	tester := &stubCampusChannelTester{
		results: map[string]*orchestrator.TestChannelResult{
			"gpt-5": {
				Success:          false,
				Latency:          0.12,
				StatusCode:       &badStatus,
				Error:            &upstreamError,
				ModelUnsupported: true,
			},
			"gpt-5.6-sol": {
				Success:    true,
				Latency:    0.34,
				StatusCode: &okStatus,
			},
		},
		errs: map[string]error{},
	}
	recorder := &stubCampusChannelProbeRecorder{}
	catalog := &stubCampusCatalogReader{
		probeGUID:   objects.GUID{Type: ent.TypeChannel, ID: 7},
		probeModels: []string{"gpt-5", "gpt-5.6-sol", "gpt-5.4"},
		health:      &biz.CampusChannelHealth{State: "healthy"},
	}
	handler := &CampusCatalogHandlers{
		catalog:       catalog,
		probe:         tester,
		channelProbes: recorder,
		probeLast:     make(map[string]time.Time),
	}

	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Params = gin.Params{{Key: "id", Value: "7"}}
	requestCtx := contexts.WithUser(context.Background(), &ent.User{ID: 42})
	ctx.Request = httptest.NewRequest(http.MethodPost, "/admin/campus/channels/7/probe", nil).WithContext(requestCtx)
	handler.PostChannelProbe(ctx)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []string{"gpt-5", "gpt-5.6-sol"}, tester.calls)
	require.Equal(t, []bool{true}, recorder.results)
	require.Contains(t, response.Body.String(), `"modelID":"gpt-5.6-sol"`)
	require.Contains(t, response.Body.String(), `"statusCode":400`)
	require.Contains(t, response.Body.String(), `"statusCode":200`)
	require.Contains(t, response.Body.String(), "not supported")
	require.NotContains(t, response.Body.String(), "upstream-secret")
	require.NotContains(t, response.Body.String(), "student@example.com")
	require.Contains(t, response.Body.String(), "[REDACTED]")
	require.Contains(t, response.Body.String(), "[EMAIL]")
	require.JSONEq(t, `{
		"success":true,
		"channelID":"7",
		"modelID":"gpt-5.6-sol",
		"latency":0.46,
		"statusCode":200,
		"attempts":[
			{"modelID":"gpt-5","success":false,"latency":0.12,"statusCode":400,"error":"The 'gpt-5' model is not supported when using Codex with a ChatGPT account. Authorization=[REDACTED] [REDACTED] [EMAIL]"},
			{"modelID":"gpt-5.6-sol","success":true,"latency":0.34,"statusCode":200}
		],
		"health":{"state":"healthy","recentSuccessRate":0,"recentRequestCount":0}
	}`, response.Body.String())
}

func TestCampusChannelProbeDoesNotHideAuthenticationFailureBehindFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	status := http.StatusUnauthorized
	upstreamError := "Your authentication token has been invalidated. Please try signing in again."
	tester := &stubCampusChannelTester{
		results: map[string]*orchestrator.TestChannelResult{
			"gpt-5.6-sol": {
				Success:    false,
				Latency:    0.2,
				StatusCode: &status,
				Error:      &upstreamError,
			},
		},
		errs: map[string]error{},
	}
	recorder := &stubCampusChannelProbeRecorder{}
	handler := &CampusCatalogHandlers{
		catalog: &stubCampusCatalogReader{
			probeGUID:   objects.GUID{Type: ent.TypeChannel, ID: 10},
			probeModels: []string{"gpt-5.6-sol", "gpt-5.4"},
			health:      &biz.CampusChannelHealth{State: "unhealthy"},
		},
		probe:         tester,
		channelProbes: recorder,
		probeLast:     make(map[string]time.Time),
	}

	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Params = gin.Params{{Key: "id", Value: "10"}}
	requestCtx := contexts.WithUser(context.Background(), &ent.User{ID: 43})
	ctx.Request = httptest.NewRequest(http.MethodPost, "/admin/campus/channels/10/probe", nil).WithContext(requestCtx)
	handler.PostChannelProbe(ctx)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []string{"gpt-5.6-sol"}, tester.calls)
	require.Equal(t, []bool{false}, recorder.results)
	require.Contains(t, response.Body.String(), `"statusCode":401`)
	require.Contains(t, response.Body.String(), upstreamError)
	require.Contains(t, response.Body.String(), `"errorCategory":"probe_failed"`)
}
