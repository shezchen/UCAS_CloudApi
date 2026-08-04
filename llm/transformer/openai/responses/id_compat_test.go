package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestValidResponsesItemIDOrEmpty_IsTypeAware(t *testing.T) {
	tests := []struct {
		name     string
		itemType string
		id       string
		expected string
	}{
		{name: "reasoning keeps rs namespace", itemType: "reasoning", id: "rs_provider_1", expected: "rs_provider_1"},
		{name: "function keeps fc namespace", itemType: "function_call", id: "fc_provider_1", expected: "fc_provider_1"},
		{name: "custom keeps ctc namespace", itemType: "custom_tool_call", id: "ctc_provider_1", expected: "ctc_provider_1"},
		{name: "message keeps msg namespace", itemType: "message", id: " msg_provider_1 ", expected: "msg_provider_1"},
		{name: "agent message keeps amsg namespace", itemType: "agent_message", id: "amsg_provider_1", expected: "amsg_provider_1"},
		{name: "legacy reasoning id is omitted", itemType: "reasoning", id: "item_legacy", expected: ""},
		{name: "function cannot use reasoning namespace", itemType: "function_call", id: "rs_wrong_type", expected: ""},
		{name: "custom cannot use function namespace", itemType: "custom_tool_call", id: "fc_wrong_type", expected: ""},
		{name: "agent message cannot use message namespace", itemType: "agent_message", id: "msg_amsg_wrong_type", expected: ""},
		{name: "empty suffix is omitted", itemType: "message", id: "msg_", expected: ""},
		{name: "unknown item type is not guessed", itemType: "future_item", id: "future_1", expected: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.expected, validResponsesItemIDOrEmpty(tt.itemType, tt.id))
		})
	}
}

func TestNormalizeRequestInputItemIDs_MigratesKnownTypesWithoutTouchingConversationState(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"previous_response_id":"resp_previous",
		"input":[
			{"type":"reasoning","id":"item_legacy_reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"keep"}]},
			{"type":"function_call","id":"call_wrong_namespace","call_id":"call_function","name":"shell","arguments":"{}"},
			{"type":"custom_tool_call","id":"fc_wrong_namespace","call_id":"call_custom","name":"apply_patch","input":"patch"},
			{"type":"message","id":"item_legacy_message","role":"assistant","content":[]},
			{"role":"user","id":"msg_valid_implicit","content":[]},
			{"type":"reasoning","id":" rs_valid_trimmed ","encrypted_content":"opaque-valid"},
			{"type":"function_call","id":"fc_valid","call_id":"call_valid","name":"shell","arguments":"{}"},
			{"type":"custom_tool_call","id":"ctc_valid","call_id":"call_valid_custom","name":"apply_patch","input":"patch"},
			{"type":"message","id":"","role":"user","content":[]},
			{"type":"reasoning","id":null,"summary":[]},
			{"type":"function_call_output","id":"item_extension_owned","call_id":"call_function","output":"ok"},
			{"type":"future_item","id":"item_future_owned","payload":{"id":"nested_untouched"}}
		]
	}`)

	normalized, err := NormalizeRequestInputItemIDs(body)
	require.NoError(t, err)

	for _, index := range []int{0, 1, 2, 3, 8, 9} {
		require.False(t, gjson.GetBytes(normalized, fmt.Sprintf("input.%d.id", index)).Exists(), "input[%d] id", index)
	}
	require.Equal(t, "msg_valid_implicit", gjson.GetBytes(normalized, "input.4.id").String())
	require.Equal(t, "rs_valid_trimmed", gjson.GetBytes(normalized, "input.5.id").String())
	require.Equal(t, "fc_valid", gjson.GetBytes(normalized, "input.6.id").String())
	require.Equal(t, "ctc_valid", gjson.GetBytes(normalized, "input.7.id").String())
	require.Equal(t, "item_extension_owned", gjson.GetBytes(normalized, "input.10.id").String())
	require.Equal(t, "item_future_owned", gjson.GetBytes(normalized, "input.11.id").String())
	require.Equal(t, "nested_untouched", gjson.GetBytes(normalized, "input.11.payload.id").String())
	require.Equal(t, "call_function", gjson.GetBytes(normalized, "input.1.call_id").String())
	require.Equal(t, "call_custom", gjson.GetBytes(normalized, "input.2.call_id").String())
	require.Equal(t, "opaque", gjson.GetBytes(normalized, "input.0.encrypted_content").String())
	require.Equal(t, "keep", gjson.GetBytes(normalized, "input.0.summary.0.text").String())
	require.Equal(t, "resp_previous", gjson.GetBytes(normalized, "previous_response_id").String())
}

func TestNormalizeRequestInputItemIDs_LeavesNonArrayInputUnchanged(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","client_extension":{"id":"item_client"}}`)
	normalized, err := NormalizeRequestInputItemIDs(body)
	require.NoError(t, err)
	require.Equal(t, body, normalized)
}

