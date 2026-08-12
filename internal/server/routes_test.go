package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/static"
)

// newRoutingTestEngine mirrors the ordering SetupRoutes uses for the parts that
// decide how an unmatched path is answered: the SPA fallback on NoRoute, the
// CORS middleware, the OPTIONS catch-all, and an API-key gate on the model
// routes. It goes through New so the engine carries the production settings.
func newRoutingTestEngine(t *testing.T) *gin.Engine {
	t.Helper()

	srv, err := New(Config{})
	require.NoError(t, err)

	srv.NoRoute(static.Handler())
	srv.Use(middleware.WithCORS(nil))
	srv.OPTIONS("*any", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	apiGroup := srv.Group("", func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
		}
	})
	apiGroup.GET("/v1/models", func(c *gin.Context) { c.String(http.StatusOK, "models") })
	apiGroup.POST("/v1/chat/completions", func(c *gin.Context) { c.String(http.StatusOK, "chat") })

	adminGroup := srv.Group("/admin")
	adminGroup.GET("/system/status", func(c *gin.Context) { c.String(http.StatusOK, "status") })

	return srv.Engine
}

func doRequest(engine *gin.Engine, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	return w
}

// gin's RedirectTrailingSlash answers from the router itself, before any
// middleware runs, so a redirected request never reaches WithCORS and the
// browser sees an opaque CORS failure instead of following the redirect.
func TestTrailingSlashPublicAPIRequestsKeepCORSHeaders(t *testing.T) {
	engine := newRoutingTestEngine(t)

	tests := []struct {
		name   string
		method string
		target string
	}{
		{"chat completions", http.MethodPost, "/v1/chat/completions/"},
		{"models", http.MethodGet, "/v1/models/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := doRequest(engine, tt.method, tt.target, map[string]string{
				"Origin":        "https://app.example",
				"Authorization": "Bearer sk-test",
			})

			require.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
			require.NotContains(t, []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect}, w.Code)
			require.Equal(t, http.StatusNotFound, w.Code)
			require.Contains(t, w.Header().Get("Content-Type"), "application/json")
			require.Contains(t, w.Body.String(), "path not found")
		})
	}
}

func TestExactPublicAPIRequestsStillRoute(t *testing.T) {
	engine := newRoutingTestEngine(t)

	w := doRequest(engine, http.MethodGet, "/v1/models", map[string]string{
		"Origin":        "https://app.example",
		"Authorization": "Bearer sk-test",
	})
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "models", w.Body.String())
	require.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))

	w = doRequest(engine, http.MethodGet, "/v1/models", map[string]string{
		"Origin": "https://app.example",
	})
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestTrailingSlashAdminAPIReturnsJSONNotFound(t *testing.T) {
	engine := newRoutingTestEngine(t)

	w := doRequest(engine, http.MethodGet, "/admin/system/status/", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Contains(t, w.Header().Get("Content-Type"), "application/json")
	require.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
		"management routes must not pick up the public wildcard policy")

	w = doRequest(engine, http.MethodGet, "/admin/system/status", nil)
	require.Equal(t, http.StatusOK, w.Code)
}

// Frontend routes must keep reaching the SPA fallback rather than the JSON 404
// branch, with or without a trailing slash.
func TestTrailingSlashFrontendRouteStillServesSPA(t *testing.T) {
	engine := newRoutingTestEngine(t)

	for _, target := range []string{"/settings/profile", "/settings/profile/"} {
		w := doRequest(engine, http.MethodGet, target, nil)
		require.NotEqual(t, http.StatusMovedPermanently, w.Code,
			"%s must reach the handler chain instead of being redirected", target)
		require.NotContains(t, w.Body.String(), "path not found",
			"%s must not be answered by the API 404 branch", target)
	}
}
