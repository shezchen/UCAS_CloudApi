package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestIsPublicAPIPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/v1/chat/completions", want: true},
		{path: "/v1/models", want: true},
		{path: "/v1/responses", want: true},
		{path: "/v1/messages", want: true},
		{path: "/v1", want: true},
		{path: "/v1beta/models", want: true},
		{path: "/anthropic/v1/messages", want: true},
		{path: "/jina/v1/rerank", want: true},
		{path: "/doubao/v3/contents/generations/tasks", want: true},
		{path: "/gemini/v1beta/models", want: true},
		{path: "/v1x/other", want: false},
		{path: "/admin/graphql", want: false},
		{path: "/admin/auth/signin", want: false},
		{path: "/openapi/v1/graphql", want: false},
		{path: "/oauth/callback", want: false},
		{path: "/health", want: false},
		{path: "/", want: false},
		{path: "", want: false},

		// Trailing slashes are not routed by gin but still reach the SPA
		// fallback, which needs the public policy.
		{path: "/v1/models/", want: true},
		{path: "/v1/chat/completions/", want: true},
		{path: "/admin/graphql/", want: false},

		// Non-canonical paths are served by whatever route the cleaned path
		// resolves to, so they must not inherit the public policy.
		{path: "/v1/../admin/graphql", want: false},
		{path: "/v1/../admin/system/status", want: false},
		{path: "/v1/./models", want: false},
		{path: "/v1//models", want: false},
		{path: "//v1/models", want: false},
		{path: "/v1/models/..", want: false},
		{path: "/admin/../v1/models", want: false},
	}

	for _, tt := range tests {
		if got := IsPublicAPIPath(tt.path); got != tt.want {
			t.Errorf("IsPublicAPIPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// newCORSTestEngine mirrors the middleware layout of SetupRoutes: WithCORS is
// global, followed by an auth middleware that rejects unauthenticated calls,
// plus the catch-all OPTIONS route.
func newCORSTestEngine(restricted gin.HandlerFunc) *gin.Engine {
	return newCORSTestEngineWithPrivateNetwork(restricted, false)
}

func newCORSTestEngineWithPrivateNetwork(restricted gin.HandlerFunc, allowPrivateNetwork bool) *gin.Engine {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(WithCORS(restricted, allowPrivateNetwork))
	engine.OPTIONS("*any", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	requireAuth := func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			AbortWithError(c, http.StatusUnauthorized, ErrAPIKeyRequired)
			return
		}

		c.Next()
	}

	api := engine.Group("/", requireAuth)
	api.GET("/v1/models", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"object": "list"})
	})
	api.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"object": "chat.completion"})
	})

	admin := engine.Group("/admin")
	admin.GET("/system/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"isInitialized": true})
	})

	return engine
}

// newCORSTestEngineWithGlobalAuth puts the auth middleware in the global chain
// directly after WithCORS. newCORSTestEngine attaches it to a route group
// instead, which gin never consults for a preflight -- OPTIONS lives in its own
// method tree and matches the catch-all route -- so that engine cannot show
// whether WithCORS really answers ahead of authentication.
func newCORSTestEngineWithGlobalAuth(authCalled *bool) *gin.Engine {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(WithCORS(nil, false))
	engine.Use(func(c *gin.Context) {
		*authCalled = true

		AbortWithError(c, http.StatusUnauthorized, ErrAPIKeyRequired)
	})

	engine.OPTIONS("*any", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"object": "chat.completion"})
	})

	return engine
}

func TestWithCORSPublicPreflightShortCircuitsBeforeAuth(t *testing.T) {
	authCalled := false
	engine := newCORSTestEngineWithGlobalAuth(&authCalled)

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://random-client.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type,x-client-name")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", w.Code, http.StatusNoContent)
	}

	if authCalled {
		t.Error("authentication ran on a public preflight request")
	}

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}

	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "authorization,content-type,x-client-name" {
		t.Errorf("Access-Control-Allow-Headers = %q, want reflected request headers", got)
	}

	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") || !strings.Contains(got, "OPTIONS") {
		t.Errorf("Access-Control-Allow-Methods = %q, want POST and OPTIONS included", got)
	}

	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want empty", got)
	}

	if got := w.Header().Get("Access-Control-Max-Age"); got == "" {
		t.Error("Access-Control-Max-Age is empty")
	}

	// Control: the same engine does reach authentication for a real request,
	// so the assertion above is about the short-circuit and not about the
	// middleware being absent.
	authCalled = false
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://random-client.example")

	w = httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if !authCalled {
		t.Fatal("authentication did not run on a non-preflight request")
	}

	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestWithCORSPublicPreflightFallbackHeaders(t *testing.T) {
	engine := newCORSTestEngine(nil)

	req := httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "GET")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", w.Code, http.StatusNoContent)
	}

	allowHeaders := w.Header().Get("Access-Control-Allow-Headers")
	for _, required := range []string{"Authorization", "Content-Type", "X-Api-Key", "X-Client-Name"} {
		if !strings.Contains(allowHeaders, required) {
			t.Errorf("Access-Control-Allow-Headers %q missing %q", allowHeaders, required)
		}
	}
}

