package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
)

type campusCatalogReader interface {
	GetResources(context.Context) (*biz.CampusResources, error)
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

func (h *CampusCatalogHandlers) GetResources(c *gin.Context) {
	resources, err := h.catalog.GetResources(c.Request.Context())
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrCampusCatalogProjectRequired):
			JSONError(c, http.StatusBadRequest, err)
		case errors.Is(err, biz.ErrCampusCatalogUnauthorized):
			JSONError(c, http.StatusUnauthorized, err)
		case errors.Is(err, biz.ErrCampusCatalogForbidden):
			JSONError(c, http.StatusForbidden, err)
		default:
			log.Error(c.Request.Context(), "failed to load campus resource catalog", log.Cause(err))
			JSONError(c, http.StatusInternalServerError, errors.New("failed to load available resources"))
		}
		return
	}

	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, resources)
}
