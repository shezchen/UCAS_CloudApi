package responses

import (
	"encoding/json"
	"net/http"
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
	require.Len(t, reasoningIDs, 3)
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
