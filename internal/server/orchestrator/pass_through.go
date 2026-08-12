package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	openairesponses "github.com/looplj/axonhub/llm/transformer/openai/responses"
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

// normalizeResponsesRequestItemIDs is the single last-mile guard for both
// transformed and raw pass-through Responses requests. Keep it after all body
// overrides so no client or channel-specific path can bypass typed ID rules.
func normalizeResponsesRequestItemIDs() pipeline.Middleware {
	return pipeline.OnRawRequest("normalize-responses-request-item-ids", func(_ context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		if request == nil || (request.APIFormat != string(llm.APIFormatOpenAIResponse) &&
			request.APIFormat != string(llm.APIFormatOpenAIResponseCompact)) {
			return request, nil
		}

		body, err := openairesponses.NormalizeRequestInputItemIDs(request.Body)
		if err != nil {
			return nil, fmt.Errorf("normalize Responses request item ids: %w", err)
		}

		request.Body = body

		return request, nil
	})
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

// rawStreamErrStore is the synchronized terminal-error slot for one pass-through
// attempt. It is written by the fan-out producer goroutine and the pipeline drain
// goroutine, and read by passThroughChannelStream.Err(), which can run concurrently
// with the producer's deferred write when Next() bails out through ctx.Done().
// The first stored value wins so a specific failure (drain panic, producer panic)
// is not overwritten by the producer's later, less specific exit error.
type rawStreamErrStore struct {
	err atomic.Pointer[error]
}

func (s *rawStreamErrStore) Store(err error) {
	s.err.CompareAndSwap(nil, &err)
}

func (s *rawStreamErrStore) Load() error {
	if errPtr := s.err.Load(); errPtr != nil {
		return *errPtr
	}

	return nil
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

		// Per-attempt error storage: each attempt writes to its own store so
		// concurrent defers from an abandoned goroutine and the new attempt's goroutine
		// never touch the same memory location, eliminating the data race on retries.
		streamErrs := &rawStreamErrStore{}

		outbound.state.RawStreamErr = streamErrs

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
					streamErrs.Store(fmt.Errorf("passthrough stream panic: %v", r))
				} else {
					streamErrs.Store(stream.Err())
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

		return &passThroughChannelStream{ctx: ctx, ch: pipelineCh, errs: streamErrs, cancel: closeStream}, nil
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

		// Snapshot the current attempt's error store. If a future retry replaces
		// state.RawStreamErr, this stream still reads from the correct store.
		streamErrs := outbound.state.RawStreamErr
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
					// Without a drain consumer the fan-out producer eventually blocks
					// on pipelineCh and rawStreamCh starves, freezing the client stream
					// until the request times out. Record a terminal error first so the
					// client observes an explicit failure, then cancel the attempt so
					// the producer exits and closes both channels.
					if streamErrs != nil {
						streamErrs.Store(fmt.Errorf("pass-through pipeline drain panic: %v", r))
					}

					if cancel != nil {
						cancel()
					}

					closeDrainedStream(ctx, stream, channel.Name)
				}
			}()

			for stream.Next() {
				_ = stream.Current()
			}

			stream.Close()
		}()

		return &passThroughChannelStream{ctx: ctx, ch: rawCh, errs: streamErrs, cancel: cancel}, nil
	})
}

// closeDrainedStream finalizes the drained pipeline stream after the drain
// goroutine panicked. OutboundPersistentStream.Close is the only place an
// attempt is settled, so skipping it leaves request_execution pending forever
// with no chunks and no usage. A second panic here must not take the process
// down with it, since the client has already been unblocked at this point.
func closeDrainedStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent], channelName string) {
	defer func() {
		if r := recover(); r != nil {
			log.Warn(ctx, "pass-through stream finalization panicked after a drain panic",
				log.Any("panic", r),
				log.String("channel", channelName),
			)
		}
	}()

	if err := stream.Close(); err != nil {
		log.Warn(ctx, "Failed to finalize pass-through stream after a drain panic",
			log.Cause(err),
			log.String("channel", channelName),
		)
	}
}

// passThroughChannelStream wraps a channel as a Stream.
//
//nolint:containedctx // Required so Next() can observe request cancellation.
type passThroughChannelStream struct {
	ctx     context.Context
	ch      <-chan *httpclient.StreamEvent
	current *httpclient.StreamEvent
	errs    *rawStreamErrStore
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
	if s.errs != nil {
		return s.errs.Load()
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
