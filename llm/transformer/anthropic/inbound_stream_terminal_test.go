package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func collectInboundStreamRawEvents(t *testing.T, transformer *InboundTransformer, input []*llm.Response) []*httpclient.StreamEvent {
	t.Helper()

	stream, err := transformer.TransformStream(t.Context(), streams.SliceStream(input))
	require.NoError(t, err)

	var events []*httpclient.StreamEvent

	for stream.Next() {
		events = append(events, stream.Current())
	}

	require.NoError(t, stream.Err())

	return events
}

func findRawEvent(t *testing.T, events []*httpclient.StreamEvent, eventType string) map[string]any {
	t.Helper()

	for _, event := range events {
		if event.Type != eventType {
			continue
		}

		var decoded map[string]any

		require.NoError(t, json.Unmarshal(event.Data, &decoded))

		return decoded
	}

	t.Fatalf("no %s event found in %d events", eventType, len(events))

	return nil
}

func TestInboundStream_TruncatedSourceEmitsSpecShapedMessageDelta(t *testing.T) {
	transformer := NewInboundTransformer()

	text := "partial answer"

	// The upstream dies after one content chunk: no finish_reason, no usage.
	input := []*llm.Response{
		{
			ID:     "msg_truncated_stream",
			Object: "chat.completion.chunk",
			Model:  "claude-sonnet-4-6",
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{
					Role: "assistant",
					Content: llm.MessageContent{
						Content: &text,
					},
				},
			}},
		},
	}

	events := collectInboundStreamRawEvents(t, transformer, input)

	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}

	require.Equal(t, []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}, types)

	messageDelta := findRawEvent(t, events, "message_delta")

	delta, ok := messageDelta["delta"]
	require.True(t, ok, "message_delta must carry a delta object, got %s", string(events[len(events)-2].Data))

	deltaObject, ok := delta.(map[string]any)
	require.True(t, ok, "delta must be an object, got %T", delta)

	stopReason, ok := deltaObject["stop_reason"]
	require.True(t, ok, "delta must carry an explicit stop_reason key")
	require.Nil(t, stopReason, "stop_reason must be null when the upstream never reported one")

	stopSequence, ok := deltaObject["stop_sequence"]
	require.True(t, ok, "delta must carry an explicit stop_sequence key")
	require.Nil(t, stopSequence)

	usage, ok := messageDelta["usage"].(map[string]any)
	require.True(t, ok, "message_delta must carry usage")
	require.InDelta(t, float64(0), usage["input_tokens"], 0, "usage must not be fabricated when the upstream sent none")
	require.InDelta(t, float64(0), usage["output_tokens"], 0)
}

func TestInboundStream_TruncatedSourceKeepsReportedUsage(t *testing.T) {
	transformer := NewInboundTransformer()

	text := "partial answer"

	input := []*llm.Response{
		{
			ID:     "msg_truncated_usage_stream",
			Object: "chat.completion.chunk",
			Model:  "claude-sonnet-4-6",
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{
					Role: "assistant",
					Content: llm.MessageContent{
						Content: &text,
					},
				},
			}},
			Usage: &llm.Usage{PromptTokens: 12, CompletionTokens: 7},
		},
	}

	events := collectInboundStreamRawEvents(t, transformer, input)
	messageDelta := findRawEvent(t, events, "message_delta")

	usage, ok := messageDelta["usage"].(map[string]any)
	require.True(t, ok, "message_delta must carry usage")
	require.InDelta(t, float64(12), usage["input_tokens"], 0)
	require.InDelta(t, float64(7), usage["output_tokens"], 0)
}

func TestInboundStream_MessageDeltaKeepsReportedStopReason(t *testing.T) {
	transformer := NewInboundTransformer()

	text := "complete answer"
	finishReason := "stop"

	// finish_reason arrives but the upstream never sends the trailing usage
	// chunk, so the terminal events are synthesized on source exhaustion.
	input := []*llm.Response{
		{
			ID:     "msg_stop_no_usage_stream",
			Object: "chat.completion.chunk",
			Model:  "claude-sonnet-4-6",
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{
					Role: "assistant",
					Content: llm.MessageContent{
						Content: &text,
					},
				},
			}},
		},
		{
			ID:     "msg_stop_no_usage_stream",
			Object: "chat.completion.chunk",
			Model:  "claude-sonnet-4-6",
			Choices: []llm.Choice{{
				Index:        0,
				FinishReason: &finishReason,
			}},
		},
	}

	events := collectInboundStreamRawEvents(t, transformer, input)
	messageDelta := findRawEvent(t, events, "message_delta")

	deltaObject, ok := messageDelta["delta"].(map[string]any)
	require.True(t, ok, "delta must be an object")
	require.Equal(t, "end_turn", deltaObject["stop_reason"])
	require.Contains(t, deltaObject, "stop_sequence")
	require.Nil(t, deltaObject["stop_sequence"])
}

func TestStreamEvent_MarshalJSON_OnlyMessageDeltaForcesStopFields(t *testing.T) {
	textType := "text_delta"
	text := "hi"
	index := int64(0)

	contentBlockDelta, err := json.Marshal(&StreamEvent{
		Type:  "content_block_delta",
		Index: &index,
		Delta: &StreamDelta{Type: &textType, Text: &text},
	})
	require.NoError(t, err)
	require.JSONEq(
		t,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		string(contentBlockDelta),
	)

	messageDelta, err := json.Marshal(&StreamEvent{Type: "message_delta"})
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null}}`, string(messageDelta))
}
