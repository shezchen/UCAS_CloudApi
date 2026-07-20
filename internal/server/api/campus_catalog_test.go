package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/biz"
)

type stubCampusCatalogReader struct {
	resources *biz.CampusResources
	err       error
}

func (s stubCampusCatalogReader) GetResources(context.Context) (*biz.CampusResources, error) {
	return s.resources, s.err
}

func TestCampusCatalogHandlerSuccessIsPrivateAndNoStore(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/admin/campus/resources", nil)

	handler := &CampusCatalogHandlers{catalog: stubCampusCatalogReader{resources: &biz.CampusResources{
		Models:   []string{"kimi-k2.5"},
		APIKeys:  []biz.CampusAPIKeyResources{{Name: "默认密钥", Models: []string{"kimi-k2.5"}}},
		Channels: []biz.CampusChannelResource{},
	}}}
	handler.GetResources(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
	require.JSONEq(t, `{"models":["kimi-k2.5"],"apiKeys":[{"name":"默认密钥","models":["kimi-k2.5"]}],"channels":[]}`, recorder.Body.String())
}

func TestCampusCatalogHandlerErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "missing project", err: biz.ErrCampusCatalogProjectRequired, status: http.StatusBadRequest},
		{name: "unauthorized", err: biz.ErrCampusCatalogUnauthorized, status: http.StatusUnauthorized},
		{name: "forbidden", err: biz.ErrCampusCatalogForbidden, status: http.StatusForbidden},
		{name: "internal", err: errors.New("database secret detail"), status: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/admin/campus/resources", nil)
			handler := &CampusCatalogHandlers{catalog: stubCampusCatalogReader{err: tt.err}}

			handler.GetResources(ctx)

			require.Equal(t, tt.status, recorder.Code)
			if tt.status == http.StatusInternalServerError {
				require.NotContains(t, recorder.Body.String(), "database secret detail")
			}
		})
	}
}
