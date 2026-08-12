// Package apipath centralizes the URL prefixes AxonHub serves its APIs under.
//
// Two independent places classify a request by prefix: the CORS middleware,
// which decides between the wildcard policy for model APIs and the configured
// strict policy, and the static SPA fallback, which answers unmatched API
// paths with JSON instead of index.html. Keeping one list here means a newly
// added API surface cannot be registered in one and forgotten in the other.
package apipath

import "strings"

// PublicModelPrefixes are the OpenAI / Anthropic / Gemini / Jina / Doubao
// compatible model endpoints. They authenticate with API keys carried in
// request headers rather than cookies or browser sessions.
var PublicModelPrefixes = []string{
	"/v1",
	"/v1beta",
	"/anthropic",
	"/jina",
	"/doubao",
	"/gemini",
}

// ManagementPrefixes are the operator-facing surfaces: the dashboard REST and
// GraphQL APIs and the service-account OpenAPI. They are session or
// service-account authenticated and keep the configurable strict CORS policy.
var ManagementPrefixes = []string{
	"/admin",
	"/openapi",
}

// IsPublicModelAPI reports whether path is served by one of the public model
// APIs. The path is matched verbatim; callers that care about traversal or
// other non-canonical forms must normalize first.
func IsPublicModelAPI(path string) bool {
	return hasAnyPrefix(path, PublicModelPrefixes)
}

// IsAPI reports whether path is served by any API surface, public or
// management, as opposed to the static frontend.
func IsAPI(path string) bool {
	return IsPublicModelAPI(path) || hasAnyPrefix(path, ManagementPrefixes)
}

func hasAnyPrefix(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}

	return false
}
