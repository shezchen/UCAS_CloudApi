package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	responsestransformer "github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func responsesEvent(t *testing.T, eventType string, fields map[string]any) *httpclient.StreamEvent {
	t.Helper()
	fields["type"] = eventType
	data, err := json.Marshal(fields)
	require.NoError(t, err)
	return &httpclient.StreamEvent{Type: eventType, Data: data}
}

func responsesToolCallStream(t *testing.T, responseID, reasoningID, functionID, callID, toolName string) []*httpclient.StreamEvent {
	t.Helper()
	reasoningItem := map[string]any{
		"id": reasoningID, "type": "reasoning", "status": "completed",
		"summary":           []any{map[string]any{"type": "summary_text", "text": "I need the next tool."}},
		"encrypted_content": "enc_" + reasoningID,
	}
	functionItem := map[string]any{
		"id": functionID, "type": "function_call", "status": "completed",
		"call_id": callID, "name": toolName, "arguments": `{"step":1}`,
	}

	return []*httpclient.StreamEvent{
		responsesEvent(t, "response.created", map[string]any{"response": map[string]any{
			"id": responseID, "object": "response", "created_at": 1700000000,
			"model": "gpt-5.6-sol", "status": "in_progress", "output": []any{},
		}}),
		responsesEvent(t, "response.output_item.added", map[string]any{
			"output_index": 0,
			"item":         map[string]any{"id": reasoningID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
		}),
		responsesEvent(t, "response.reasoning_summary_part.added", map[string]any{
			"item_id": reasoningID, "output_index": 0, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		}),
		responsesEvent(t, "response.reasoning_summary_text.delta", map[string]any{
			"item_id": reasoningID, "output_index": 0, "summary_index": 0, "delta": "I need the next tool.",
		}),
		responsesEvent(t, "response.reasoning_summary_text.done", map[string]any{
			"item_id": reasoningID, "output_index": 0, "summary_index": 0, "text": "I need the next tool.",
		}),
		responsesEvent(t, "response.output_item.done", map[string]any{"output_index": 0, "item": reasoningItem}),
		responsesEvent(t, "response.output_item.added", map[string]any{
			"output_index": 1,
			"item": map[string]any{
				"id": functionID, "type": "function_call", "status": "in_progress",
				"call_id": callID, "name": toolName, "arguments": "",
			},
		}),
		responsesEvent(t, "response.function_call_arguments.delta", map[string]any{
			"item_id": functionID, "output_index": 1, "delta": `{"step":1}`,
		}),
		responsesEvent(t, "response.function_call_arguments.done", map[string]any{
			"item_id": functionID, "output_index": 1, "arguments": `{"step":1}`,
		}),
		responsesEvent(t, "response.output_item.done", map[string]any{"output_index": 1, "item": functionItem}),
		responsesEvent(t, "response.completed", map[string]any{"response": map[string]any{
			"id": responseID, "object": "response", "created_at": 1700000000,
			"model": "gpt-5.6-sol", "status": "completed",
			"output": []any{reasoningItem, functionItem},
			"usage":  map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15},
		}}),
	}
}

func responsesFinalStream(t *testing.T, responseID, reasoningID, messageID string) []*httpclient.StreamEvent {
	t.Helper()
	reasoningItem := map[string]any{
		"id": reasoningID, "type": "reasoning", "status": "completed", "summary": []any{},
		"encrypted_content": "enc_" + reasoningID,
	}
	messageItem := map[string]any{
		"id": messageID, "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": "all tools completed", "annotations": []any{}}},
	}

	return []*httpclient.StreamEvent{
		responsesEvent(t, "response.created", map[string]any{"response": map[string]any{
			"id": responseID, "object": "response", "created_at": 1700000000,
			"model": "gpt-5.6-sol", "status": "in_progress", "output": []any{},
		}}),
		responsesEvent(t, "response.output_item.added", map[string]any{
			"output_index": 0,
			"item":         map[string]any{"id": reasoningID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
		}),
		responsesEvent(t, "response.output_item.done", map[string]any{"output_index": 0, "item": reasoningItem}),
		responsesEvent(t, "response.output_item.added", map[string]any{
			"output_index": 1,
			"item":         map[string]any{"id": messageID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		}),
		responsesEvent(t, "response.content_part.added", map[string]any{
			"item_id": messageID, "output_index": 1, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}),
		responsesEvent(t, "response.output_text.delta", map[string]any{
			"item_id": messageID, "output_index": 1, "content_index": 0, "delta": "all tools completed",
		}),
		responsesEvent(t, "response.output_text.done", map[string]any{
			"item_id": messageID, "output_index": 1, "content_index": 0, "text": "all tools completed",
		}),
		responsesEvent(t, "response.content_part.done", map[string]any{
			"item_id": messageID, "output_index": 1, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "all tools completed", "annotations": []any{}},
		}),
		responsesEvent(t, "response.output_item.done", map[string]any{"output_index": 1, "item": messageItem}),
		responsesEvent(t, "response.completed", map[string]any{"response": map[string]any{
			"id": responseID, "object": "response", "created_at": 1700000000,
			"model": "gpt-5.6-sol", "status": "completed", "output": []any{reasoningItem, messageItem},
			"usage": map[string]any{"input_tokens": 20, "output_tokens": 4, "total_tokens": 24},
		}}),
	}
}

