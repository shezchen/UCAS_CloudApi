package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/streams"
)

func affinityMessage(role, content string) llm.Message {
	return llm.Message{
		Role: role,
		Content: llm.MessageContent{
			Content: &content,
		},
	}
}

func TestSessionAffinityRequestKey_StableContextPrefixAndIdentityScope(t *testing.T) {
	tracker := newSessionAffinityTrackerWithSecret("test-secret", time.Hour, 8)
	apiKey := &ent.APIKey{ProjectID: 3, UserID: 11}
	first := &llm.Request{Messages: []llm.Message{
		affinityMessage("system", "stable instructions"),
		affinityMessage("user", "first question"),
	}}
	later := &llm.Request{Messages: append(
		append([]llm.Message{}, first.Messages...),
		affinityMessage("assistant", "answer"),
		affinityMessage("user", "follow-up"),
	)}

	firstKey, ok := tracker.RequestKey(context.Background(), first, apiKey)
	require.True(t, ok)
	laterKey, ok := tracker.RequestKey(context.Background(), later, apiKey)
	require.True(t, ok)
	require.Equal(t, firstKey, laterKey)
	require.Len(t, firstKey, sha256HexLength)
	require.NotContains(t, firstKey, "first question")

	otherUserKey, ok := tracker.RequestKey(context.Background(), first, &ent.APIKey{ProjectID: 3, UserID: 12})
	require.True(t, ok)
	require.NotEqual(t, firstKey, otherUserKey)

	otherPrefix := &llm.Request{Messages: []llm.Message{
		affinityMessage("system", "stable instructions"),
		affinityMessage("user", "another conversation"),
	}}
	otherPrefixKey, ok := tracker.RequestKey(context.Background(), otherPrefix, apiKey)
	require.True(t, ok)
	require.NotEqual(t, firstKey, otherPrefixKey)
}

const sha256HexLength = 64

func TestSessionAffinityTracker_BoundedLRUAndTTL(t *testing.T) {
	tracker := newSessionAffinityTrackerWithSecret("test-secret", time.Hour, 2)
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }

	tracker.Bind("one", 1)
	now = now.Add(time.Minute)
	tracker.Bind("two", 2)
	now = now.Add(time.Minute)
	_, ok := tracker.Lookup("one")
	require.True(t, ok)
	now = now.Add(time.Minute)
	tracker.Bind("three", 3)

	_, ok = tracker.Lookup("two")
	require.False(t, ok, "least recently used entry must be evicted at the bound")
	channelID, ok := tracker.Lookup("one")
	require.True(t, ok)
	require.Equal(t, 1, channelID)
	channelID, ok = tracker.Lookup("three")
	require.True(t, ok)
	require.Equal(t, 3, channelID)

	now = now.Add(2 * time.Hour)
	_, ok = tracker.Lookup("one")
	require.False(t, ok)
}

func TestSessionAffinityRecording_BindsOnlySemanticSuccessAndRebindsAfterFailover(t *testing.T) {
	tracker := newSessionAffinityTrackerWithSecret("test-secret", time.Hour, 8)
	key := strings.Repeat("a", sha256HexLength)
	channelOne := &biz.Channel{Channel: &ent.Channel{ID: 1}}
	channelTwo := &biz.Channel{Channel: &ent.Channel{ID: 2}}
	state := &PersistenceState{
		SessionAffinityKey: key,
		CurrentCandidate:   &ChannelModelsCandidate{Channel: channelOne},
	}
	middleware := &sessionAffinityRecording{
		outbound: &PersistentOutboundTransformer{state: state},
		tracker:  tracker,
	}

	empty := &llm.Response{Choices: []llm.Choice{{Message: &llm.Message{Role: "assistant"}}}}
	_, err := middleware.OnOutboundLlmResponse(context.Background(), empty)
	require.NoError(t, err)
	_, ok := tracker.Lookup(key)
	require.False(t, ok)

	incompleteText := "partial response"
	incompleteFinishReason := "length"
	_, err = middleware.OnOutboundLlmResponse(context.Background(), &llm.Response{
		ProtocolStatus:   "incomplete",
		IncompleteReason: "max_output_tokens",
		Choices: []llm.Choice{{
			Message:      &llm.Message{Role: "assistant", Content: llm.MessageContent{Content: &incompleteText}},
			FinishReason: &incompleteFinishReason,
		}},
	})
	require.NoError(t, err)
	_, ok = tracker.Lookup(key)
	require.False(t, ok, "an explicit Responses incomplete terminal must never bind session affinity")

	toolOnly := &llm.Response{Choices: []llm.Choice{{
		Message: &llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1"}}},
	}}}
	_, err = middleware.OnOutboundLlmResponse(context.Background(), toolOnly)
	require.NoError(t, err)
	channelID, ok := tracker.Lookup(key)
	require.True(t, ok)
	require.Equal(t, 1, channelID)

	state.CurrentCandidate = &ChannelModelsCandidate{Channel: channelTwo}
	text := "recovered on failover"
	_, err = middleware.OnOutboundLlmResponse(context.Background(), &llm.Response{
		Choices: []llm.Choice{{Message: &llm.Message{
			Role:    "assistant",
			Content: llm.MessageContent{Content: &text},
		}}},
	})
	require.NoError(t, err)
	channelID, ok = tracker.Lookup(key)
	require.True(t, ok)
	require.Equal(t, 2, channelID)
}

