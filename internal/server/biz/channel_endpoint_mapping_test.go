package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestDefaultEndpointsForChannelType_UseLLMAPIFormatValues(t *testing.T) {
	tests := []struct {
		name     string
		typ      channel.Type
		expected []string
	}{
		{
			name: "openai defaults to chat completions",
			typ:  channel.TypeOpenai,
			expected: []string{
				llm.APIFormatOpenAIChatCompletion.String(),
				llm.APIFormatOpenAIEmbedding.String(),
				llm.APIFormatOpenAIImageGeneration.String(),
				llm.APIFormatOpenAIImageEdit.String(),
				llm.APIFormatOpenAIImageVariation.String(),
				llm.APIFormatOpenAIVideo.String(),
				llm.APIFormatOpenAISpeech.String(),
				llm.APIFormatOpenAITranscription.String(),
				llm.APIFormatOpenAITranslation.String(),
			},
		},
		{
			name: "atlascloud keeps openai-compatible built-in endpoints",
			typ:  channel.TypeAtlascloud,
			expected: []string{
				llm.APIFormatOpenAIChatCompletion.String(),
				llm.APIFormatOpenAIEmbedding.String(),
				llm.APIFormatOpenAIImageGeneration.String(),
				llm.APIFormatOpenAIImageEdit.String(),
				llm.APIFormatOpenAIImageVariation.String(),
				llm.APIFormatOpenAIVideo.String(),
			},
		},
		{
			name: "vercel keeps openai-compatible built-in endpoints for compatibility",
			typ:  channel.TypeVercel,
			expected: []string{
				llm.APIFormatOpenAIChatCompletion.String(),
				llm.APIFormatOpenAIEmbedding.String(),
				llm.APIFormatOpenAIImageGeneration.String(),
				llm.APIFormatOpenAIImageEdit.String(),
				llm.APIFormatOpenAIImageVariation.String(),
				llm.APIFormatOpenAIVideo.String(),
			},
		},
		{
			name: "github models keeps openai-compatible built-in endpoints for compatibility",
			typ:  channel.TypeGithub,
			expected: []string{
				llm.APIFormatOpenAIChatCompletion.String(),
				llm.APIFormatOpenAIEmbedding.String(),
				llm.APIFormatOpenAIImageGeneration.String(),
				llm.APIFormatOpenAIImageEdit.String(),
				llm.APIFormatOpenAIImageVariation.String(),
				llm.APIFormatOpenAIVideo.String(),
			},
		},
		{
			name:     "cline exposes chat only",
			typ:      channel.TypeCline,
			expected: []string{llm.APIFormatOpenAIChatCompletion.String()},
		},
		{
			name:     "minimax exposes chat only",
			typ:      channel.TypeMinimax,
			expected: []string{llm.APIFormatOpenAIChatCompletion.String()},
		},
		{
			name:     "xiaomi exposes chat only",
			typ:      channel.TypeXiaomi,
			expected: []string{llm.APIFormatOpenAIChatCompletion.String()},
		},
		{
			name:     "nanogpt responses defaults to responses",
			typ:      channel.TypeNanogptResponses,
			expected: []string{llm.APIFormatOpenAIResponse.String()},
		},
		{
			name: "codex exposes responses plus image generation and edit",
			typ:  channel.TypeCodex,
			expected: []string{
				llm.APIFormatOpenAIResponse.String(),
				llm.APIFormatOpenAIImageGeneration.String(),
				llm.APIFormatOpenAIImageEdit.String(),
			},
		},
		{
			name:     "jina exposes rerank and embedding",
			typ:      channel.TypeJina,
			expected: []string{llm.APIFormatJinaRerank.String(), llm.APIFormatJinaEmbedding.String()},
		},
		{
			name: "gemini exposes contents and embeddings",
			typ:  channel.TypeGemini,
			expected: []string{
				llm.APIFormatGeminiContents.String(),
				llm.APIFormatGeminiEmbedding.String(),
			},
		},
		{
			name: "nanogpt exposes chat plus delegated openai capability endpoints",
			typ:  channel.TypeNanogpt,
			expected: []string{
				llm.APIFormatOpenAIChatCompletion.String(),
				llm.APIFormatOpenAIEmbedding.String(),
				llm.APIFormatOpenAIImageGeneration.String(),
				llm.APIFormatOpenAIImageEdit.String(),
				llm.APIFormatOpenAIImageVariation.String(),
				llm.APIFormatOpenAIVideo.String(),
				llm.APIFormatOpenAISpeech.String(),
				llm.APIFormatOpenAITranscription.String(),
				llm.APIFormatOpenAITranslation.String(),
			},
		},
		{
			name: "doubao exposes chat and seedance video",
			typ:  channel.TypeDoubao,
			expected: []string{
				llm.APIFormatOpenAIChatCompletion.String(),
				llm.APIFormatSeedanceVideo.String(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoints := DefaultEndpointsForChannelType(tt.typ)
			require.Len(t, endpoints, len(tt.expected))

			actual := make([]string, 0, len(endpoints))
			for _, endpoint := range endpoints {
				actual = append(actual, endpoint.APIFormat)
			}

			require.Equal(t, tt.expected, actual)
		})
	}
}

func TestValidateEndpoints(t *testing.T) {
	t.Run("empty api_format returns error", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{{APIFormat: ""}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "api_format is required")
	})

	t.Run("unsupported api_format returns error", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{{APIFormat: "unknown/format"}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported api_format")
	})

	t.Run("duplicate api_format returns error", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{
			{APIFormat: llm.APIFormatOpenAIChatCompletion.String()},
			{APIFormat: llm.APIFormatOpenAIChatCompletion.String()},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate api_format")
	})

	t.Run("path must start with slash", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{
			{APIFormat: llm.APIFormatOpenAIChatCompletion.String(), Path: "v1/chat"},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "path must start with '/'")
	})

	t.Run("path must not be a full URL", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{
			{APIFormat: llm.APIFormatOpenAIChatCompletion.String(), Path: "https://example.com/v1"},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "path must not be a full URL")
	})

	t.Run("websocket transport only supports responses", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{
			{APIFormat: llm.APIFormatOpenAIChatCompletion.String(), Transport: objects.ChannelEndpointTransportWebSocket},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "websocket transport only supports")
	})

	t.Run("websocket responses endpoint passes validation", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{
			{APIFormat: llm.APIFormatOpenAIResponse.String(), Transport: objects.ChannelEndpointTransportWebSocket},
		})
		require.NoError(t, err)
	})

	t.Run("websocket compact responses endpoint passes validation", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{
			{APIFormat: llm.APIFormatOpenAIResponseCompact.String(), Transport: objects.ChannelEndpointTransportWebSocket},
		})
		require.NoError(t, err)
	})

	t.Run("valid endpoints pass validation", func(t *testing.T) {
		err := ValidateEndpoints([]objects.ChannelEndpoint{
			{APIFormat: llm.APIFormatOpenAIChatCompletion.String()},
			{APIFormat: llm.APIFormatGeminiContents.String(), Path: "/custom/gemini"},
		})
		require.NoError(t, err)
	})

	t.Run("empty endpoints list passes validation", func(t *testing.T) {
		err := ValidateEndpoints(nil)
		require.NoError(t, err)
	})
}