func collectResponsesTurn(t *testing.T, result *pipeline.Result) []map[string]any {
	t.Helper()
	require.NotNil(t, result)
	require.True(t, result.Stream)

	var doneItems []map[string]any
	var completed bool
	for result.EventStream.Next() {
		var event map[string]any
		require.NoError(t, json.Unmarshal(result.EventStream.Current().Data, &event))
		switch event["type"] {
		case "response.output_item.done":
			item, ok := event["item"].(map[string]any)
			require.True(t, ok)
			doneItems = append(doneItems, item)
		case "response.completed":
			completed = true
		}
	}
	require.NoError(t, result.EventStream.Err())
	require.NoError(t, result.EventStream.Close())
	require.True(t, completed)
	return doneItems
}

func requireResponsesItem(t *testing.T, items []map[string]any, itemType, id, callID string) map[string]any {
	t.Helper()
	for _, item := range items {
		if item["type"] != itemType {
			continue
		}
		require.Equal(t, id, item["id"])
		if callID != "" {
			require.Equal(t, callID, item["call_id"])
		}
		return item
	}
	require.FailNow(t, "Responses output item missing", "type=%s id=%s", itemType, id)
	return nil
}

func TestPipeline_ResponsesStoreFalseTwoToolTurnsPreserveExactItemIdentity(t *testing.T) {
	providerTurns := [][]*httpclient.StreamEvent{
		responsesToolCallStream(t, "resp_turn_1", "rs_turn_1", "fc_turn_1", "call_turn_1", "first_tool"),
		responsesToolCallStream(t, "resp_turn_2", "rs_turn_2", "fc_turn_2", "call_turn_2", "second_tool"),
		responsesFinalStream(t, "resp_turn_3", "rs_turn_3", "msg_final"),
	}
	var upstreamRequests []map[string]any
	executor := &mockExecutor{doStreamFunc: func(_ context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
		var payload map[string]any
		require.NoError(t, json.Unmarshal(request.Body, &payload))
		upstreamRequests = append(upstreamRequests, payload)
		turn := len(upstreamRequests) - 1
		require.Less(t, turn, len(providerTurns))
		return streams.SliceStream(providerTurns[turn]), nil
	}}
	inbound := responsestransformer.NewInboundTransformer()
	outbound, err := responsestransformer.NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)
	pipe := pipeline.NewFactory(executor).Pipeline(inbound, outbound)

	userItem := map[string]any{"type": "message", "role": "user", "content": "run both tools in order"}
	history := []any{userItem}
	runTurn := func(input []any) []map[string]any {
		body, marshalErr := json.Marshal(map[string]any{
			"model": "gpt-5.6-sol", "stream": true, "store": false, "input": input,
		})
		require.NoError(t, marshalErr)
		result, processErr := pipe.Process(t.Context(), &httpclient.Request{
			Method: http.MethodPost, URL: "/v1/responses", ContentType: "application/json",
			Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body,
		})
		require.NoError(t, processErr)
		return collectResponsesTurn(t, result)
	}

	turn1 := runTurn(history)
	reasoning1 := requireResponsesItem(t, turn1, "reasoning", "rs_turn_1", "")
	function1 := requireResponsesItem(t, turn1, "function_call", "fc_turn_1", "call_turn_1")
	history = append(history, reasoning1, function1, map[string]any{
		"type": "function_call_output", "call_id": "call_turn_1", "output": "first tool finished",
	})

	turn2 := runTurn(history)
	reasoning2 := requireResponsesItem(t, turn2, "reasoning", "rs_turn_2", "")
	function2 := requireResponsesItem(t, turn2, "function_call", "fc_turn_2", "call_turn_2")
	history = append(history, reasoning2, function2, map[string]any{
		"type": "function_call_output", "call_id": "call_turn_2", "output": "second tool finished",
	})

	turn3 := runTurn(history)
	requireResponsesItem(t, turn3, "reasoning", "rs_turn_3", "")
	requireResponsesItem(t, turn3, "message", "msg_final", "")
	require.Len(t, upstreamRequests, 3)

	for turn, expected := range []struct {
		reasoningIDs []string
		functionIDs  map[string]string
	}{
		{reasoningIDs: nil, functionIDs: nil},
		{reasoningIDs: []string{"rs_turn_1"}, functionIDs: map[string]string{"fc_turn_1": "call_turn_1"}},
		{reasoningIDs: []string{"rs_turn_1", "rs_turn_2"}, functionIDs: map[string]string{
			"fc_turn_1": "call_turn_1", "fc_turn_2": "call_turn_2",
		}},
	} {
		input, ok := upstreamRequests[turn]["input"].([]any)
		require.True(t, ok, "turn %d input must remain an item array", turn+1)
		seenReasoning := map[string]bool{}
		seenFunctions := map[string]string{}
		for _, rawItem := range input {
			item, itemOK := rawItem.(map[string]any)
			if !itemOK {
				continue
			}
			switch item["type"] {
			case "reasoning":
				seenReasoning[fmt.Sprint(item["id"])] = true
			case "function_call":
				seenFunctions[fmt.Sprint(item["id"])] = fmt.Sprint(item["call_id"])
			}
		}
		for _, id := range expected.reasoningIDs {
			require.True(t, seenReasoning[id], "turn %d must replay exact reasoning ID %s; upstream input=%#v", turn+1, id, input)
		}
		for id, callID := range expected.functionIDs {
			require.Equal(t, callID, seenFunctions[id], "turn %d must keep item ID separate from call_id", turn+1)
		}
	}
}

