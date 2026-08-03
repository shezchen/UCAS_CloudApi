package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

const codexResponsesLiteHeader = "X-OpenAI-Internal-Codex-Responses-Lite"

// isPassThroughEnabled returns true when the configured pass-through flag for the
// current channel is enabled and both API formats and stream semantics align.
//
// The effective flag is the channel-level PassThroughBody when set, otherwise it falls back
// to the global system setting. systemService may be nil; in that case only the channel-level
// setting is consulted (used by tests that exercise per-channel behavior in isolation).
func (p *PersistentOutboundTransformer) isPassThroughEnabled(ctx context.Context, systemService *biz.SystemService) bool {
	channel := p.GetCurrentChannel()
	if channel == nil {
		return false
	}

	rawReq := p.state.RawProviderRequest
	if rawReq == nil || rawReq.APIFormat == "" {
		return false
	}

	llmReq := p.state.LlmRequest
	if llmReq == nil || string(llmReq.APIFormat) != rawReq.APIFormat {
		return false
	}

	if !passThroughStreamAligned(p.state.OriginalRequestStream, llmReq.Stream) {
		return false
	}

	var enabled bool

	switch {
	case channel.Settings != nil && channel.Settings.PassThroughBody != nil:
		enabled = *channel.Settings.PassThroughBody
	case systemService != nil:
		global, err := systemService.PassThrough(ctx)
		if err != nil {
			log.Warn(ctx, "failed to get global pass-through setting", log.Cause(err))

			return false
		}

		enabled = global
	}

	return enabled
}

// isRequestBodyPassThroughEnabled keeps automatic compatibility for the Codex
// Responses Lite request envelope only. Responses still use AxonHub's normal
// transformer so terminal events are validated and persistence has one owner.
// Gateway prompt mutations always take precedence over the original raw body.
func (p *PersistentOutboundTransformer) isRequestBodyPassThroughEnabled(ctx context.Context, systemService *biz.SystemService) bool {
	if p.state.PromptPayloadMutated {
		return false
	}

	if p.isPassThroughEnabled(ctx, systemService) {
		return true
	}

	return p.isCodexResponsesLiteRequest()
}

func (p *PersistentOutboundTransformer) isCodexResponsesLiteRequest() bool {
	currentChannel := p.GetCurrentChannel()
	if currentChannel == nil || currentChannel.Type != channel.TypeCodex {
		return false
	}

	llmReq := p.state.LlmRequest
	if llmReq == nil || llmReq.APIFormat != llm.APIFormatOpenAIResponse || llmReq.RawRequest == nil {
		return false
	}

	return strings.EqualFold(
		strings.TrimSpace(llmReq.RawRequest.Headers.Get(codexResponsesLiteHeader)),
		"true",
	)
}

func passThroughStreamAligned(originalStream, effectiveStream *bool) bool {
	originalEnabled := originalStream != nil && *originalStream
	effectiveEnabled := effectiveStream != nil && *effectiveStream

	return originalEnabled == effectiveEnabled
}

// applyPassThroughRequestBody creates a middleware that reuses the original inbound request body when
// the channel enables pass-through and the inbound and outbound API formats are identical.
// For formats that encode the selected model in the request body, the mapped llmReq.Model is
// written back into the copied raw payload so pass-through does not bypass model mapping.
// Save the actual outbound provider request so pass-through checks use the emitted API format.
func applyPassThroughRequestBody(outbound *PersistentOutboundTransformer, systemService *biz.SystemService) pipeline.Middleware {
	return pipeline.OnRawRequest("pass-through-request-body", func(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		outbound.state.RawProviderRequest = request

		if !outbound.isRequestBodyPassThroughEnabled(ctx, systemService) {
			return request, nil
		}

		channel := outbound.GetCurrentChannel()
		llmReq := outbound.state.LlmRequest
		codexResponsesLite := outbound.isCodexResponsesLiteRequest()

		// Multipart bodies cannot be reused: the outbound transformer rebuilds the
		// multipart payload with a new boundary in Content-Type, so replaying the inbound
		// bytes would mismatch the header, and form fields cannot be patched via sjson.
		if !passThroughBodySupported(llmReq.APIFormat) {
			return request, nil
		}

		log.Debug(ctx, "applying pass-through body",
			log.String("channel", channel.Name),
			log.String("api_format", request.APIFormat),
		)

		body, err := mergePassThroughRequestBody(
			llmReq.RawRequest.Body,
			llmReq.APIFormat,
			llmReq.Model,
			llmReq.ReasoningEffort,
		)
		if err != nil {
			log.Warn(ctx, "failed to merge pass-through body, keeping outbound body",
				log.String("channel", channel.Name),
				log.Int("channel_id", channel.ID),
				log.Cause(err),
			)

			return request, nil
		}
		if codexResponsesLite {
			body, err = normalizeCodexResponsesLiteMessageIDs(body)
			if err != nil {
				return nil, fmt.Errorf("normalize Codex Responses Lite message ids: %w", err)
			}
		}

		request.Body = body
		outbound.state.PassThroughApplied = true

		return request, nil
	})
}

