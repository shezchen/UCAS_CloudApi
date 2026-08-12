package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestWriteOpenAIInternalErrorDoesNotEchoInternalDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	internal := errors.New("dial tcp 10.0.0.7:5432: connect: connection refused")

	(&OpenAIHandlers{}).writeOpenAIInternalError(c, "req-123", internal)

	require.Equal(t, http.StatusInternalServerError, w.Code)

	body := w.Body.String()
	require.NotContains(t, body, internal.Error())
	require.NotContains(t, body, "10.0.0.7")
	require.Contains(t, body, "internal server error")
	require.Contains(t, body, "req-123")

	// The detail is still available to the AccessLog middleware.
	require.Len(t, c.Errors, 1)
	require.ErrorIs(t, c.Errors.Last(), internal)
}
