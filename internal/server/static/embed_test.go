package static

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	ginstatic "github.com/gin-contrib/static"

	"github.com/looplj/axonhub/internal/objects"
)

func useTestStaticFS(t *testing.T) {
	t.Helper()

	tempDir := t.TempDir()
	require.NoError(t, os.WriteFile(tempDir+"/index.html", []byte("<html><body>test</body></html>"), 0o644))

	originalStaticFS := staticFS
	staticFS = ginstatic.LocalFile(tempDir, false)

	t.Cleanup(func() {
		staticFS = originalStaticFS
	})
}

func TestHandler_ReturnsJSON404ForUnknownAPIPaths(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useTestStaticFS(t)

	router := gin.New()
	router.NoRoute(Handler())

	for _, path := range []string{"/v1/not-found", "/anthropic/not-found", "/admin/not-found"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)

			router.ServeHTTP(recorder, req)

			require.Equal(t, http.StatusNotFound, recorder.Code)
			require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")

			var resp objects.ErrorResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
			require.Equal(t, http.StatusText(http.StatusNotFound), resp.Error.Type)
			require.Equal(t, "path not found: "+path, resp.Error.Message)
		})
	}
}

func TestHandler_ServesSPAIndexForFrontendRoutes(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useTestStaticFS(t)

	router := gin.New()
	router.NoRoute(Handler())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/settings/profile", nil)

	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "text/html")
	require.Equal(t, "no-cache, no-store, must-revalidate", recorder.Header().Get("Cache-Control"))
}

// The engine disables RedirectTrailingSlash so that CORS middleware runs on
// these requests, which routes them here instead. Each one has to land in the
// same branch it would without the trailing slash.
func TestHandler_TrailingSlashKeepsTheRouteClass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useTestStaticFS(t)

	router := gin.New()
	router.RedirectTrailingSlash = false
	router.NoRoute(Handler())

	t.Run("api path returns JSON 404", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/models/", nil)

		router.ServeHTTP(recorder, req)

		require.Equal(t, http.StatusNotFound, recorder.Code)
		require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")

		var resp objects.ErrorResponse
		require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
		require.Equal(t, "path not found: /v1/models/", resp.Error.Message)
	})

	t.Run("frontend route serves the SPA", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/settings/profile/", nil)

		router.ServeHTTP(recorder, req)

		require.Equal(t, http.StatusOK, recorder.Code)
		require.Contains(t, recorder.Header().Get("Content-Type"), "text/html")
	})
}

func TestHandler_DoesNotFallbackMissingStaticAssetToSPAIndex(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	useTestStaticFS(t)

	router := gin.New()
	router.NoRoute(Handler())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/definitely-missing.js", nil)

	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.NotContains(t, recorder.Header().Get("Content-Type"), "text/html")
}
