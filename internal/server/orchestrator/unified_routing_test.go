package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

func unifiedCandidate(id int, keys ...string) *ChannelModelsCandidate {
	return &ChannelModelsCandidate{
		Channel: &biz.Channel{Channel: &ent.Channel{
			ID:          id,
			Name:        "channel",
			Credentials: objects.ChannelCredentials{APIKeys: keys},
		}},
		Models:    []biz.ChannelModelEntry{{RequestModel: "gpt", ActualModel: "gpt"}},
		APIFormat: "responses",
	}
}

func unifiedRoute(candidate *ChannelModelsCandidate, modelIndex int, credential string) biz.RouteKey {
	return biz.RouteKey{
		ChannelID:      candidate.Channel.ID,
		CredentialID:   biz.CredentialFingerprint(credential),
		ActualModel:    candidate.Models[modelIndex].ActualModel,
		APIFormat:      candidate.APIFormat,
		ConfigRevision: biz.RouteConfigRevision(candidate.Channel, candidate.APIFormat, nil),
	}
}

func TestUnifiedRescue_UsesPassRoutesAndForcesExactCredential(t *testing.T) {
	routes := biz.NewUnifiedRouteState()
	keyA := "key-a"
	keyB := "key-b"
	failedCandidate := unifiedCandidate(3, "bad")
	unknownCandidate := unifiedCandidate(1, keyA)
	passCandidate := unifiedCandidate(2, keyB)
	routes.RecordTestVerdict(unifiedRoute(passCandidate, 0, keyB), biz.RouteTestVerdict{Completed: true, Pass: true})
	routes.RecordTestVerdict(unifiedRoute(failedCandidate, 0, "bad"), biz.RouteTestVerdict{Completed: true, Pass: false})

	state := &PersistenceState{
		OriginalModel: "gpt",
		UnifiedRoutes: routes,
		ChannelModelsCandidates: []*ChannelModelsCandidate{
			failedCandidate,  // fair primary, now failed
			unknownCandidate, // unknown, not a verified rescue
			passCandidate,    // exact PASS rescue
		},
		CurrentCandidateIndex: 0,
		CurrentCandidate:      failedCandidate,
		AttemptedRoutes:       map[biz.RouteKey]struct{}{unifiedRoute(failedCandidate, 0, "bad"): {}},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	require.True(t, outbound.HasMoreChannels())
	require.NoError(t, outbound.NextChannel(context.Background()))
	require.Equal(t, 2, state.CurrentCandidate.Channel.ID)
	require.Equal(t, keyB, state.ForcedCredential, "rescue must use the credential that actually passed TestChannel")
}

func TestUnifiedRescue_NoKnownPassWalksEveryRingSlot(t *testing.T) {
	one := unifiedCandidate(1)
	two := unifiedCandidate(2)
	three := unifiedCandidate(3)
	state := &PersistenceState{
		OriginalModel: "gpt",
		UnifiedRoutes: biz.NewUnifiedRouteState(),
		ChannelModelsCandidates: []*ChannelModelsCandidate{
			one, two, three,
		},
		CurrentCandidateIndex: 0,
		CurrentCandidate:      one,
		AttemptedRoutes:       map[biz.RouteKey]struct{}{unifiedRoute(one, 0, ""): {}},
	}
	outbound := &PersistentOutboundTransformer{state: state}
	require.NoError(t, outbound.NextChannel(context.Background()))
	require.Equal(t, 2, state.CurrentCandidate.Channel.ID)
	state.AttemptedRoutes[unifiedRoute(two, 0, "")] = struct{}{}
	require.NoError(t, outbound.NextChannel(context.Background()))
	require.Equal(t, 3, state.CurrentCandidate.Channel.ID)
}

func TestUnifiedRescue_ExactRoutesBeforeRepeatAndAlternateActualModel(t *testing.T) {
	candidate := unifiedCandidate(1, "key-a", "key-b")
	candidate.Models = append(candidate.Models, biz.ChannelModelEntry{RequestModel: "gpt", ActualModel: "gpt-fallback"})
	routes := biz.NewUnifiedRouteState()
	all := []biz.RouteKey{
		unifiedRoute(candidate, 0, "key-a"),
		unifiedRoute(candidate, 0, "key-b"),
		unifiedRoute(candidate, 1, "key-a"),
		unifiedRoute(candidate, 1, "key-b"),
	}
	for _, route := range all {
		routes.RecordTestVerdict(route, biz.RouteTestVerdict{Completed: true, Pass: true})
	}
	state := &PersistenceState{
		OriginalModel:           "gpt",
		UnifiedRoutes:           routes,
		ChannelModelsCandidates: []*ChannelModelsCandidate{candidate},
		CurrentCandidate:        candidate,
		AttemptedRoutes:         map[biz.RouteKey]struct{}{all[0]: {}},
	}
	outbound := &PersistentOutboundTransformer{state: state}

	seen := map[biz.RouteKey]struct{}{all[0]: {}}
	for range 3 {
		require.NoError(t, outbound.NextChannel(context.Background()))
		selected := unifiedRoute(candidate, state.CurrentModelIndex, state.ForcedCredential)
		_, duplicate := seen[selected]
		require.False(t, duplicate, "every exact PASS route must be tried before repetition")
		seen[selected] = struct{}{}
		state.AttemptedRoutes[selected] = struct{}{}
	}
	require.Len(t, seen, 4)
	require.NoError(t, outbound.NextChannel(context.Background()), "retry budget may wrap only after all exact routes")
}

func TestUnifiedPrimary_DeletedAffinityFallsBackAndAdvancesFairCursor(t *testing.T) {
	routes := biz.NewUnifiedRouteState()
	one := unifiedCandidate(1)
	two := unifiedCandidate(2)
	state := &PersistenceState{UnifiedRoutes: routes}

	withDeletedAffinity := contextWithSessionAffinityChannel(context.Background(), 99)
	first := orderUnifiedCandidates(withDeletedAffinity, state, "gpt", []*ChannelModelsCandidate{one, two})
	require.Equal(t, 1, first[0].Channel.ID)
	second := orderUnifiedCandidates(context.Background(), state, "gpt", []*ChannelModelsCandidate{one, two})
	require.Equal(t, 2, second[0].Channel.ID, "invalid affinity must consume a normal fair slot")
}

func TestUnifiedRescue_StaleConfigPassIsNotReusable(t *testing.T) {
	candidate := unifiedCandidate(1, "key")
	candidate.Channel.Endpoints = []objects.ChannelEndpoint{{APIFormat: "responses", Path: "/old"}}
	routes := biz.NewUnifiedRouteState()
	routes.RecordTestVerdict(unifiedRoute(candidate, 0, "key"), biz.RouteTestVerdict{Completed: true, Pass: true})

	candidate.Channel.Endpoints[0].Path = "/new"
	state := &PersistenceState{
		OriginalModel:           "gpt",
		UnifiedRoutes:           routes,
		ChannelModelsCandidates: []*ChannelModelsCandidate{candidate},
	}
	outbound := &PersistentOutboundTransformer{state: state}
	require.Empty(t, outbound.routeOptions(true, false), "old endpoint PASS must not authorize the edited route")
	require.NotEmpty(t, outbound.routeOptions(false, false), "edited route remains eligible for fair best-effort testing")
}

func TestUnifiedRoute_ProxyOverrideRevisionMatchesAttemptAndRescue(t *testing.T) {
	candidate := unifiedCandidate(1, "key")
	proxy := &httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "https://proxy.example", Password: "secret"}
	state := &PersistenceState{
		UnifiedRoutes:           biz.NewUnifiedRouteState(),
		ChannelModelsCandidates: []*ChannelModelsCandidate{candidate},
		CurrentCandidate:        candidate,
		Proxy:                   proxy,
	}
	outbound := &PersistentOutboundTransformer{state: state}
	attempt, _, _, ok := outbound.currentRoute(context.Background())
	require.True(t, ok)
	options := outbound.routeOptions(false, false)
	require.Len(t, options, 1)
	require.Equal(t, attempt, options[0].route)
	require.Equal(t, biz.RouteConfigRevision(candidate.Channel, candidate.APIFormat, proxy), attempt.ConfigRevision)
}