func TestPipeline_ResponsesPreviousResponseIDTwoToolTurnsRemainChained(t *testing.T) {
	providerTurns := [][]*httpclient.StreamEvent{
		responsesToolCallStream(t, "resp_stateful_1", "rs_stateful_1", "fc_stateful_1", "call_stateful_1", "first_tool"),
		responsesToolCallStream(t, "resp_stateful_2", "rs_stateful_2", "fc_stateful_2", "call_stateful_2", "second_tool"),
		responsesFinalStream(t, "resp_stateful_3", "rs_stateful_3", "msg_stateful_final"),
	}
	var upstreamRequests []map[string]any
	executor := &mockExecutor{doStreamFunc: func(_ context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
		var payload map[string]any
		require.NoError(t, json.Unmarshal(request.Body, &payload))
		upstreamRequests = append(upstreamRequests, payload)
		return streams.SliceStream(providerTurns[len(upstreamRequests)-1]), nil
	}}
	inbound := responsestransformer.NewInboundTransformer()
	outbound, err := responsestransformer.NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)
	pipe := pipeline.NewFactory(executor).Pipeline(inbound, outbound)

	runTurn := func(input any, previous string) []map[string]any {
		payload := map[string]any{"model": "gpt-5.6-sol", "stream": true, "store": true, "input": input}
		if previous != "" {
			payload["previous_response_id"] = previous
		}
		body, marshalErr := json.Marshal(payload)
		require.NoError(t, marshalErr)
		result, processErr := pipe.Process(t.Context(), &httpclient.Request{
			Method: http.MethodPost, URL: "/v1/responses", ContentType: "application/json",
			Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: body,
		})
		require.NoError(t, processErr)
		return collectResponsesTurn(t, result)
	}

	turn1 := runTurn("run both tools in order", "")
	requireResponsesItem(t, turn1, "reasoning", "rs_stateful_1", "")
	requireResponsesItem(t, turn1, "function_call", "fc_stateful_1", "call_stateful_1")
	turn2 := runTurn([]any{map[string]any{
		"type": "function_call_output", "call_id": "call_stateful_1", "output": "first tool finished",
	}}, "resp_stateful_1")
	requireResponsesItem(t, turn2, "reasoning", "rs_stateful_2", "")
	requireResponsesItem(t, turn2, "function_call", "fc_stateful_2", "call_stateful_2")
	turn3 := runTurn([]any{map[string]any{
		"type": "function_call_output", "call_id": "call_stateful_2", "output": "second tool finished",
	}}, "resp_stateful_2")
	requireResponsesItem(t, turn3, "message", "msg_stateful_final", "")

	require.Len(t, upstreamRequests, 3)
	require.NotContains(t, upstreamRequests[0], "previous_response_id")
	require.Equal(t, "resp_stateful_1", upstreamRequests[1]["previous_response_id"])
	require.Equal(t, "resp_stateful_2", upstreamRequests[2]["previous_response_id"])
}
