package responses

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestResponsesRequestRoundTrip_InterleavedMessagesKeepTheirOwnProviderIDs(t *testing.T) {
	inbound := NewInboundTransformer()
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"reasoning","id":"rs_first","encrypted_content":"gAAAA_first","summary":[{"type":"summary_text","text":"first reasoning"}]},
			{"type":"message","id":"msg_provider_first","role":"assistant","content":[{"type":"output_text","text":"first answer"}]},
			{"type":"message","id":"msg_user_first","role":"user","content":[{"type":"input_text","text":"continue"}]},
			{"type":"reasoning","id":"rs_second","encrypted_content":"gAAAA_second","summary":[{"type":"summary_text","text":"second reasoning"}]},
			{"type":"message","id":"msg_provider_refusal","role":"assistant","content":[{"type":"refusal","refusal":"cannot comply"}]},
			{"type":"message","id":"msg_user_second","role":"user","content":[{"type":"input_text","text":"try a safe version"}]},
			{"type":"reasoning","id":"rs_third","encrypted_content":"gAAAA_third","summary":[]},
			{"type":"message","id":"msg_provider_third","role":"assistant","content":[{"type":"output_text","text":"third answer"}]}
		]
	}`)

	llmReq, err := inbound.TransformRequest(t.Context(), &httpclient.Request{
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    raw,
	})
	require.NoError(t, err)
	require.Len(t, llmReq.Messages, 5)
	require.Equal(t, "msg_provider_first", llmReq.Messages[0].ID)
	require.Equal(t, "msg_user_first", llmReq.Messages[1].ID)
	require.Equal(t, "msg_provider_refusal", llmReq.Messages[2].ID)
	require.Equal(t, "cannot comply", llmReq.Messages[2].Refusal)
	require.Equal(t, "msg_user_second", llmReq.Messages[3].ID)
	require.Equal(t, "msg_provider_third", llmReq.Messages[4].ID)

	httpReq, err := outbound.TransformRequest(t.Context(), llmReq)
	require.NoError(t, err)

	var replay Request
	require.NoError(t, json.Unmarshal(httpReq.Body, &replay))

	var (
		messageIDs   []string
		reasoningIDs []string
	)
	for _, item := range replay.Input.Items {
		switch item.Type {
		case "message":
			messageIDs = append(messageIDs, item.ID)
			require.True(t, isValidMessageItemID(item.ID), "outbound message id must be replayable: %q", item.ID)
		case "reasoning":
			reasoningIDs = append(reasoningIDs, item.ID)
			require.False(t, isValidMessageItemID(item.ID), "reasoning item id must not be emitted as a message id: %q", item.ID)
		}
	}

	require.Equal(t, []string{
		"msg_provider_first",
		"msg_user_first",
		"msg_provider_refusal",
		"msg_user_second",
		"msg_provider_third",
	}, messageIDs)
	require.Equal(t, []string{"rs_first", "rs_second", "rs_third"}, reasoningIDs,
		"reasoning item identity must survive replay with following assistant messages")
}

func TestResponsesGeneratedMessageIDs_ConsecutiveMessagesAfterReasoningRemainSeparateOnReplay(t *testing.T) {
	response := convertToResponsesAPIResponse(&llm.Response{
		ID:      "resp_generated_messages",
		Object:  "chat.completion",
		Model:   "gpt-5.6-sol",
		Created: 1700000000,
		Choices: []llm.Choice{
			{
				Index: 0,
				Message: &llm.Message{
					Role:               "assistant",
					ReasoningContent:   lo.ToPtr("first reasoning"),
					ReasoningSignature: lo.ToPtr("gAAAA_first"),
					Content:            llm.MessageContent{Content: lo.ToPtr("first answer")},
				},
			},
			{
				Index: 1,
				Message: &llm.Message{
					ID:                 "item_legacy_message_id",
					Role:               "assistant",
					ReasoningContent:   lo.ToPtr("second reasoning"),
					ReasoningSignature: lo.ToPtr("gAAAA_second"),
					Refusal:            "cannot comply",
				},
			},
			{
				Index: 2,
				Message: &llm.Message{
					ID:      "rs_not_a_message_id",
					Role:    "assistant",
					Content: llm.MessageContent{Content: lo.ToPtr("third answer")},
				},
			},
		},
	})

	var generated []string
	for _, item := range response.Output {
		if item.Type != "message" {
			continue
		}
		require.True(t, isValidMessageItemID(item.ID), "generated id must use msg_ prefix: %q", item.ID)
		require.NotContains(t, generated, item.ID, "separate messages must never share a generated id")
		generated = append(generated, item.ID)
	}
	require.Len(t, generated, 3)

	messages, err := convertInputToMessages(&Input{Items: response.Output})
	require.NoError(t, err)
	actualMessageIDs := make([]string, 0, len(messages))
	actualRefusals := make([]string, 0, len(messages))
	for _, message := range messages {
		actualMessageIDs = append(actualMessageIDs, message.ID)
		actualRefusals = append(actualRefusals, message.Refusal)
	}
	assert.Equal(t, generated, actualMessageIDs, "a reasoning item must not consume and overwrite a second assistant message")
	assert.Equal(t, []string{"", "cannot comply", ""}, actualRefusals, "refusal content must remain attached to its own msg_ item")
	require.Len(t, messages, 3)

	replayed := convertInputFromMessages(messages, llm.TransformOptions{ArrayInputs: lo.ToPtr(true)})
	var replayedMessageIDs []string
	for _, item := range replayed.Items {
		if item.Type == "message" {
			replayedMessageIDs = append(replayedMessageIDs, item.ID)
			require.True(t, isValidMessageItemID(item.ID))
		} else if item.Type == "reasoning" {
			require.False(t, isValidMessageItemID(item.ID), "reasoning item must not inherit a generated message id")
		}
	}
	require.Equal(t, generated, replayedMessageIDs)
}

func TestResponsesStreamRoundTrip_InterleavedReasoningTextAndRefusalDoNotCrossWireIDs(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	providerEvents := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_interleaved_ids","object":"response","created_at":1700000000,"model":"gpt-5.6-sol","status":"in_progress","output":[]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_first","type":"reasoning","summary":[]}}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_first","type":"reasoning","summary":[],"encrypted_content":"enc_first"}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_provider_text","type":"message","status":"in_progress","role":"assistant","content":[]}}`)},
		{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","item_id":"msg_provider_text","output_index":1,"content_index":0,"delta":"hello"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_provider_text","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":2,"item":{"id":"rs_second","type":"reasoning","summary":[]}}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":2,"item":{"id":"rs_second","type":"reasoning","summary":[],"encrypted_content":"enc_second"}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":3,"item":{"id":"msg_provider_refusal","type":"message","status":"in_progress","role":"assistant","content":[]}}`)},
		{Type: "response.refusal.delta", Data: []byte(`{"type":"response.refusal.delta","item_id":"msg_provider_refusal","output_index":3,"content_index":0,"delta":"cannot comply"}`)},
		{Type: "response.refusal.done", Data: []byte(`{"type":"response.refusal.done","item_id":"msg_provider_refusal","output_index":3,"content_index":0,"refusal":"cannot comply"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":3,"item":{"id":"msg_provider_refusal","type":"message","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"cannot comply"}]}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_interleaved_ids","object":"response","created_at":1700000000,"model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)},
	}

	llmStream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(providerEvents))
	require.NoError(t, err)
	llmEvents, err := streams.All(llmStream)
	require.NoError(t, err)

	inbound := NewInboundTransformer()
	replayedStream, err := inbound.TransformStream(t.Context(), streams.SliceStream(llmEvents))
	require.NoError(t, err)

	var (
		doneMessageIDs   []string
		doneReasoningIDs []string
		replayedItems    []Item
	)
	for replayedStream.Next() {
		var event StreamEvent
		require.NoError(t, json.Unmarshal(replayedStream.Current().Data, &event))

		if event.ItemID != nil {
			switch event.Type {
			case StreamEventTypeOutputTextDelta:
				require.Equal(t, "msg_provider_text", *event.ItemID)
			case StreamEventTypeRefusalDelta:
				require.Equal(t, "msg_provider_refusal", *event.ItemID)
			}
		}
		if event.Type != StreamEventTypeOutputItemDone || event.Item == nil {
			continue
		}

		replayedItems = append(replayedItems, *event.Item)
		switch event.Item.Type {
		case "message":
			doneMessageIDs = append(doneMessageIDs, event.Item.ID)
			require.True(t, isValidMessageItemID(event.Item.ID))
		case "reasoning":
			doneReasoningIDs = append(doneReasoningIDs, event.Item.ID)
			require.False(t, isValidMessageItemID(event.Item.ID), "reasoning item id was cross-wired as a message id")
		}
	}
	require.NoError(t, replayedStream.Err())
	require.Equal(t, []string{"msg_provider_text", "msg_provider_refusal"}, doneMessageIDs)
	require.Equal(t, []string{"rs_first", "rs_second"}, doneReasoningIDs)

	nextTurnMessages, err := convertInputToMessages(&Input{Items: replayedItems})
	require.NoError(t, err)
	nextTurn := convertInputFromMessages(nextTurnMessages, llm.TransformOptions{ArrayInputs: lo.ToPtr(true)})

	var nextTurnMessageIDs []string
	for _, item := range nextTurn.Items {
		if item.Type != "message" {
			continue
		}
		nextTurnMessageIDs = append(nextTurnMessageIDs, item.ID)
		require.True(t, isValidMessageItemID(item.ID), "next-turn outbound message reference is invalid: %q", item.ID)
	}
	require.Equal(t, doneMessageIDs, nextTurnMessageIDs)
}