func mergePassThroughRequestBody(rawBody []byte, apiFormat llm.APIFormat, model, reasoningEffort string) ([]byte, error) {
	body := append([]byte(nil), rawBody...)

	if !passThroughBodyNeedsModelPatch(apiFormat) {
		return body, nil
	}

	if model != "" {
		nextBody, err := sjson.SetBytes(body, "model", model)
		if err != nil {
			return nil, fmt.Errorf("set model in pass-through body: %w", err)
		}

		body = nextBody
	}

	if reasoningEffort != "" && (apiFormat == llm.APIFormatOpenAIResponse || apiFormat == llm.APIFormatOpenAIResponseCompact) {
		nextBody, err := sjson.SetBytes(body, "reasoning.effort", reasoningEffort)
		if err != nil {
			return nil, fmt.Errorf("set reasoning effort in pass-through body: %w", err)
		}

		body = nextBody
	}

	return body, nil
}

// normalizeCodexResponsesLiteMessageIDs removes invalid IDs from message input
// items before a Lite envelope is replayed to the Codex upstream. Responses
// accepts message IDs with the msg_ prefix; an AxonHub-generated item_ ID from
// an older response must not be forwarded as though it were an upstream ID.
// Other item types keep their IDs because reasoning and tool items use their own
// provider-defined prefixes.
func normalizeCodexResponsesLiteMessageIDs(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	normalized := append([]byte(nil), body...)
	var normalizeErr error

	input.ForEach(func(index, item gjson.Result) bool {
		if normalizeErr != nil {
			return false
		}

		itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
		isMessage := itemType == "message" || (itemType == "" && item.Get("role").String() != "")
		if !isMessage {
			return true
		}

		id := strings.TrimSpace(item.Get("id").String())
		if id == "" || strings.HasPrefix(id, "msg_") {
			return true
		}

		path := fmt.Sprintf("input.%d.id", index.Int())
		normalized, normalizeErr = sjson.DeleteBytes(normalized, path)
		if normalizeErr != nil {
			normalizeErr = fmt.Errorf("remove invalid Codex Responses message id at %s: %w", path, normalizeErr)
		}

		return normalizeErr == nil
	})

	if normalizeErr != nil {
		return nil, normalizeErr
	}

	return normalized, nil
}

// passThroughBodySupported reports whether the raw inbound body can safely replace the
// outbound request body. Multipart formats are excluded.
func passThroughBodySupported(apiFormat llm.APIFormat) bool {
	//nolint:exhaustive // only multipart formats are excluded.
	switch apiFormat {
	case llm.APIFormatOpenAITranscription,
		llm.APIFormatOpenAITranslation,
		llm.APIFormatOpenAIImageEdit,
		llm.APIFormatOpenAIImageVariation:
		return false
	default:
		return true
	}
}

func passThroughBodyNeedsModelPatch(apiFormat llm.APIFormat) bool {
	//nolint:exhaustive // ohter format do not need model field.
	switch apiFormat {
	case llm.APIFormatOpenAIChatCompletion,
		llm.APIFormatOpenAIResponse,
		llm.APIFormatOpenAIResponseCompact,
		llm.APIFormatOpenAIEmbedding,
		llm.APIFormatJinaEmbedding,
		llm.APIFormatJinaRerank,
		llm.APIFormatAnthropicMessage,
		// Speech (TTS) has a JSON body with a model field; transcription/translation
		// use multipart bodies that cannot be patched via sjson, so they are excluded.
		llm.APIFormatOpenAISpeech:
		return true
	default:
		return false
	}
}

