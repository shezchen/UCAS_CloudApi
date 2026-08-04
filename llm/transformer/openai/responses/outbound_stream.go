package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/internal/pkg/xurl"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

// ErrStreamIncomplete is returned when the stream ends without a terminal event.
// Failed and cancelled terminal events are surfaced as llm.ResponseError instead.
var ErrStreamIncomplete = llm.ErrStreamIncomplete

// TransformStream transforms OpenAI Responses API SSE events to unified llm.Response stream.
func (t *OutboundTransformer) TransformStream(
	ctx context.Context,
	req *httpclient.Request,
	stream streams.Stream[*httpclient.StreamEvent],
) (streams.Stream[*llm.Response], error) {
	// Append the DONE event to the stream
	doneEvent := lo.ToPtr(llm.DoneStreamEvent)
	streamWithDone := streams.AppendStream(stream, doneEvent)

	return streams.NoNil(newResponsesOutboundStream(streamWithDone)), nil
}

// responsesOutboundStream wraps a stream and maintains state during processing.
type responsesOutboundStream struct {
	stream streams.Stream[*httpclient.StreamEvent]
	state  *outboundStreamState

	// Event queue
	eventQueue []*llm.Response
	queueIndex int
	err        error

	// Track whether the response reached a non-error terminal event.
	responseTerminal bool
}

// outboundStreamState holds the state for a streaming session.
type outboundStreamState struct {
	responseID         string
	responseModel      string
	previousResponseID *string
	usage              *llm.Usage
	created            int64
	messageItemID      string

	// Content accumulation
	textContent      strings.Builder
	reasoningContent strings.Builder
	refusalContent   strings.Builder

	// Tool call tracking
	toolCalls     map[string]*llm.ToolCall // callID -> tool call
	itemToCallID  map[string]string        // item.id -> call_id mapping
	toolCallIndex map[string]int           // callID -> index in the output

	// Reasoning signature tracking
	pendingReasoningEncryptedContent map[string]*string

	// Transformer metadata tracking
	transformerMetadata        map[string]any
	transformerMetadataEmitted bool
}

func newResponsesOutboundStream(stream streams.Stream[*httpclient.StreamEvent]) *responsesOutboundStream {
	return &responsesOutboundStream{
		stream: stream,
		state: &outboundStreamState{
			toolCalls:                        make(map[string]*llm.ToolCall),
			itemToCallID:                     make(map[string]string),
			toolCallIndex:                    make(map[string]int),
			pendingReasoningEncryptedContent: make(map[string]*string),
			transformerMetadata:              make(map[string]any),
		},
	}
}

func (s *responsesOutboundStream) enqueue(resp *llm.Response) {
	s.eventQueue = append(s.eventQueue, resp)
}

func (s *responsesOutboundStream) Next() bool {
	// If we have events in the queue, return them first
	if s.queueIndex < len(s.eventQueue) {
		return true
	}

	// Clear the queue and reset index for new events
	s.eventQueue = nil
	s.queueIndex = 0

	// Try to get the next chunk from source
	if !s.stream.Next() {
		// Stream ended - check if we received a terminal event
		// If not, this is an incomplete stream (e.g., upstream EOF)
		if s.err == nil && !s.responseTerminal && s.stream.Err() == nil {
			// Only set this error if we had started receiving response data
			// This distinguishes between "no response" and "incomplete response"
			if s.state.responseID != "" {
				s.err = ErrStreamIncomplete
			}
		}
		return false
	}

	event := s.stream.Current()

	err := s.transformStreamChunk(event)
	if err != nil {
		s.err = err
		return false
	}

	// Continue to the next event if no events were enqueued
	return s.Next()
}