func TestResponsesStreamRoundTrip_PreservesReasoningIDWhenSummaryPrecedesEncryptedContent(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	const reasoningID = "rs_provider_summary_first"
	providerEvents := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_reasoning_summary_first","object":"response","created_at":1700000000,"model":"gpt-5.6-sol","status":"in_progress","output":[]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_provider_summary_first","type":"reasoning","status":"in_progress","summary":[]}}`)},
		{Type: "response.reasoning_summary_part.added", Data: []byte(`{"type":"response.reasoning_summary_part.added","item_id":"rs_provider_summary_first","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)},
		{Type: "response.reasoning_summary_text.delta", Data: []byte(`{"type":"response.reasoning_summary_text.delta","item_id":"rs_provider_summary_first","output_index":0,"summary_index":0,"delta":"I need to inspect the repository."}`)},
		{Type: "response.reasoning_summary_text.done", Data: []byte(`{"type":"response.reasoning_summary_text.done","item_id":"rs_provider_summary_first","output_index":0,"summary_index":0,"text":"I need to inspect the repository."}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_provider_summary_first","type":"reasoning","status":"completed","summary":[{"type":"summary_text","text":"I need to inspect the repository."}],"encrypted_content":"enc_reasoning_summary_first"}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_reasoning_summary_first","object":"response","created_at":1700000000,"model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)},
	}

	llmStream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(providerEvents))
	require.NoError(t, err)
	llmEvents, err := streams.All(llmStream)
	require.NoError(t, err)

	var summaryDeltaSeen bool
	for _, event := range llmEvents {
		if event == llm.DoneResponse || len(event.Choices) == 0 || event.Choices[0].Delta == nil ||
			event.Choices[0].Delta.ReasoningContent == nil {
			continue
		}

		summaryDeltaSeen = true
		metadata, ok := getResponsesReasoningItemMetadata(event.TransformerMetadata)
		require.True(t, ok, "reasoning summary delta must carry its Responses item identity")
		require.Equal(t, reasoningID, metadata.ID)
		require.False(t, metadata.Done)
	}
	require.True(t, summaryDeltaSeen)

	inbound := NewInboundTransformer()
	replayedStream, err := inbound.TransformStream(t.Context(), streams.SliceStream(llmEvents))
	require.NoError(t, err)

	var (
		addedID string
		deltaID string
		doneID  string
	)
	for replayedStream.Next() {
		var event StreamEvent
		require.NoError(t, json.Unmarshal(replayedStream.Current().Data, &event))

		switch event.Type {
		case StreamEventTypeOutputItemAdded:
			if event.Item != nil && event.Item.Type == "reasoning" {
				addedID = event.Item.ID
			}
		case StreamEventTypeReasoningSummaryTextDelta:
			deltaID = lo.FromPtr(event.ItemID)
		case StreamEventTypeOutputItemDone:
			if event.Item != nil && event.Item.Type == "reasoning" {
				doneID = event.Item.ID
			}
		}
	}
	require.NoError(t, replayedStream.Err())
	require.Equal(t, reasoningID, addedID)
	require.Equal(t, reasoningID, deltaID)
	require.Equal(t, reasoningID, doneID)
}

func TestResponsesResponseRoundTrip_PreservesReasoningItemID(t *testing.T) {
	const reasoningID = "rs_provider_non_stream"
	metadata := map[string]any{}
	message := convertOutputToMessage([]Item{{
		ID:               reasoningID,
		Type:             "reasoning",
		Status:           lo.ToPtr("completed"),
		Summary:          []ReasoningSummary{{Type: "summary_text", Text: "Inspect the repository."}},
		EncryptedContent: lo.ToPtr("enc_provider_non_stream"),
	}}, metadata)

	preserved, ok := getResponsesReasoningItemMetadata(metadata)
	require.True(t, ok, "non-streaming Responses output must retain its reasoning item identity")
	require.Equal(t, reasoningID, preserved.ID)

	response := convertToResponsesAPIResponse(&llm.Response{
		ID:                  "resp_reasoning_non_stream",
		Object:              "chat.completion",
		Model:               "gpt-5.6-sol",
		Created:             1700000000,
		TransformerMetadata: metadata,
		Choices: []llm.Choice{{
			Index:        0,
			Message:      &message,
			FinishReason: lo.ToPtr("stop"),
		}},
	})

	require.NotEmpty(t, response.Output)
	require.Equal(t, "reasoning", response.Output[0].Type)
	require.Equal(t, reasoningID, response.Output[0].ID)
	require.NotContains(t, response.Output[0].ID, "item_")
}

func TestBuildReasoningItem_UsesReasoningNamespaceWithoutProviderIdentity(t *testing.T) {
	item, ok := buildReasoningItem(llm.Message{
		ReasoningContent: lo.ToPtr("Cross-protocol reasoning."),
	}, nil)
	require.True(t, ok)
	require.True(t, strings.HasPrefix(item.ID, "rs_"), item.ID)
	require.NotContains(t, item.ID, "item_")
}

func TestResponsesNonStreamRoundTrip_PreservesToolItemIDsSeparatelyFromCallIDs(t *testing.T) {
	providerItems := []Item{
		{
			ID:        "fc_provider_weather",
			Type:      "function_call",
			CallID:    "call_provider_weather",
			Name:      "get_weather",
			Arguments: `{"city":"Beijing"}`,
		},
		{
			ID:     "ctc_provider_patch",
			Type:   "custom_tool_call",
			CallID: "call_provider_patch",
			Name:   "apply_patch",
			Input:  lo.ToPtr("*** Begin Patch"),
		},
	}

	message := convertOutputToMessage(providerItems, nil)
	require.Len(t, message.ToolCalls, 2)
	require.Equal(t, "call_provider_weather", message.ToolCalls[0].ID)
	require.Equal(t, "fc_provider_weather", getResponsesToolCallItemID(message.ToolCalls[0]))
	require.Equal(t, "call_provider_patch", message.ToolCalls[1].ResponseCustomToolCall.CallID)
	require.Equal(t, "ctc_provider_patch", getResponsesToolCallItemID(message.ToolCalls[1]))

	response := convertToResponsesAPIResponse(&llm.Response{
		ID:      "resp_tool_identity",
		Object:  "chat.completion",
		Model:   "gpt-5.6-sol",
		Created: 1700000000,
		Choices: []llm.Choice{{Index: 0, Message: &message}},
	})
	require.Len(t, response.Output, 2)
	require.Equal(t, "fc_provider_weather", response.Output[0].ID)
	require.Equal(t, "call_provider_weather", response.Output[0].CallID)
	require.Equal(t, "ctc_provider_patch", response.Output[1].ID)
	require.Equal(t, "call_provider_patch", response.Output[1].CallID)

	messages, err := convertInputToMessages(&Input{Items: providerItems})
	require.NoError(t, err)
	replayed := convertInputFromMessages(messages, llm.TransformOptions{ArrayInputs: lo.ToPtr(true)})
	require.Len(t, replayed.Items, 2)
	require.Equal(t, "fc_provider_weather", replayed.Items[0].ID)
	require.Equal(t, "call_provider_weather", replayed.Items[0].CallID)
	require.Equal(t, "ctc_provider_patch", replayed.Items[1].ID)
	require.Equal(t, "call_provider_patch", replayed.Items[1].CallID)
}

func TestResponsesStreamRoundTrip_PreservesToolItemIDsSeparatelyFromCallIDs(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	providerEvents := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_tool_ids","object":"response","created_at":1700000000,"model":"gpt-5.6-sol","status":"in_progress","output":[]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_provider_weather","type":"function_call","status":"in_progress","call_id":"call_provider_weather","name":"get_weather","arguments":""}}`)},
		{Type: "response.function_call_arguments.delta", Data: []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_provider_weather","output_index":0,"delta":"{\"city\":\"Beijing\"}"}`)},
		{Type: "response.function_call_arguments.done", Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_provider_weather","output_index":0,"call_id":"call_provider_weather","name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_provider_weather","type":"function_call","status":"completed","call_id":"call_provider_weather","name":"get_weather","arguments":"{\"city\":\"Beijing\"}"}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"ctc_provider_patch","type":"custom_tool_call","status":"in_progress","call_id":"call_provider_patch","name":"apply_patch","input":""}}`)},
		{Type: "response.custom_tool_call_input.delta", Data: []byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ctc_provider_patch","output_index":1,"delta":"*** Begin Patch"}`)},
		{Type: "response.custom_tool_call_input.done", Data: []byte(`{"type":"response.custom_tool_call_input.done","item_id":"ctc_provider_patch","output_index":1,"call_id":"call_provider_patch","name":"apply_patch","input":"*** Begin Patch"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"ctc_provider_patch","type":"custom_tool_call","status":"completed","call_id":"call_provider_patch","name":"apply_patch","input":"*** Begin Patch"}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_tool_ids","object":"response","created_at":1700000000,"model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`)},
	}

	llmStream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(providerEvents))
	require.NoError(t, err)
	llmEvents, err := streams.All(llmStream)
	require.NoError(t, err)

	inbound := NewInboundTransformer()
	replayedStream, err := inbound.TransformStream(t.Context(), streams.SliceStream(llmEvents))
	require.NoError(t, err)

	added := make(map[string]string)
	done := make(map[string]string)
	for replayedStream.Next() {
		var event StreamEvent
		require.NoError(t, json.Unmarshal(replayedStream.Current().Data, &event))
		if event.Item == nil {
			continue
		}

		switch event.Type {
		case StreamEventTypeOutputItemAdded:
			added[event.Item.Type] = event.Item.ID + ":" + event.Item.CallID
		case StreamEventTypeOutputItemDone:
			done[event.Item.Type] = event.Item.ID + ":" + event.Item.CallID
		}
	}
	require.NoError(t, replayedStream.Err())
	require.Equal(t, "fc_provider_weather:call_provider_weather", added["function_call"])
	require.Equal(t, "ctc_provider_patch:call_provider_patch", added["custom_tool_call"])
	require.Equal(t, added, done)
}
