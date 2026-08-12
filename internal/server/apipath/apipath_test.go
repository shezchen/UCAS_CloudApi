package apipath

import "testing"

func TestIsPublicModelAPI(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v1", true},
		{"/v1/models", true},
		{"/v1/chat/completions", true},
		{"/v1beta/models", true},
		{"/anthropic/v1/messages", true},
		{"/gemini/v1beta/models", true},
		{"/jina/v1/embeddings", true},
		{"/doubao/api/v3/chat/completions", true},

		{"/v1abc", false},
		{"/v1beta2/models", false},
		{"/V1/models", false},
		{"/admin/graphql", false},
		{"/openapi/v1/users", false},
		{"/oauth/authorize", false},
		{"/health", false},
		{"/", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := IsPublicModelAPI(tt.path); got != tt.want {
			t.Errorf("IsPublicModelAPI(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIsAPI(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/admin", true},
		{"/admin/graphql", true},
		{"/openapi/v1/users", true},
		{"/v1/models", true},

		{"/administrator", false},
		{"/oauth/authorize", false},
		{"/settings/profile", false},
		{"/assets/index-abc123.js", false},
		{"/", false},
	}

	for _, tt := range tests {
		if got := IsAPI(tt.path); got != tt.want {
			t.Errorf("IsAPI(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// Every prefix the CORS middleware treats as a public model API has to be
// recognised as an API path by the SPA fallback too, otherwise an unmatched
// route under that prefix would answer with index.html instead of JSON.
func TestPublicModelPrefixesAreAlsoAPIPaths(t *testing.T) {
	for _, prefix := range PublicModelPrefixes {
		if !IsAPI(prefix) {
			t.Errorf("IsAPI(%q) = false, public model prefixes must be API paths", prefix)
		}

		if !IsAPI(prefix + "/nonexistent") {
			t.Errorf("IsAPI(%q) = false, public model prefixes must be API paths", prefix+"/nonexistent")
		}
	}
}