func TestValidateDonationChannelConfigurationAllowsPublicProxyAndRejectsPrivateDestinations(t *testing.T) {
	publicBaseURL := "https://8.8.8.8/v1"

	err := ValidateDonationChannelConfiguration(context.Background(), channel.TypeOpenai, &publicBaseURL, nil, &objects.ChannelSettings{
		Proxy: &httpclient.ProxyConfig{
			Type: httpclient.ProxyTypeURL,
			URL:  "http://127.0.0.1:8080",
		},
	}, nil)
	require.ErrorContains(t, err, "restricted address")

	err = ValidateDonationChannelConfiguration(context.Background(), channel.TypeOpenai, &publicBaseURL, nil, &objects.ChannelSettings{
		Proxy: &httpclient.ProxyConfig{
			Type: httpclient.ProxyTypeURL,
			URL:  "http://8.8.8.8:8080",
		},
	}, nil)
	require.NoError(t, err)

	err = ValidateDonationChannelConfiguration(context.Background(), channel.TypeOpenai, &publicBaseURL, nil, nil, []objects.ChannelEndpoint{
		{BaseURL: "https://169.254.169.254/latest/meta-data"},
	})
	require.ErrorContains(t, err, "endpoint[0]")
}

func TestValidateDonationChannelConfigurationValidatesGCPServiceAccountURLs(t *testing.T) {
	valid := objects.ChannelCredentials{GCP: &objects.GCPCredential{
		Region: "us-central1",
		JSONData: `{
			"type":"service_account",
			"token_uri":"https://oauth2.googleapis.com/token",
			"auth_uri":"https://accounts.google.com/o/oauth2/auth",
			"auth_provider_x509_cert_url":"https://www.googleapis.com/oauth2/v1/certs",
			"client_x509_cert_url":"https://www.googleapis.com/robot/v1/metadata/x509/test%40example.com",
			"universe_domain":"googleapis.com"
		}`,
	}}
	require.NoError(t, ValidateDonationChannelConfiguration(
		context.Background(), channel.TypeAnthropicGcp, nil, &valid, nil, nil,
	))

	tests := []struct {
		name       string
		region     string
		credential string
		errorText  string
	}{
		{
			name:       "region cannot inject a host",
			region:     "us-central1.evil.example",
			credential: valid.GCP.JSONData,
			errorText:  "GCP region",
		},
		{
			name:       "external account credential source is rejected",
			region:     "global",
			credential: `{"type":"external_account","token_url":"http://169.254.169.254/token"}`,
			errorText:  "service_account",
		},
		{
			name:       "token URI cannot target metadata",
			region:     "global",
			credential: `{"type":"service_account","token_uri":"https://169.254.169.254/token"}`,
			errorText:  "token_uri",
		},
		{
			name:       "token URI path must be canonical",
			region:     "global",
			credential: `{"type":"service_account","token_uri":"https://oauth2.googleapis.com/redirect"}`,
			errorText:  "token_uri",
		},
		{
			name:       "auth URI cannot target another host",
			region:     "global",
			credential: `{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token","auth_uri":"https://example.com/o/oauth2/auth"}`,
			errorText:  "auth_uri",
		},
		{
			name:       "certificate URL cannot target another host",
			region:     "global",
			credential: `{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token","client_x509_cert_url":"https://example.com/robot/v1/metadata/x509/a"}`,
			errorText:  "client_x509_cert_url",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			credentials := objects.ChannelCredentials{GCP: &objects.GCPCredential{
				Region:   tt.region,
				JSONData: tt.credential,
			}}
			err := ValidateDonationChannelConfiguration(
				context.Background(), channel.TypeAnthropicGcp, nil, &credentials, nil, nil,
			)
			require.ErrorContains(t, err, tt.errorText)
		})
	}
}

