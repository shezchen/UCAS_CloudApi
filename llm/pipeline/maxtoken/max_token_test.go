package maxtoken

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/looplj/axonhub/llm"
)

func TestEnsureMaxTokens(t *testing.T) {
	defaultValue := int64(200)
	decorator := EnsureMaxTokens(defaultValue)

	content := "Hello"
	req := &llm.Request{
		Messages: []llm.Message{
			{Role: "user", Content: llm.MessageContent{Content: &content}},
		},
		// MaxTokens is nil initially
	}

	result, err := decorator.OnInboundLlmRequest(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, &defaultValue, result.MaxTokens)
}

func TestEnsureMaxTokens_ExistingValue(t *testing.T) {
	defaultValue := int64(200)
	decorator := EnsureMaxTokens(defaultValue)

	existingValue := int64(100)
	content := "Hello"
	req := &llm.Request{
		Messages: []llm.Message{
			{Role: "user", Content: llm.MessageContent{Content: &content}},
		},
		MaxTokens: &existingValue, // Already has a value
	}

	result, err := decorator.OnInboundLlmRequest(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, &existingValue, result.MaxTokens) // Should remain unchanged
	assert.NotEqual(t, &defaultValue, result.MaxTokens)
}

func TestEnsureMaxTokens_ClampsAboveDefault(t *testing.T) {
	decorator := EnsureMaxTokens(200)

	requested := int64(4000)
	req := &llm.Request{MaxTokens: &requested}

	result, err := decorator.OnInboundLlmRequest(context.Background(), req)
	assert.NoError(t, err)
	assert.Equal(t, int64(200), *result.MaxTokens)
	assert.Equal(t, int64(4000), requested, "clamping must not write through the caller's pointer")
}

func TestEnsureMaxTokens_DoesNotShareCeilingAcrossRequests(t *testing.T) {
	decorator := EnsureMaxTokens(200)
	ctx := context.Background()

	first, err := decorator.OnInboundLlmRequest(ctx, &llm.Request{})
	assert.NoError(t, err)

	// Anything writing through the applied pointer must not move the ceiling
	// for later requests.
	*first.MaxTokens = 1

	second, err := decorator.OnInboundLlmRequest(ctx, &llm.Request{})
	assert.NoError(t, err)
	assert.Equal(t, int64(200), *second.MaxTokens)
	assert.NotSame(t, first.MaxTokens, second.MaxTokens)
}
