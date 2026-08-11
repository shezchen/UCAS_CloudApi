package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// publicAPIPrefixes lists the route prefixes of the public model APIs
// (OpenAI / Anthropic / Gemini / Jina / Doubao compatible endpoints).
// These routes authenticate with API keys carried in request headers instead
// of cookies or browser sessions, so they are safe to expose to browser-based
// clients from any origin.
//
// Management surfaces (/admin, /openapi, /oauth, static frontend) are
// intentionally excluded and keep the configurable strict CORS policy.
var publicAPIPrefixes = []string{
	"/v1",
	"/v1beta",
	"/anthropic",
	"/jina",
	"/doubao",
	"/gemini",
}

const (
	publicCORSAllowMethods = "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS"

	// publicCORSAllowHeaders is the fallback allow-list used when a preflight
	// request does not carry Access-Control-Request-Headers. When the browser
	// does send Access-Control-Request-Headers, the requested headers are
	// reflected back instead, so preflight never fails on client-specific
	// headers (e.g. x-client-name, x-stainless-*, anthropic-version).
	publicCORSAllowHeaders = "Accept, Accept-Language, Authorization, Cache-Control, Content-Type, Origin, User-Agent, " +
		"X-Api-Key, Api-Key, X-Goog-Api-Key, X-Google-Api-Key, X-Requested-With, X-Request-Id, " +
		"X-Client-Name, X-Client-Version, OpenAI-Organization, OpenAI-Project, OpenAI-Beta, " +
		"Anthropic-Version, Anthropic-Beta, Anthropic-Dangerous-Direct-Browser-Access, " +
		"X-Goog-Api-Client, X-Project-Id, AH-Thread-Id, AH-Trace-Id, X-Trace-Id, X-Thread-Id, " +
		"X-Session-Affinity, X-Stainless-Arch, X-Stainless-Lang, X-Stainless-Os, " +
		"X-Stainless-Package-Version, X-Stainless-Retry-Count, X-Stainless-Runtime, " +
		"X-Stainless-Runtime-Version, X-Stainless-Timeout, X-Stainless-Helper-Method"

	// publicCORSExposeHeaders makes AxonHub's tracing headers readable from
	// browser JavaScript on actual (non-preflight) responses.
	publicCORSExposeHeaders = "AH-Request-Id, AH-Trace-Id, AH-Thread-Id, X-Request-Id, X-Vercel-AI-Data-Stream"

	// publicCORSMaxAge caps preflight cache duration at 24h; browsers clamp
	// this to their own maximum (e.g. 2h in Chromium).
	publicCORSMaxAge = "86400"
)

// IsPublicAPIPath reports whether the request path belongs to the public
// model APIs that use the browser-friendly wildcard CORS policy.
func IsPublicAPIPath(path string) bool {
	for _, prefix := range publicAPIPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}

	return false
}

// WithCORS dispatches CORS handling by route class.
//
// Public model API routes always get a wildcard, credential-less policy so
// that any browser-based client holding a valid API key can call them
// directly. Every other route (admin dashboard, management APIs, OAuth) is
// delegated to the restricted handler, which is the configurable strict CORS
// policy and may be nil when disabled.
//
// The middleware must be registered before any authentication middleware:
// it short-circuits OPTIONS preflight requests on public routes with 204
// before API key validation can reject them.
func WithCORS(restricted gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if IsPublicAPIPath(c.Request.URL.Path) {
			applyPublicCORS(c)
			return
		}

		if restricted != nil {
			restricted(c)
		}
	}
}

func applyPublicCORS(c *gin.Context) {
	header := c.Writer.Header()
	header.Set("Access-Control-Allow-Origin", "*")
	// A wildcard origin must never be combined with credentials; browsers
	// reject that pairing. Public APIs use API keys, not cookies, so drop
	// whatever another layer may have set.
	header.Del("Access-Control-Allow-Credentials")

	if c.Request.Method != http.MethodOptions {
		header.Set("Access-Control-Expose-Headers", publicCORSExposeHeaders)
		return
	}

	header.Set("Access-Control-Allow-Methods", publicCORSAllowMethods)

	if requested := c.Request.Header.Get("Access-Control-Request-Headers"); requested != "" {
		header.Set("Access-Control-Allow-Headers", requested)
	} else {
		header.Set("Access-Control-Allow-Headers", publicCORSAllowHeaders)
	}

	header.Set("Access-Control-Max-Age", publicCORSMaxAge)
	header.Add("Vary", "Access-Control-Request-Headers")

	// Chromium sends Private Network Access preflights when a public site
	// calls a locally hosted instance (e.g. SillyTavern on HTTPS talking to
	// a self-hosted gateway on localhost). Granting it keeps that setup
	// working; authentication is still enforced by API keys.
	if c.Request.Header.Get("Access-Control-Request-Private-Network") == "true" {
		header.Set("Access-Control-Allow-Private-Network", "true")
	}

	c.AbortWithStatus(http.StatusNoContent)
}