// transformStreamChunk transforms a single OpenAI Responses API streaming chunk to unified llm.Response.
// Events are enqueued via s.enqueue() instead of being returned.
//
//nolint:maintidx,gocognit // It is complex and hard to split.
func (s *responsesOutboundStream) transformStreamChunk(event *httpclient.StreamEvent) error {
	if event == nil || len(event.Data) == 0 {
		return nil
	}

	// Handle [DONE] marker. AppendStream always adds a synthetic [DONE] after the
	// upstream SSE ends. A real Responses terminal event must already have set
	// responseTerminal; otherwise this is an incomplete stream and must fail
	// before the pipeline commits output to the client.
	if string(event.Data) == "[DONE]" {
		if !s.responseTerminal && s.state.responseID != "" {
			return ErrStreamIncomplete
		}
		s.enqueue(llm.DoneResponse)
		return nil
	}

	// Parse the streaming event
	var streamEvent StreamEvent

	err := json.Unmarshal(event.Data, &streamEvent)
	if err != nil {
		return fmt.Errorf("failed to unmarshal responses api stream event: %w", err)
	}
	streamEvent.Type = normalizeResponseTerminalEventType(streamEvent.Type, streamEvent.Response)

	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		slog.DebugContext(context.Background(), "received response stream event", slog.Any("event", streamEvent))
	}

	// Build base response
	resp := &llm.Response{
		Object:             "chat.completion.chunk",
		ID:                 s.state.responseID,
		Model:              s.state.responseModel,
		Created:            s.state.created,
		PreviousResponseID: s.state.previousResponseID,
	}

	//nolint:exhaustive //Only process events we care about.
	switch streamEvent.Type {
	case StreamEventTypeResponseCreated:
		if streamEvent.Response != nil {
			s.state.responseID = streamEvent.Response.ID
			s.state.responseModel = streamEvent.Response.Model
			s.state.created = streamEvent.Response.CreatedAt
			s.state.previousResponseID = streamEvent.Response.PreviousResponseID

			resp.ID = s.state.responseID
			resp.Model = s.state.responseModel
			resp.Created = s.state.created
			resp.PreviousResponseID = s.state.previousResponseID

			if streamEvent.Response.Usage != nil {
				s.state.usage = streamEvent.Response.Usage.ToUsage()
				resp.Usage = s.state.usage
			}
		}

		resp.Choices = []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					Role: "assistant",
				},
			},
		}

	case StreamEventTypeResponseInProgress:
		// Update state but don't emit an event
		if streamEvent.Response != nil {
			s.state.responseID = streamEvent.Response.ID
			s.state.responseModel = streamEvent.Response.Model
			s.state.created = streamEvent.Response.CreatedAt
			s.state.previousResponseID = streamEvent.Response.PreviousResponseID

			if streamEvent.Response.Usage != nil {
				s.state.usage = streamEvent.Response.Usage.ToUsage()
			}
		}

		return nil // Intentionally skip this event
	case StreamEventTypeOutputItemAdded:
		// Output item added - check type to determine how to handle
		if streamEvent.Item == nil {
			// No item data, skip
			return nil // Intentionally skip this event
		}

		item := streamEvent.Item
		switch item.Type {
		case "message":
			// Keep the provider's message item identifier attached to the
			// unified stream. The Responses inbound transformer must return this
			// exact msg_ identifier to clients so a later turn can replay it.
			s.state.messageItemID = item.ID

			return nil

		case "reasoning":
			if item.ID == "" || item.EncryptedContent == nil || *item.EncryptedContent == "" {
				return nil // Intentionally skip this event
			}

			// Responses streams may send a provisional encrypted_content on item.added
			// and the final blob on item.done. Hold the value until item.done so the
			// final blob replaces the provisional one instead of being concatenated.
			s.state.pendingReasoningEncryptedContent[item.ID] = shared.EncodeOpenAIEncryptedContent(item.EncryptedContent)
			return nil

		case "function_call":
			// Initialize tool call tracking
			toolCallIdx := len(s.state.toolCalls)
			s.state.toolCalls[item.CallID] = &llm.ToolCall{
				ID:                  item.CallID,
				Type:                "function",
				TransformerMetadata: responsesToolCallItemMetadataForType("function_call", item.ID),
				Function: llm.FunctionCall{
					Name:      item.Name,
					Namespace: item.Namespace,
					Arguments: "",
				},
			}
			// Map item.id to call_id for later lookup
			s.state.itemToCallID[item.ID] = item.CallID
			s.state.toolCallIndex[item.CallID] = toolCallIdx

			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{
						ToolCalls: []llm.ToolCall{
							{
								ID:                  item.CallID,
								Type:                "function",
								Index:               toolCallIdx,
								TransformerMetadata: responsesToolCallItemMetadataForType("function_call", item.ID),
								Function: llm.FunctionCall{
									Name:      item.Name,
									Namespace: item.Namespace,
								},
							},
						},
					},
				},
			}

		case "custom_tool_call":
			// Custom tool call - initialize tracking, input will be streamed via delta events
			toolCallIdx := len(s.state.toolCalls)
			s.state.toolCalls[item.CallID] = &llm.ToolCall{
				ID:                  item.CallID,
				Type:                llm.ToolTypeResponsesCustomTool,
				TransformerMetadata: responsesToolCallItemMetadataForType("custom_tool_call", item.ID),
				ResponseCustomToolCall: &llm.ResponseCustomToolCall{
					CallID: item.CallID,
					Name:   item.Name,
					Input:  "",
				},
			}
			s.state.itemToCallID[item.ID] = item.CallID
			s.state.toolCallIndex[item.CallID] = toolCallIdx

			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{
						ToolCalls: []llm.ToolCall{
							{
								ID:                  item.CallID,
								Type:                llm.ToolTypeResponsesCustomTool,
								Index:               toolCallIdx,
								TransformerMetadata: responsesToolCallItemMetadataForType("custom_tool_call", item.ID),
								ResponseCustomToolCall: &llm.ResponseCustomToolCall{
									CallID: item.CallID,
									Name:   item.Name,
								},
							},
						},
					},
				},
			}

		default:
			// For other item types (e.g., message), skip - no meaningful content to emit
			return nil // Intentionally skip this event
		}

	case StreamEventTypeFunctionCallArgumentsDelta:
		// Function call arguments delta
		if streamEvent.ItemID != nil {
			// Look up call_id from item_id mapping
			callID, ok := s.state.itemToCallID[*streamEvent.ItemID]
			if !ok {
				// Fallback: item_id might be the call_id itself
				callID = *streamEvent.ItemID
			}

			if tc, ok := s.state.toolCalls[callID]; ok {
				tc.Function.Arguments += streamEvent.Delta
				toolCallIdx := s.state.toolCallIndex[callID]

				resp.Choices = []llm.Choice{
					{
						Index: 0,
						Delta: &llm.Message{
							ToolCalls: []llm.ToolCall{
								{
									Index: toolCallIdx,
									Function: llm.FunctionCall{
										Arguments: streamEvent.Delta,
									},
								},
							},
						},
					},
				}
			}
		}

	case StreamEventTypeFunctionCallArgumentsDone:
		// Function call completed - update state but don't emit an event
		if streamEvent.CallID != "" {
			if tc, ok := s.state.toolCalls[streamEvent.CallID]; ok {
				if streamEvent.Name != "" {
					tc.Function.Name = streamEvent.Name
				}
				if streamEvent.Namespace != "" {
					tc.Function.Namespace = streamEvent.Namespace
				}
				tc.Function.Arguments = streamEvent.Arguments
			}
		}

		return nil // Intentionally skip this event

	case StreamEventTypeCustomToolCallInputDelta:
		// Custom tool call input delta - accumulate and emit as tool call delta
		if streamEvent.ItemID != nil {
			callID, ok := s.state.itemToCallID[*streamEvent.ItemID]
			if !ok {
				callID = *streamEvent.ItemID
			}

			if tc, ok := s.state.toolCalls[callID]; ok {
				tc.ResponseCustomToolCall.Input += streamEvent.Delta
				toolCallIdx := s.state.toolCallIndex[callID]

				resp.Choices = []llm.Choice{
					{
						Index: 0,
						Delta: &llm.Message{
							ToolCalls: []llm.ToolCall{
								{
									Index: toolCallIdx,
									Type:  llm.ToolTypeResponsesCustomTool,
									ResponseCustomToolCall: &llm.ResponseCustomToolCall{
										CallID: callID,
										Name:   tc.ResponseCustomToolCall.Name,
										Input:  streamEvent.Delta,
									},
								},
							},
						},
					},
				}
			}
		}

	case StreamEventTypeCustomToolCallInputDone:
		// Custom tool call input completed - update state but don't emit an event
		if streamEvent.ItemID != nil {
			callID, ok := s.state.itemToCallID[*streamEvent.ItemID]
			if !ok {
				callID = *streamEvent.ItemID
			}

			if tc, ok := s.state.toolCalls[callID]; ok {
				tc.ResponseCustomToolCall.Input = streamEvent.Input
			}
		}

		return nil // Intentionally skip this event

	case StreamEventTypeContentPartAdded:
		// Content part added - skip, no meaningful content to emit
		return nil // Intentionally skip this event

	case StreamEventTypeOutputTextDelta:
		// Text content delta
		s.state.textContent.WriteString(streamEvent.Delta)
		if streamEvent.ItemID != nil && *streamEvent.ItemID != "" {
			s.state.messageItemID = *streamEvent.ItemID
		}

		resp.Choices = []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					ID: s.state.messageItemID,
					Content: llm.MessageContent{
						Content: &streamEvent.Delta,
					},
				},
			},
		}

	case StreamEventTypeReasoningSummaryTextDelta:
		// Reasoning content delta
		s.state.reasoningContent.WriteString(streamEvent.Delta)
		if streamEvent.ItemID != nil && *streamEvent.ItemID != "" {
			resp.TransformerMetadata = map[string]any{
				responsesReasoningItemTransformerMetadataKey: map[string]any{
					"id": *streamEvent.ItemID,
				},
			}
		}

		resp.Choices = []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					ReasoningContent: &streamEvent.Delta,
				},
			},
		}

	case StreamEventTypeOutputTextDone:
		// Text content completed - skip, content was already streamed via deltas
		return nil // Intentionally skip this event

	case StreamEventTypeRefusalDelta:
		s.state.refusalContent.WriteString(streamEvent.Delta)
		if streamEvent.ItemID != nil && *streamEvent.ItemID != "" {
			s.state.messageItemID = *streamEvent.ItemID
		}

		resp.Choices = []llm.Choice{{
			Index: 0,
			Delta: &llm.Message{
				ID:      s.state.messageItemID,
				Refusal: streamEvent.Delta,
			},
		}}

	case StreamEventTypeRefusalDone:
		return nil // The refusal text was already emitted by delta events.

	case StreamEventTypeReasoningSummaryTextDone:
		// Reasoning content completed - skip, content was already streamed via deltas
		return nil // Intentionally skip this event

	case StreamEventTypeOutputItemDone:
		if streamEvent.Item == nil {
			return nil // Intentionally skip this event
		}
		if streamEvent.Item.Type == "web_search_call" {
			appendResponseWebSearchCallMetadata(s.state.transformerMetadata, *streamEvent.Item)
			return nil // Intentionally skip this event
		}
		if streamEvent.Item.Type == "reasoning" {
			if streamEvent.Item.ID == "" {
				return nil // Intentionally skip this event
			}

			encryptedContent := shared.EncodeOpenAIEncryptedContent(streamEvent.Item.EncryptedContent)
			if encryptedContent == nil || *encryptedContent == "" {
				encryptedContent = s.state.pendingReasoningEncryptedContent[streamEvent.Item.ID]
			}
			delete(s.state.pendingReasoningEncryptedContent, streamEvent.Item.ID)
			if encryptedContent == nil || *encryptedContent == "" {
				return nil // Intentionally skip this event
			}

			resp.TransformerMetadata = map[string]any{
				responsesReasoningItemTransformerMetadataKey: map[string]any{
					"id":   streamEvent.Item.ID,
					"done": true,
				},
			}
			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{
						ReasoningSignature: encryptedContent,
					},
				},
			}
			break
		}
		if streamEvent.Item.Type != "message" {
			return nil // Intentionally skip this event
		}

		msg := convertOutputToMessage([]Item{*streamEvent.Item}, s.state.transformerMetadata)
		if msg.ID != "" {
			s.state.messageItemID = msg.ID
		}
		// Some compatible providers omit response.refusal.delta and only include
		// the completed refusal in output_item.done. Emit it once in that case.
		if msg.Refusal != "" && s.state.refusalContent.Len() == 0 {
			s.state.refusalContent.WriteString(msg.Refusal)
			resp.Choices = []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ID: msg.ID, Refusal: msg.Refusal},
			}}

			break
		}
		if len(msg.Annotations) == 0 {
			return nil // Intentionally skip this event
		}
		if len(s.state.transformerMetadata) > 0 {
			resp.TransformerMetadata = s.state.transformerMetadata
			s.state.transformerMetadataEmitted = true
		}

		resp.Choices = []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					ID:          msg.ID,
					Annotations: msg.Annotations,
				},
			},
		}

	case StreamEventTypeContentPartDone,
		StreamEventTypeReasoningSummaryPartAdded, StreamEventTypeReasoningSummaryPartDone:
		// These events don't need special handling - skip
		return nil // Intentionally skip this event

	case StreamEventTypeResponseCompleted:
		// Response completed - emit two events: one with finish_reason, one with usage
		if responseErr := responseTerminalError(streamEvent.Response); responseErr != nil {
			return responseErr
		}
		s.responseTerminal = true
		if streamEvent.Response != nil {
			s.state.previousResponseID = streamEvent.Response.PreviousResponseID
			resp.PreviousResponseID = s.state.previousResponseID
		}
		if len(s.state.transformerMetadata) > 0 && !s.state.transformerMetadataEmitted {
			resp.TransformerMetadata = s.state.transformerMetadata
			s.state.transformerMetadataEmitted = true
		}

		finishReason := "stop"
		if len(s.state.toolCalls) > 0 {
			finishReason = "tool_calls"
		}

		// First event: finish_reason with empty delta
		resp.Choices = []llm.Choice{
			{
				Index:        0,
				Delta:        &llm.Message{},
				FinishReason: &finishReason,
			},
		}

		// Second event: usage (if available)
		if streamEvent.Response != nil && streamEvent.Response.Usage != nil {
			s.state.usage = streamEvent.Response.Usage.ToUsage()
			usageResp := &llm.Response{
				Object:             "chat.completion.chunk",
				ID:                 s.state.responseID,
				Model:              s.state.responseModel,
				Created:            s.state.created,
				PreviousResponseID: s.state.previousResponseID,
				Choices:            []llm.Choice{},
				Usage:              s.state.usage,
			}

			s.enqueue(resp)
			s.enqueue(usageResp)

			return nil
		}

	case StreamEventTypeResponseFailed:
		return newUpstreamResponseError("failed", responseErrorFromSnapshot(streamEvent.Response))

	case StreamEventTypeResponseIncomplete:
		// Response incomplete (e.g., max tokens)
		if responseErr := responseTerminalError(streamEvent.Response); responseErr != nil {
			return responseErr
		}
		s.responseTerminal = true
		resp.ProtocolStatus = "incomplete"
		if streamEvent.Response != nil {
			s.state.previousResponseID = streamEvent.Response.PreviousResponseID
			resp.PreviousResponseID = s.state.previousResponseID
			if streamEvent.Response.IncompleteDetails != nil {
				resp.IncompleteReason = strings.TrimSpace(streamEvent.Response.IncompleteDetails.Reason)
			}
		}
		if len(s.state.transformerMetadata) > 0 && !s.state.transformerMetadataEmitted {
			resp.TransformerMetadata = s.state.transformerMetadata
			s.state.transformerMetadataEmitted = true
		}
		finishReason := "length"
		resp.Choices = []llm.Choice{
			{
				Index:        0,
				Delta:        &llm.Message{},
				FinishReason: &finishReason,
			},
		}
		if streamEvent.Response != nil && streamEvent.Response.Usage != nil {
			s.state.usage = streamEvent.Response.Usage.ToUsage()
			s.enqueue(resp)
			s.enqueue(&llm.Response{
				Object:             "chat.completion.chunk",
				ID:                 s.state.responseID,
				Model:              s.state.responseModel,
				Created:            s.state.created,
				PreviousResponseID: s.state.previousResponseID,
				Choices:            []llm.Choice{},
				Usage:              s.state.usage,
			})

			return nil
		}

	case StreamEventTypeResponseCancelled:
		return newUpstreamResponseError("cancelled", responseErrorFromSnapshot(streamEvent.Response))

	case StreamEventTypeError:
		return newUpstreamStreamEventError(&streamEvent)

	case StreamEventTypeImageGenerationPartialImage,
		StreamEventTypeImageGenerationGenerating,
		StreamEventTypeImageGenerationInProgress,
		StreamEventTypeImageGenerationCompleted:
		// Handle image generation events
		if streamEvent.PartialImageB64 != "" {
			imageURL := xurl.BuildDataURL("image/png", streamEvent.PartialImageB64, true)
			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{
						Content: llm.MessageContent{
							MultipleContent: []llm.MessageContentPart{
								{
									Type: "image_url",
									ImageURL: &llm.ImageURL{
										URL: imageURL,
									},
								},
							},
						},
					},
				},
			}
		} else {
			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{},
				},
			}
		}

	default:
		// Unknown event type - skip
		return nil // Intentionally skip this event
	}

	s.enqueue(resp)

	return nil
}

func (s *responsesOutboundStream) Current() *llm.Response {
	if s.queueIndex < len(s.eventQueue) {
		event := s.eventQueue[s.queueIndex]
		s.queueIndex++

		return event
	}

	return nil
}

func (s *responsesOutboundStream) Err() error {
	if s.err != nil {
		return s.err
	}

	return s.stream.Err()
}

func (s *responsesOutboundStream) Close() error {
	return s.stream.Close()
}

// AggregateStreamChunks aggregates OpenAI Responses API streaming chunks into a complete response.
func (t *OutboundTransformer) AggregateStreamChunks(
	ctx context.Context, _ *httpclient.Request,
	chunks []*httpclient.StreamEvent,
) ([]byte, llm.ResponseMeta, error) {
	return AggregateStreamChunks(ctx, chunks)
}
