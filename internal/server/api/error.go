package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/objects"
)

// JSONError returns a JSON error response and adds the error to gin context for access logging.
func JSONError(c *gin.Context, status int, err error) {
	JSONErrorWithCode(c, status, "", err)
}

// JSONErrorWithCode adds a stable machine-readable code while keeping the
// existing human-readable error shape.
func JSONErrorWithCode(c *gin.Context, status int, code string, err error) {
	_ = c.Error(err)
	c.JSON(status, objects.ErrorResponse{
		Error: objects.Error{
			Type:    http.StatusText(status),
			Message: err.Error(),
			Code:    code,
		},
	})
}