// applyUserAgentPassThrough creates a middleware that applies the User-Agent pass-through setting.
func applyUserAgentPassThrough(outbound *PersistentOutboundTransformer, systemService *biz.SystemService) pipeline.Middleware {
	return pipeline.OnRawRequest("user-agent-pass-through", func(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		channel := outbound.GetCurrentChannel()
		if channel == nil {
			return request, nil
		}

		var passThroughEnabled bool
		if channel.Settings != nil && channel.Settings.PassThroughUserAgent != nil {
			passThroughEnabled = *channel.Settings.PassThroughUserAgent
		} else {
			globalPassThrough, err := systemService.UserAgentPassThrough(ctx)
			if err != nil {
				log.Warn(ctx, "failed to get global user agent pass through setting", log.Cause(err))

				passThroughEnabled = false
			} else {
				passThroughEnabled = globalPassThrough
			}
		}

		// Handle User-Agent header based on pass-through setting
		// This must be done here (before persistRequestExecution) to ensure
		// the correct User-Agent is logged in request execution records.
		if request.Headers == nil {
			request.Headers = make(http.Header)
		}

		if passThroughEnabled {
			// Pass-through enabled: use the original client's User-Agent
			if outbound.state.LlmRequest != nil && outbound.state.LlmRequest.RawRequest != nil {
				if clientUA := outbound.state.LlmRequest.RawRequest.Headers.Get("User-Agent"); clientUA != "" {
					request.Headers.Set("User-Agent", clientUA)
				}
			}
		} else {
			// Pass-through disabled: use AxonHub's default User-Agent
			request.Headers.Set("User-Agent", "axonhub/1.0")
		}

		return request, nil
	})
}

// captureRawProviderResponse stores the raw provider response on state for response pass-through.
func captureRawProviderResponse(outbound *PersistentOutboundTransformer, systemService *biz.SystemService) pipeline.Middleware {
	return pipeline.OnRawResponse("capture-raw-provider-response", func(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
		if outbound.isPassThroughEnabled(ctx, systemService) {
			outbound.state.RawProviderResponse = response
		}

		return response, nil
	})
}

// applyPassThroughResponse replaces the transformed response with the raw provider response
// when PassThroughBody is enabled and the inbound/outbound API formats match.
func applyPassThroughResponse(outbound *PersistentOutboundTransformer, systemService *biz.SystemService) pipeline.Middleware {
	return pipeline.OnInboundRawResponse("pass-through-response", func(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
		if !outbound.isPassThroughEnabled(ctx, systemService) {
			return response, nil
		}

		rawResp := outbound.state.RawProviderResponse
		if rawResp == nil {
			return response, nil
		}

		log.Debug(ctx, "applying pass-through response",
			log.String("channel", outbound.GetCurrentChannel().Name),
			log.String("api_format", outbound.state.RawProviderRequest.APIFormat),
		)

		return rawResp, nil
	})
}