func TestPrimaryEndpointTransport(t *testing.T) {
	t.Run("infers websocket from primary base url", func(t *testing.T) {
		transport := primaryEndpointTransport(&ent.Channel{
			BaseURL: "wss://api.openai.com/v1#",
		}, llm.APIFormatOpenAIResponse.String())

		require.Equal(t, objects.ChannelEndpointTransportWebSocket, transport)
	})

	t.Run("uses matching endpoint transport override", func(t *testing.T) {
		transport := primaryEndpointTransport(&ent.Channel{
			BaseURL: "https://api.openai.com/v1",
			Endpoints: []objects.ChannelEndpoint{
				{APIFormat: llm.APIFormatOpenAIResponse.String(), Transport: objects.ChannelEndpointTransportWebSocket},
			},
		}, llm.APIFormatOpenAIResponse.String())

		require.Equal(t, objects.ChannelEndpointTransportWebSocket, transport)
	})
}

func TestResolveEndpoints_MergesDefaultsAndUserOverrides(t *testing.T) {
	ch := &Channel{
		Channel: &ent.Channel{
			Type: channel.TypeOpenai,
			Endpoints: []objects.ChannelEndpoint{
				{APIFormat: llm.APIFormatOpenAIChatCompletion.String(), Path: "/v1/custom/chat"},
				{APIFormat: llm.APIFormatGeminiContents.String(), Path: "/v1/gemini"},
			},
		},
	}

	endpoints := ch.ResolveEndpoints()
	require.Equal(t, []objects.ChannelEndpoint{
		{APIFormat: llm.APIFormatOpenAIChatCompletion.String(), Path: "/v1/custom/chat"},
		{APIFormat: llm.APIFormatOpenAIEmbedding.String()},
		{APIFormat: llm.APIFormatOpenAIImageGeneration.String()},
		{APIFormat: llm.APIFormatOpenAIImageEdit.String()},
		{APIFormat: llm.APIFormatOpenAIImageVariation.String()},
		{APIFormat: llm.APIFormatOpenAIVideo.String()},
		{APIFormat: llm.APIFormatOpenAISpeech.String()},
		{APIFormat: llm.APIFormatOpenAITranscription.String()},
		{APIFormat: llm.APIFormatOpenAITranslation.String()},
		{APIFormat: llm.APIFormatGeminiContents.String(), Path: "/v1/gemini"},
	}, endpoints)
}

func TestSupportedAPIFormats_UsesLLMAPIFormatValues(t *testing.T) {
	formats := []string{
		llm.APIFormatOpenAIChatCompletion.String(),
		llm.APIFormatOpenAICompletion.String(),
		llm.APIFormatOpenAIResponse.String(),
		llm.APIFormatOpenAIResponseCompact.String(),
		llm.APIFormatOpenAIEmbedding.String(),
		llm.APIFormatOpenAIImageGeneration.String(),
		llm.APIFormatOpenAIImageEdit.String(),
		llm.APIFormatOpenAIImageVariation.String(),
		llm.APIFormatOpenAIVideo.String(),
		llm.APIFormatOpenAISpeech.String(),
		llm.APIFormatOpenAITranscription.String(),
		llm.APIFormatOpenAITranslation.String(),
		llm.APIFormatAnthropicMessage.String(),
		llm.APIFormatGeminiContents.String(),
		llm.APIFormatGeminiEmbedding.String(),
		llm.APIFormatJinaRerank.String(),
		llm.APIFormatJinaEmbedding.String(),
	}

	for _, format := range formats {
		_, ok := SupportedAPIFormats[format]
		require.Truef(t, ok, "expected %s to be supported", format)
	}
}