func TestNormalizeRequestInputItemIDs_UsesAgentMessageNamespace(t *testing.T) {
	body := []byte(`{"input":[{"type":"agent_message","id":"amsg_native","content":[]},{"type":"agent_message","id":"msg_amsg_legacy","content":[]}]}`)

	normalized, err := NormalizeRequestInputItemIDs(body)
	require.NoError(t, err)
	require.Equal(t, "amsg_native", gjson.GetBytes(normalized, "input.0.id").String())
	require.False(t, gjson.GetBytes(normalized, "input.1.id").Exists())
}

func TestResponsesLegacyItemIDsAreOmittedOnUpstreamReplay(t *testing.T) {
	inbound := NewInboundTransformer()
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"store":false,
		"input":[
			{"type":"reasoning","id":"item_legacy_reasoning","encrypted_content":"opaque-legacy-reasoning","summary":[{"type":"summary_text","text":"keep this summary"}]},
			{"type":"function_call","id":"item_legacy_function","call_id":"call_legacy_function","name":"run_command","arguments":"{}"},
			{"type":"custom_tool_call","id":"fc_wrong_custom_namespace","call_id":"call_legacy_custom","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"message","id":"item_legacy_message","role":"user","content":[{"type":"input_text","text":"continue"}]},
			{"type":"message","id":"msg_valid_message","role":"assistant","content":[{"type":"output_text","text":"previous answer"}]}
		]
	}`)

	llmReq, err := inbound.TransformRequest(context.Background(), &httpclient.Request{
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    raw,
	})
	require.NoError(t, err)

	httpReq, err := outbound.TransformRequest(context.Background(), llmReq)
	require.NoError(t, err)

	var replay Request
	require.NoError(t, json.Unmarshal(httpReq.Body, &replay))

	var reasoning, functionCall, customTool, userMessage, assistantMessage *Item
	for i := range replay.Input.Items {
		item := &replay.Input.Items[i]
		switch item.Type {
		case "reasoning":
			reasoning = item
		case "function_call":
			functionCall = item
		case "custom_tool_call":
			customTool = item
		case "message":
			if item.Role == "user" {
				userMessage = item
			} else if item.Role == "assistant" {
				assistantMessage = item
			}
		}
	}

	require.NotNil(t, reasoning)
	require.Empty(t, reasoning.ID, "legacy reasoning IDs must not be sent upstream")
	require.Equal(t, "opaque-legacy-reasoning", lo.FromPtr(reasoning.EncryptedContent))
	require.Equal(t, "keep this summary", reasoning.Summary[0].Text)

	require.NotNil(t, functionCall)
	require.Empty(t, functionCall.ID, "legacy function item IDs must not be sent upstream")
	require.Equal(t, "call_legacy_function", functionCall.CallID, "call_id is separate and must survive")

	require.NotNil(t, customTool)
	require.Empty(t, customTool.ID, "wrong custom-tool item IDs must not be sent upstream")
	require.Equal(t, "call_legacy_custom", customTool.CallID, "custom tool call_id must survive")

	require.NotNil(t, userMessage)
	require.Empty(t, userMessage.ID, "legacy message IDs must not be sent upstream")
	require.NotNil(t, assistantMessage)
	require.Equal(t, "msg_valid_message", assistantMessage.ID)
}

func TestResponsesMissingIDKeepsOpaqueReasoningAndSummary(t *testing.T) {
	messages, err := convertInputToMessages(&Input{Items: []Item{{
		Type:             "reasoning",
		EncryptedContent: lo.ToPtr("opaque-without-a-known-prefix"),
		Summary:          []ReasoningSummary{{Type: "summary_text", Text: "retain summary without an item id"}},
	}}})
	require.NoError(t, err)
	require.Len(t, messages, 1)

	replay := convertInputFromMessages(messages, llm.TransformOptions{ArrayInputs: lo.ToPtr(true)})
	require.Len(t, replay.Items, 1)
	require.Equal(t, "reasoning", replay.Items[0].Type)
	require.Empty(t, replay.Items[0].ID)
	require.Equal(t, "opaque-without-a-known-prefix", lo.FromPtr(replay.Items[0].EncryptedContent))
	require.Equal(t, "retain summary without an item id", replay.Items[0].Summary[0].Text)
}

func TestResponsesLegacyReasoningAtInput37MigratesWithoutRewritingHistory(t *testing.T) {
	items := make([]Item, 0, 38)
	for i := 0; i < 37; i++ {
		text := "history item " + string(rune('A'+i%26))
		items = append(items, Item{
			Type: "message",
			Role: "user",
			Content: &Input{Items: []Item{{
				Type: "input_text",
				Text: lo.ToPtr(text),
			}}},
		})
	}
	items = append(items, Item{
		Type:             "reasoning",
		ID:               "item_BAOrUyHLO7jCHY0N",
		EncryptedContent: lo.ToPtr("opaque-item-37"),
		Summary:          []ReasoningSummary{{Type: "summary_text", Text: "legacy item at reported index"}},
	})

	body, err := json.Marshal(Request{
		Model:              "gpt-5.6-sol",
		Store:              lo.ToPtr(false),
		PreviousResponseID: lo.ToPtr("resp_previous"),
		Input:              Input{Items: items},
	})
	require.NoError(t, err)

	inbound := NewInboundTransformer()
	llmReq, err := inbound.TransformRequest(t.Context(), &httpclient.Request{
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    body,
	})
	require.NoError(t, err)

	outbound, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)
	httpReq, err := outbound.TransformRequest(t.Context(), llmReq)
	require.NoError(t, err)

	var replay Request
	require.NoError(t, json.Unmarshal(httpReq.Body, &replay))
	require.Len(t, replay.Input.Items, 38)
	require.Equal(t, "reasoning", replay.Input.Items[37].Type)
	require.Empty(t, replay.Input.Items[37].ID)
	require.Equal(t, "opaque-item-37", lo.FromPtr(replay.Input.Items[37].EncryptedContent))
	require.Equal(t, "resp_previous", lo.FromPtr(replay.PreviousResponseID))
}

func TestResponsesOutputFallbacksUseTypedItemIDsAfterInvalidMetadata(t *testing.T) {
	legacyReasoningMetadata := map[string]any{
		responsesReasoningItemTransformerMetadataKey: map[string]any{"id": "item_legacy_reasoning"},
	}
	item, ok := buildReasoningItem(llm.Message{
		ReasoningContent:   lo.ToPtr("reasoning"),
		ReasoningSignature: lo.ToPtr("gAAAA_signature"),
	}, legacyReasoningMetadata)
	require.True(t, ok)
	require.True(t, strings.HasPrefix(item.ID, "rs_"), item.ID)

	message := llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{
			{
				ID:       "call_function",
				Function: llm.FunctionCall{Name: "run_command", Arguments: "{}"},
				TransformerMetadata: map[string]any{
					responsesToolCallItemIDTransformerMetadataKey: "item_legacy_function",
				},
			},
			{
				ID: "call_custom",
				ResponseCustomToolCall: &llm.ResponseCustomToolCall{
					CallID: "call_custom",
					Name:   "apply_patch",
					Input:  "*** Begin Patch",
				},
				TransformerMetadata: map[string]any{
					responsesToolCallItemIDTransformerMetadataKey: "fc_wrong_custom_namespace",
				},
			},
		},
	}
	response := convertToResponsesAPIResponse(&llm.Response{
		ID:      "resp_fallbacks",
		Model:   "gpt-5.6-sol",
		Choices: []llm.Choice{{Message: &message}},
	})

	var functionID, customID string
	for _, output := range response.Output {
		switch output.Type {
		case "function_call":
			functionID = output.ID
		case "custom_tool_call":
			customID = output.ID
		}
	}
	require.True(t, strings.HasPrefix(functionID, "fc_"), functionID)
	require.True(t, strings.HasPrefix(customID, "ctc_"), customID)
}

func TestResponsesStreamFallbacksUseTypedItemIDs(t *testing.T) {
	transformed, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{
		{
			ID: "resp_legacy_stream", Object: "chat.completion.chunk", Model: "gpt-5.6-sol",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{Role: "assistant"}}},
		},
		{
			ID: "resp_legacy_stream", Object: "chat.completion.chunk", Model: "gpt-5.6-sol",
			TransformerMetadata: map[string]any{
				responsesReasoningItemTransformerMetadataKey: map[string]any{
					"id": "item_legacy_reasoning", "done": true,
				},
			},
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{
				ID: "item_legacy_reasoning", ReasoningSignature: lo.ToPtr("opaque-stream-reasoning"),
			}}},
		},
		{
			ID: "resp_legacy_stream", Object: "chat.completion.chunk", Model: "gpt-5.6-sol",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
				Index: 0,
				ID:    "call_stream_function",
				TransformerMetadata: map[string]any{
					responsesToolCallItemIDTransformerMetadataKey: "call_stream_function",
				},
				Function: llm.FunctionCall{Name: "run_command", Arguments: "{}"},
			}}}}},
		},
		{
			ID: "resp_legacy_stream", Object: "chat.completion.chunk", Model: "gpt-5.6-sol",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{}, FinishReason: lo.ToPtr("tool_calls")}},
		},
		{
			ID: "resp_legacy_stream", Object: "chat.completion.chunk", Model: "gpt-5.6-sol",
			Usage: &llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
		},
	}))
	require.NoError(t, err)

	var reasoningID, functionID string
	for transformed.Next() {
		var event StreamEvent
		require.NoError(t, json.Unmarshal(transformed.Current().Data, &event))
		if event.Type != StreamEventTypeOutputItemAdded || event.Item == nil {
			continue
		}
		switch event.Item.Type {
		case "reasoning":
			reasoningID = event.Item.ID
		case "function_call":
			functionID = event.Item.ID
			require.Equal(t, "call_stream_function", event.Item.CallID)
		}
	}
	require.NoError(t, transformed.Err())
	require.True(t, strings.HasPrefix(reasoningID, "rs_"), reasoningID)
	require.True(t, strings.HasPrefix(functionID, "fc_"), functionID)
}

func TestResponsesMultiReasoningSidecarNormalizesIDsOnce(t *testing.T) {
	response := &llm.Response{}
	output := []Item{
		{
			Type:             "reasoning",
			ID:               "item_legacy_reasoning",
			EncryptedContent: lo.ToPtr("opaque-first"),
			Summary:          []ReasoningSummary{{Type: "summary_text", Text: "first"}},
		},
		{
			Type:             "reasoning",
			ID:               "rs_valid_second",
			EncryptedContent: lo.ToPtr("opaque-second"),
			Summary:          []ReasoningSummary{{Type: "summary_text", Text: "second"}},
		},
		{
			Type:      "function_call",
			ID:        "item_legacy_function",
			CallID:    "call_function",
			Name:      "run_command",
			Arguments: "{}",
		},
		{
			Type:   "custom_tool_call",
			ID:     "fc_wrong_custom_namespace",
			CallID: "call_custom",
			Name:   "apply_patch",
			Input:  lo.ToPtr("*** Begin Patch"),
		},
	}

	require.NoError(t, preserveResponsesOutputItems(response, output))
	firstRead, ok := getPreservedResponsesOutputItems(response)
	require.True(t, ok)
	secondRead, ok := getPreservedResponsesOutputItems(response)
	require.True(t, ok)
	require.Equal(t, firstRead, secondRead, "sidecar fallback IDs must remain stable after storage")

	require.True(t, strings.HasPrefix(firstRead[0].ID, "rs_"), firstRead[0].ID)
	require.Equal(t, "opaque-first", lo.FromPtr(firstRead[0].EncryptedContent))
	require.Equal(t, "first", firstRead[0].Summary[0].Text)
	require.Equal(t, "rs_valid_second", firstRead[1].ID)
	require.True(t, strings.HasPrefix(firstRead[2].ID, "fc_"), firstRead[2].ID)
	require.Equal(t, "call_function", firstRead[2].CallID)
	require.True(t, strings.HasPrefix(firstRead[3].ID, "ctc_"), firstRead[3].ID)
	require.Equal(t, "call_custom", firstRead[3].CallID)
}