func TestSessionAffinityRecording_StreamIncompleteWithContentDoesNotBind(t *testing.T) {
	tracker := newSessionAffinityTrackerWithSecret("test-secret", time.Hour, 8)
	key := strings.Repeat("b", sha256HexLength)
	state := &PersistenceState{
		SessionAffinityKey: key,
		CurrentCandidate:   &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{ID: 1}}},
		AttemptAccepted:    true,
	}
	middleware := &sessionAffinityRecording{
		outbound: &PersistentOutboundTransformer{state: state},
		tracker:  tracker,
	}
	text := "partial response"
	finishReason := "length"
	stream, err := middleware.OnOutboundLlmStream(context.Background(), streams.SliceStream([]*llm.Response{
		{Choices: []llm.Choice{{Delta: &llm.Message{Content: llm.MessageContent{Content: &text}}}}},
		{
			ProtocolStatus:   "incomplete",
			IncompleteReason: "max_output_tokens",
			Choices:          []llm.Choice{{FinishReason: &finishReason}},
		},
	}))
	require.NoError(t, err)
	for stream.Next() {
		_ = stream.Current()
	}
	require.NoError(t, stream.Close())
	_, ok := tracker.Lookup(key)
	require.False(t, ok, "committed partial output with an incomplete terminal must not bind session affinity")
}

func TestSessionAffinityStrategy_PrefersRememberedCandidate(t *testing.T) {
	strategy := NewSessionAffinityStrategy()
	ctx := contextWithSessionAffinityChannel(context.Background(), 2)

	require.Zero(t, strategy.Score(ctx, &biz.Channel{Channel: &ent.Channel{ID: 1}}))
	require.Equal(t, sessionAffinityScore, strategy.Score(ctx, &biz.Channel{Channel: &ent.Channel{ID: 2}}))
}

func TestSessionAffinityTracker_ConcurrentBoundedAccess(t *testing.T) {
	tracker := newSessionAffinityTrackerWithSecret("test-secret", time.Hour, 32)
	var wg sync.WaitGroup

	for worker := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := range 50 {
				key := fmt.Sprintf("%d-%d", worker, iteration)
				tracker.Bind(key, worker+1)
				_, _ = tracker.Lookup(key)
			}
		}()
	}
	wg.Wait()

	tracker.mu.Lock()
	entryCount := len(tracker.entries)
	tracker.mu.Unlock()
	require.LessOrEqual(t, entryCount, tracker.maxEntries)
}