func TestUnifiedRoute_StaticTestProviderKeepsExactCredential(t *testing.T) {
	candidate := unifiedCandidate(1, "key-a", "key-b")
	candidate.ForcedCredential = "key-b"
	outbound := &PersistentOutboundTransformer{state: &PersistenceState{CurrentCandidate: candidate}}
	route, credential, _, ok := outbound.currentRoute(context.Background())
	require.True(t, ok)
	require.Equal(t, "key-b", credential)
	require.Equal(t, biz.CredentialFingerprint("key-b"), route.CredentialID)
}

func TestUnifiedRoute_OAuthIdentityMatchesAttemptAndRescueUniverse(t *testing.T) {
	// Even one stray legacy API key must not turn an OAuth account into an API-key
	// route: otherwise programmatic health tests would exercise a different
	// credential path than production.
	candidate := unifiedCandidate(1, "unused-api-key")
	candidate.Channel.Credentials.OAuth = &objects.OAuthCredentials{
		ClientID:     "client",
		AccessToken:  "short-lived-access",
		RefreshToken: "stable-account-refresh",
	}
	state := &PersistenceState{
		UnifiedRoutes:           biz.NewUnifiedRouteState(),
		ChannelModelsCandidates: []*ChannelModelsCandidate{candidate},
		CurrentCandidate:        candidate,
	}
	outbound := &PersistentOutboundTransformer{state: state}

	attempt, credential, _, ok := outbound.currentRoute(context.Background())
	require.True(t, ok)
	require.Empty(t, credential, "OAuth transformers do not execute legacy API keys stored beside OAuth")
	require.Equal(t, biz.RouteCredentialFingerprint(candidate.Channel, ""), attempt.CredentialID)

	options := outbound.routeOptions(false, false)
	require.Len(t, options, 1, "one OAuth account must create exactly one rescue route")
	require.Equal(t, attempt, options[0].route)
	require.Empty(t, options[0].credential)
}