// captureRawProviderStream fans out raw provider stream events to both the pipeline
// (for transforms and LLM middlewares like connection tracking, performance recording)
// and a pass-through channel. The pipeline receives events via pipelineCh, while
// raw events are stored on state.RawStreamCh for pass-through delivery.
func captureRawProviderStream(outbound *PersistentOutboundTransformer, systemService *biz.SystemService) pipeline.Middleware {
	return pipeline.OnRawStream("capture-raw-provider-stream", func(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
		if !outbound.isPassThroughEnabled(ctx, systemService) {
			return stream, nil
		}

		channel := outbound.GetCurrentChannel()

		pipelineCh := make(chan *httpclient.StreamEvent, 64)
		rawStreamCh := make(chan *httpclient.StreamEvent, 64)
		outbound.state.RawStreamCh = rawStreamCh

		// Per-attempt local error storage: each attempt writes to its own variable so
		// concurrent defers from an abandoned goroutine and the new attempt's goroutine
		// never touch the same memory location, eliminating the data race on retries.
		var rawStreamErr error

		outbound.state.RawStreamErrRef = &rawStreamErr

		// Per-attempt cancelable context: PrepareForRetry / NextChannel call this cancel
		// to unblock the goroutine's channel sends and release the upstream HTTP connection
		// before the next attempt starts, preventing goroutine leaks.
		attemptCtx, cancel := context.WithCancel(ctx)
		var closeStreamOnce sync.Once
		closeStream := func() {
			closeStreamOnce.Do(func() {
				cancel()
				_ = stream.Close()
			})
		}
		outbound.state.RawStreamCancel = closeStream

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warn(ctx, "captureRawProviderStream goroutine panicked, recovering",
						log.Any("panic", r),
						log.String("channel", channel.Name),
					)
					rawStreamErr = fmt.Errorf("passthrough stream panic: %v", r)
				} else {
					rawStreamErr = stream.Err()
				}

				close(pipelineCh)
				close(rawStreamCh)
			}()
			// Ensure the context is cleaned up when the goroutine exits, regardless of
			// whether it finished naturally or was canceled by a retry.
			defer closeStream()

			for {
				select {
				case <-attemptCtx.Done():
					log.Debug(ctx, "context canceled before reading pass-through stream",
						log.String("channel", channel.Name))

					return
				default:
				}

				if !stream.Next() {
					return
				}

				event := stream.Current()
				// Use blocking sends so events are not silently dropped when a
				// consumer is slower than the upstream provider. Bail out on
				// attempt cancellation (retry) or request cancellation to avoid
				// blocking forever.
				select {
				case pipelineCh <- event:
				case <-attemptCtx.Done():
					log.Debug(ctx, "context canceled while sending pipeline event",
						log.String("channel", channel.Name))

					return
				}

				select {
				case rawStreamCh <- event:
				case <-attemptCtx.Done():
					log.Debug(ctx, "context canceled while sending pass-through event",
						log.String("channel", channel.Name))

					return
				}
			}
		}()

		return &passThroughChannelStream{ctx: ctx, ch: pipelineCh, errRef: &rawStreamErr, cancel: closeStream}, nil
	})
}

// applyPassThroughStream returns a stream of raw provider events when PassThroughBody is enabled.
// A goroutine drains the transformed pipeline stream so that LLM middlewares (connection tracking,
// performance recording, rate limit tracking) still process events.
func applyPassThroughStream(outbound *PersistentOutboundTransformer, systemService *biz.SystemService) pipeline.Middleware {
	return pipeline.OnInboundRawStream("pass-through-response-stream", func(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
		if !outbound.isPassThroughEnabled(ctx, systemService) {
			return stream, nil
		}

		rawCh := outbound.state.RawStreamCh
		if rawCh == nil {
			return stream, nil
		}

		// Snapshot the current attempt's error reference. If a future retry replaces
		// state.RawStreamErrRef, this stream still reads from the correct variable.
		errRef := outbound.state.RawStreamErrRef
		cancel := outbound.state.RawStreamCancel

		channel := outbound.GetCurrentChannel()

		log.Debug(ctx, "applying pass-through stream",
			log.String("channel", channel.Name),
		)

		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Warn(ctx, "pass-through stream drain panicked, recovering",
						log.Any("panic", r),
						log.String("channel", channel.Name),
					)
				}
			}()

			for stream.Next() {
				_ = stream.Current()
			}

			stream.Close()
		}()

		return &passThroughChannelStream{ctx: ctx, ch: rawCh, errRef: errRef, cancel: cancel}, nil
	})
}

// passThroughChannelStream wraps a channel as a Stream.
//
//nolint:containedctx // Required so Next() can observe request cancellation.
type passThroughChannelStream struct {
	ctx     context.Context
	ch      <-chan *httpclient.StreamEvent
	current *httpclient.StreamEvent
	errRef  *error
	cancel  context.CancelFunc
	once    sync.Once
}

func (s *passThroughChannelStream) Next() bool {
	if s.ctx != nil {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				return false
			}

			s.current = ev

			return true
		case <-s.ctx.Done():
			_ = s.Close()

			return false
		}
	}

	ev, ok := <-s.ch
	if !ok {
		return false
	}

	s.current = ev

	return true
}

func (s *passThroughChannelStream) Current() *httpclient.StreamEvent { return s.current }

func (s *passThroughChannelStream) Err() error {
	if s.errRef != nil {
		return *s.errRef
	}

	return nil
}

func (s *passThroughChannelStream) Close() error {
	s.once.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})

	return nil
}