func TestSelectCandidates_ExplicitTraceIsStickyAndCountedOnlyWhenNew(t *testing.T) {
	ctx, client := setupTest(t)
	project := createTestProject(t, ctx, client)
	channels := createTestChannels(t, ctx, client)
	requestService := newTestRequestServiceForChannels(client, newTestSystemService(client))

	trace, err := client.Trace.Create().
		SetProjectID(project.ID).
		SetTraceID("explicit-session-with-history").
		Save(ctx)
	require.NoError(t, err)
	_, err = client.Request.Create().
		SetProjectID(project.ID).
		SetTraceID(trace.ID).
		SetChannelID(channels[1].ID).
		SetModelID("gpt-test").
		SetStatus("completed").
		SetSource("api").
		SetRequestBody([]byte(`{}`)).
		Save(ctx)
	require.NoError(t, err)

	candidates := []*ChannelModelsCandidate{
		{
			Channel: &biz.Channel{Channel: channels[0]},
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-test", ActualModel: "gpt-test"}},
		},
		{
			Channel: &biz.Channel{Channel: channels[1]},
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-test", ActualModel: "gpt-test"}},
		},
	}
	routes := biz.NewUnifiedRouteState()

	runSelection := func(selectionCtx context.Context) *PersistenceState {
		t.Helper()
		state := &PersistenceState{
			RequestService:    requestService,
			CandidateSelector: &staticChannelSelector{candidates: candidates},
			UnifiedRoutes:     routes,
		}
		middleware := selectCandidates(&PersistentInboundTransformer{state: state})
		_, selectErr := middleware.OnInboundLlmRequest(selectionCtx, &llm.Request{Model: "gpt-test"})
		require.NoError(t, selectErr)
		require.NotEmpty(t, state.ChannelModelsCandidates)
		return state
	}

	traceCtx := contexts.WithTrace(ctx, trace)
	for range 2 {
		state := runSelection(traceCtx)
		require.Equal(t, channels[1].ID, state.ChannelModelsCandidates[0].Channel.ID)
	}

	newTrace, err := client.Trace.Create().
		SetProjectID(project.ID).
		SetTraceID("brand-new-explicit-session").
		Save(ctx)
	require.NoError(t, err)
	state := runSelection(contexts.WithTrace(ctx, newTrace))
	require.Equal(t, channels[0].ID, state.ChannelModelsCandidates[0].Channel.ID)
}

func TestSelectCandidates_ContextPrefixAffinityKeepsChannelWithoutDoubleCounting(t *testing.T) {
	ctx, _ := setupTest(t)
	affinity := newSessionAffinityTrackerWithSecret("test-secret", time.Hour, 16)
	candidates := []*ChannelModelsCandidate{
		{
			Channel: &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "one"}},
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-test", ActualModel: "gpt-test"}},
		},
		{
			Channel: &biz.Channel{Channel: &ent.Channel{ID: 2, Name: "two"}},
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-test", ActualModel: "gpt-test"}},
		},
	}
	routes := biz.NewUnifiedRouteState()

	runSelection := func(req *llm.Request) *PersistenceState {
		t.Helper()
		state := &PersistenceState{
			CandidateSelector: &staticChannelSelector{candidates: candidates},
			SessionAffinity:   affinity,
			UnifiedRoutes:     routes,
		}
		middleware := selectCandidates(&PersistentInboundTransformer{state: state})
		_, err := middleware.OnInboundLlmRequest(ctx, req)
		require.NoError(t, err)
		require.NotEmpty(t, state.ChannelModelsCandidates)
		return state
	}

	firstRequest := &llm.Request{
		Model: "gpt-test",
		Messages: []llm.Message{
			affinityMessage("system", "stable instructions"),
			affinityMessage("user", "first question"),
		},
	}
	firstState := runSelection(firstRequest)
	require.Equal(t, 1, firstState.ChannelModelsCandidates[0].Channel.ID)
	require.NotEmpty(t, firstState.SessionAffinityKey)

	firstState.CurrentCandidate = firstState.ChannelModelsCandidates[0]
	recorder := &sessionAffinityRecording{
		outbound: &PersistentOutboundTransformer{state: firstState},
		tracker:  affinity,
	}
	_, err := recorder.OnOutboundLlmResponse(ctx, &llm.Response{Choices: []llm.Choice{{
		Message: &llm.Message{
			Role:      "assistant",
			ToolCalls: []llm.ToolCall{{ID: "call-1", Type: "function"}},
		},
	}}})
	require.NoError(t, err)

	followUp := &llm.Request{
		Model: "gpt-test",
		Messages: append(
			append([]llm.Message{}, firstRequest.Messages...),
			affinityMessage("assistant", "tool request"),
			affinityMessage("user", "follow-up"),
		),
	}
	secondState := runSelection(followUp)
	require.Equal(t, firstState.SessionAffinityKey, secondState.SessionAffinityKey)
	require.Equal(t, 1, secondState.ChannelModelsCandidates[0].Channel.ID,
		"the stable prefix must keep the recovered conversation on its successful channel")
}
