package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/stretchr/testify/require"
)

func TestConvertModelFacadeToOpenAIExtended_NilMetadata(t *testing.T) {
	facade := biz.ModelFacade{
		ID:      "gpt-4",
		Created: time.Unix(1686935002, 0).Unix(),
		OwnedBy: "configured",
	}

	result := convertModelFacadeToOpenAIExtended(facade, nil)

	require.Equal(t, "gpt-4", result.ID)
	require.Equal(t, "model", result.Object)
	require.Equal(t, "configured", result.OwnedBy)
	require.Equal(t, int64(1686935002), result.Created)
	require.Empty(t, result.Name)
	require.Nil(t, result.Modalities)
	require.Nil(t, result.Capabilities)
	require.Nil(t, result.ReasoningOptions)
	require.Nil(t, result.Pricing)
}

func TestConvertModelFacadeToOpenAIExtended_PreservesEmptyConfiguredDeveloper(t *testing.T) {
	emptyDeveloper := ""
	result := convertModelFacadeToOpenAIExtended(biz.ModelFacade{
		ID:      "owner-model",
		OwnedBy: "configured",
		Metadata: &objects.ModelMetadataPatch{
			Developer: &emptyDeveloper,
		},
	}, nil)

	require.Empty(t, result.OwnedBy, "facade conversion must retain legacy configured-model behavior")
}

func TestConvertModelFacadeToOpenAIExtended_CompleteMetadata(t *testing.T) {
	name := "GPT-4"
	description := "GPT-4 is a large multimodal model"
	developer := "openai"
	modelType := "chat"
	icon := "OpenAI"
	vision := true
	toolCall := true
	reasoning := true
	contextLength := 8192
	maxOutputTokens := 4096
	modalitiesInput := []string{"text", "image"}
	modalitiesOutput := []string{"text"}
	inputCost := 0.03
	outputCost := 0.06
	cacheReadCost := 0.015
	cacheWriteCost := 0.03
	facade := biz.ModelFacade{
		ID:      "gpt-4",
		Created: time.Unix(1686935002, 0).Unix(),
		OwnedBy: "configured",
		Metadata: &objects.ModelMetadataPatch{
			Name:        &name,
			Description: &description,
			Developer:   &developer,
			Type:        &modelType,
			Icon:        &icon,
			Vision:      &vision,
			ToolCall:    &toolCall,
			Reasoning:   &objects.ModelCardReasoningPatch{Supported: &reasoning},
			Modalities:  &objects.ModelCardModalitiesPatch{Input: &modalitiesInput, Output: &modalitiesOutput},
			Limit:       &objects.ModelCardLimitPatch{Context: &contextLength, Output: &maxOutputTokens},
			Cost: &objects.ModelCardCostPatch{
				Input:      &inputCost,
				Output:     &outputCost,
				CacheRead:  &cacheReadCost,
				CacheWrite: &cacheWriteCost,
			},
		},
	}

	result := convertModelFacadeToOpenAIExtended(facade, nil)

	require.Equal(t, "gpt-4", result.ID)
	require.Equal(t, "GPT-4", result.Name)
	require.Equal(t, description, result.Description)
	require.Equal(t, "openai", result.OwnedBy)
	require.Equal(t, "chat", result.Type)
	require.Equal(t, "OpenAI", result.Icon)
	require.NotNil(t, result.Capabilities)
	require.True(t, result.Capabilities.Vision)
	require.True(t, result.Capabilities.ToolCall)
	require.True(t, result.Capabilities.Reasoning)
	require.Equal(t, []ReasoningOption{{
		Type:   "effort",
		Values: []string{"low", "medium", "high", "xhigh", "max"},
	}}, result.ReasoningOptions)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"reasoning_options":[{"type":"effort","values":["low","medium","high","xhigh","max"]}]`)
	require.Equal(t, 8192, result.ContextLength)
	require.Equal(t, 4096, result.MaxOutputTokens)
	require.Equal(t, modalitiesInput, result.Modalities.Input)
	require.Equal(t, modalitiesOutput, result.Modalities.Output)
	require.NotNil(t, result.Pricing)
	require.Equal(t, inputCost, result.Pricing.Input)
	require.Equal(t, outputCost, result.Pricing.Output)
	require.Equal(t, cacheReadCost, result.Pricing.CacheRead)
	require.Equal(t, cacheWriteCost, result.Pricing.CacheWrite)
	require.Equal(t, "per_1m_tokens", result.Pricing.Unit)
	require.Equal(t, "USD", result.Pricing.Currency)
}

func TestConvertModelFacadeToOpenAIExtended_RespectsInclude(t *testing.T) {
	name := "Hidden name"
	developer := "moonshot"
	vision := true
	reasoning := true
	contextLength := 1_000_000
	input := []string{"text", "image"}
	facade := biz.ModelFacade{
		ID:      "custom-model",
		OwnedBy: "openai",
		Metadata: &objects.ModelMetadataPatch{
			Name:       &name,
			Developer:  &developer,
			Vision:     &vision,
			Reasoning:  &objects.ModelCardReasoningPatch{Supported: &reasoning},
			Modalities: &objects.ModelCardModalitiesPatch{Input: &input},
			Limit:      &objects.ModelCardLimitPatch{Context: &contextLength},
		},
	}

	result := convertModelFacadeToOpenAIExtended(facade, map[string]bool{
		"capabilities":   true,
		"context_length": true,
	})

	require.Equal(t, "moonshot", result.OwnedBy)
	require.Empty(t, result.Name)
	require.Nil(t, result.Modalities)
	require.NotNil(t, result.Capabilities)
	require.True(t, result.Capabilities.Vision)
	require.Nil(t, result.ReasoningOptions)
	require.Equal(t, 1_000_000, result.ContextLength)
	require.Nil(t, result.Pricing)

	reasoningOnly := convertModelFacadeToOpenAIExtended(facade, map[string]bool{
		"reasoning_options": true,
	})
	require.Nil(t, reasoningOnly.Capabilities)
	require.Zero(t, reasoningOnly.ContextLength)
	require.Equal(t, []ReasoningOption{{
		Type:   "effort",
		Values: []string{"low", "medium", "high", "xhigh", "max"},
	}}, reasoningOnly.ReasoningOptions)
}

func TestConvertModelFacadeToOpenAIExtended_OmitsReasoningOptionsWhenUnsupported(t *testing.T) {
	reasoning := false
	result := convertModelFacadeToOpenAIExtended(biz.ModelFacade{
		ID: "non-reasoning-model",
		Metadata: &objects.ModelMetadataPatch{
			Reasoning: &objects.ModelCardReasoningPatch{Supported: &reasoning},
		},
	}, nil)

	require.NotNil(t, result.Capabilities)
	require.False(t, result.Capabilities.Reasoning)
	require.Nil(t, result.ReasoningOptions)
}