func TestWithCORSPublicErrorResponseKeepsHeaders(t *testing.T) {
	engine := newCORSTestEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://random-client.example")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin on 401 = %q, want *", got)
	}

	if got := w.Header().Get("Access-Control-Expose-Headers"); got == "" {
		t.Error("Access-Control-Expose-Headers is empty on actual response")
	}
}

func TestWithCORSPublicSuccessResponseKeepsHeaders(t *testing.T) {
	engine := newCORSTestEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://random-client.example")
	req.Header.Set("Authorization", "Bearer test-key")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin on 200 = %q, want *", got)
	}
}

func TestWithCORSPrivateNetworkPreflight(t *testing.T) {
	tests := []struct {
		name                string
		allowPrivateNetwork bool
		want                string
	}{
		{name: "disabled by default", allowPrivateNetwork: false, want: ""},
		{name: "granted when enabled", allowPrivateNetwork: true, want: "true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := newCORSTestEngineWithPrivateNetwork(nil, tt.allowPrivateNetwork)

			req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
			req.Header.Set("Origin", "https://random-client.example")
			req.Header.Set("Access-Control-Request-Method", "POST")
			req.Header.Set("Access-Control-Request-Private-Network", "true")

			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if w.Code != http.StatusNoContent {
				t.Fatalf("preflight status = %d, want %d", w.Code, http.StatusNoContent)
			}

			if got := w.Header().Get("Access-Control-Allow-Private-Network"); got != tt.want {
				t.Errorf("Access-Control-Allow-Private-Network = %q, want %q", got, tt.want)
			}

			// The rest of the public policy is unaffected either way.
			if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
			}
		})
	}
}

// A page that is not doing Private Network Access must never be handed the
// grant, even with the flag on.
func TestWithCORSPrivateNetworkNotGrantedWithoutRequest(t *testing.T) {
	engine := newCORSTestEngineWithPrivateNetwork(nil, true)

	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://random-client.example")
	req.Header.Set("Access-Control-Request-Method", "POST")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Private-Network"); got != "" {
		t.Errorf("Access-Control-Allow-Private-Network = %q, want empty", got)
	}
}

// A path such as /v1/../admin/system/status carries a public prefix but is not
// what gets served, so it must be handled by the restricted policy rather than
// handed the wildcard origin.
func TestWithCORSNonCanonicalPathsUseRestrictedPolicy(t *testing.T) {
	targets := []string{
		"/v1/../admin/system/status",
		"/v1/..%2fadmin/system/status",
		"/v1/./models",
		"/v1//models",
		"/admin/../v1/models",
	}

	for _, target := range targets {
		t.Run(target, func(t *testing.T) {
			restrictedCalled := false
			engine := newCORSTestEngine(func(c *gin.Context) {
				restrictedCalled = true
			})

			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.Header.Set("Origin", "https://random-client.example")

			w := httptest.NewRecorder()
			engine.ServeHTTP(w, req)

			if !restrictedCalled {
				t.Error("restricted CORS handler was not called")
			}

			if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
				t.Error("non-canonical path must not get wildcard Access-Control-Allow-Origin")
			}
		})
	}
}

func TestWithCORSRestrictedDelegation(t *testing.T) {
	restrictedCalled := false
	restricted := func(c *gin.Context) {
		restrictedCalled = true
	}

	engine := newCORSTestEngine(restricted)

	// Admin routes delegate to the restricted policy and must not get the
	// wildcard origin.
	req := httptest.NewRequest(http.MethodGet, "/admin/system/status", nil)
	req.Header.Set("Origin", "https://random-client.example")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if !restrictedCalled {
		t.Error("restricted CORS handler was not called for admin route")
	}

	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("admin route must not get wildcard Access-Control-Allow-Origin")
	}

	// Public routes bypass the restricted policy entirely.
	restrictedCalled = false
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://random-client.example")
	req.Header.Set("Authorization", "Bearer test-key")

	engine.ServeHTTP(httptest.NewRecorder(), req)

	if restrictedCalled {
		t.Error("restricted CORS handler must not run for public API routes")
	}
}

func TestWithCORSNilRestrictedAdminUntouched(t *testing.T) {
	engine := newCORSTestEngine(nil)

	req := httptest.NewRequest(http.MethodGet, "/admin/system/status", nil)
	req.Header.Set("Origin", "https://random-client.example")

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want empty when CORS disabled", got)
	}
}
