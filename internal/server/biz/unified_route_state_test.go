package biz

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestUnifiedRouteState_PrimaryFairnessIgnoresAvailability(t *testing.T) {
	state := NewUnifiedRouteState()
	failed := RouteKey{ChannelID: 3, CredentialID: "c", ActualModel: "gpt", APIFormat: "responses"}
	require.True(t, state.RecordTestVerdict(failed, RouteTestVerdict{Completed: true, Pass: false}))

	var counts [4]atomic.Int64
	var group sync.WaitGroup
	for range 600 {
		group.Add(1)
		go func() {
			defer group.Done()
			channelID, ok := state.NextPrimary("gpt", []int{3, 1, 2, 2})
			require.True(t, ok)
			counts[channelID].Add(1)
		}()
	}
	group.Wait()

	require.EqualValues(t, 200, counts[1].Load())
	require.EqualValues(t, 200, counts[2].Load())
	require.EqualValues(t, 200, counts[3].Load(), "TestChannel FAIL must not remove a primary slot")
}

func TestUnifiedRouteState_OnlyCompletedTestVerdictWrites(t *testing.T) {
	state := NewUnifiedRouteState()
	key := RouteKey{ChannelID: 1, CredentialID: "credential", ActualModel: "gpt", APIFormat: "responses"}

	require.False(t, state.RecordTestVerdict(key, RouteTestVerdict{Completed: false, Pass: false, Error: "tester canceled"}))
	require.False(t, state.Availability(key).Known)
	require.True(t, state.RecordTestVerdict(key, RouteTestVerdict{Completed: true, Pass: true}))
	require.Equal(t, RouteAvailability{Known: true, Available: true, LastTestAt: state.Availability(key).LastTestAt}, state.Availability(key))
}

func TestUnifiedRouteState_ProgrammaticTestsAreSingleflight(t *testing.T) {
	state := NewUnifiedRouteState()
	key := RouteKey{ChannelID: 1, CredentialID: "credential", ActualModel: "gpt", APIFormat: "responses"}
	var runs atomic.Int64
	release := make(chan struct{})
	for range 50 {
		state.TriggerTest(context.Background(), key, func(context.Context) (RouteTestVerdict, error) {
			runs.Add(1)
			<-release
			return RouteTestVerdict{Completed: true, Pass: true}, nil
		})
	}
	close(release)
	require.Eventually(t, func() bool { return state.Availability(key).Available }, time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, runs.Load())
}

func TestUnifiedRouteState_ChannelSnapshotIsDefensive(t *testing.T) {
	state := NewUnifiedRouteState()
	key := RouteKey{ChannelID: 9, CredentialID: "credential", ActualModel: "gpt", APIFormat: "responses"}
	state.RecordTestVerdict(key, RouteTestVerdict{Completed: true, Pass: true})
	snapshot := state.ChannelAvailabilities(9)
	delete(snapshot, key)
	require.True(t, state.Availability(key).Available)
}

func TestUnifiedRouteState_PrimaryCursorSurvivesMembershipChanges(t *testing.T) {
	state := NewUnifiedRouteState()
	first, _ := state.NextPrimary("gpt", []int{1, 2, 3})
	second, _ := state.NextPrimary("gpt", []int{1, 2, 3})
	afterChange, _ := state.NextPrimary("gpt", []int{1, 3, 4})
	afterWrap, _ := state.NextPrimary("gpt", []int{1, 4})
	require.Equal(t, []int{1, 2, 3, 4}, []int{first, second, afterChange, afterWrap})
}

func TestUnifiedRouteState_OlderVerdictCannotOverwriteNewer(t *testing.T) {
	state := NewUnifiedRouteState()
	key := RouteKey{ChannelID: 1, ActualModel: "gpt", APIFormat: "responses"}
	oldGeneration := state.NextTestGeneration()
	newGeneration := state.NextTestGeneration()
	require.True(t, state.RecordTestVerdictGeneration(key, newGeneration, RouteTestVerdict{Completed: true, Pass: true}))
	require.False(t, state.RecordTestVerdictGeneration(key, oldGeneration, RouteTestVerdict{Completed: true, Pass: false}))
	require.True(t, state.Availability(key).Available)
}

func TestUnifiedRouteState_InFlightSnapshotIsExact(t *testing.T) {
	state := NewUnifiedRouteState()
	key := RouteKey{ChannelID: 5, CredentialID: "fingerprint", ActualModel: "gpt", APIFormat: "responses"}
	started := make(chan struct{})
	release := make(chan struct{})
	state.TriggerTest(context.Background(), key, func(context.Context) (RouteTestVerdict, error) {
		close(started)
		<-release
		return RouteTestVerdict{Completed: true, Pass: true}, nil
	})
	<-started
	require.True(t, state.ChannelTestSnapshots(5)[key].InFlight)
	close(release)
	require.Eventually(t, func() bool {
		return !state.ChannelTestSnapshots(5)[key].InFlight && state.Availability(key).Available
	}, time.Second, 10*time.Millisecond)
}

func TestRouteConfigRevisionChangesWithEndpointAndProxy(t *testing.T) {
	channel := &Channel{Channel: &ent.Channel{
		ID:      1,
		BaseURL: "https://provider.example/v1",
		Endpoints: []objects.ChannelEndpoint{{
			APIFormat: "openai/responses",
			Path:      "/responses",
			Transport: objects.ChannelEndpointTransportHTTP,
		}},
		Settings: &objects.ChannelSettings{Proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "https://proxy-a.example"}},
	}}
	base := RouteConfigRevision(channel, "openai/responses", nil)
	channel.Endpoints[0].Path = "/v2/responses"
	endpointChanged := RouteConfigRevision(channel, "openai/responses", nil)
	overrideChanged := RouteConfigRevision(channel, "openai/responses", &httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "https://proxy-b.example", Password: "secret"})
	require.NotEmpty(t, base)
	require.NotEqual(t, base, endpointChanged)
	require.NotEqual(t, endpointChanged, overrideChanged)
	require.Len(t, overrideChanged, 16, "revision must not expose endpoint or proxy credentials")
}

func TestRouteCredentialFingerprintOAuthAccessRefreshIsStable(t *testing.T) {
	channel := &Channel{Channel: &ent.Channel{Credentials: objects.ChannelCredentials{
		OAuth: &objects.OAuthCredentials{
			ClientID:     "client-a",
			AccessToken:  "short-lived-access-a",
			RefreshToken: "stable-account-refresh",
			ExpiresAt:    time.Now().Add(time.Hour),
			Scopes:       []string{"models.read", "responses.write"},
		},
	}}}

	before := RouteCredentialFingerprint(channel, "")
	channel.Credentials.OAuth.AccessToken = "short-lived-access-b"
	channel.Credentials.OAuth.ExpiresAt = time.Now().Add(2 * time.Hour)
	channel.Credentials.OAuth.IDToken = "rotated-id-token"
	channel.Credentials.OAuth.Scopes = []string{"responses.write", "models.read"}
	afterRefresh := RouteCredentialFingerprint(channel, "")
	require.NotEmpty(t, before)
	require.Equal(t, before, afterRefresh, "access-token refresh for the same OAuth account must keep its exact-route identity")

	channel.Credentials.OAuth.RefreshToken = "different-account-refresh"
	require.NotEqual(t, before, RouteCredentialFingerprint(channel, ""), "refresh/account replacement must invalidate the old route verdict")
}

func TestRouteCredentialFingerprintOAuthWithoutRefreshTracksAccessToken(t *testing.T) {
	channel := &Channel{Channel: &ent.Channel{Credentials: objects.ChannelCredentials{
		OAuth: &objects.OAuthCredentials{ClientID: "client-a", AccessToken: "access-a"},
	}}}

	before := RouteCredentialFingerprint(channel, "")
	channel.Credentials.OAuth.AccessToken = "access-b"
	require.NotEqual(t, before, RouteCredentialFingerprint(channel, ""), "an access token is the effective identity when no refresh token exists")
}

func TestRouteCredentialFingerprintStructuredCredentialsAreCanonicalAndSensitive(t *testing.T) {
	channel := &Channel{Channel: &ent.Channel{Credentials: objects.ChannelCredentials{
		GCP: &objects.GCPCredential{
			Region:    "us-central1",
			ProjectID: "project-a",
			JSONData:  `{"client_email":"student@example.test","private_key":"secret-a"}`,
		},
	}}}

	before := RouteCredentialFingerprint(channel, "")
	channel.Credentials.GCP.JSONData = " { \n \t\"private_key\" : \"secret-a\", \"client_email\" : \"student@example.test\" } "
	require.Equal(t, before, RouteCredentialFingerprint(channel, ""), "formatting-only JSON changes must not create a new credential route")

	channel.Credentials.GCP.JSONData = `{"client_email":"student@example.test","private_key":"secret-b"}`
	afterSecretChange := RouteCredentialFingerprint(channel, "")
	require.NotEqual(t, before, afterSecretChange)
	channel.Credentials.GCP.ProjectID = "project-b"
	require.NotEqual(t, afterSecretChange, RouteCredentialFingerprint(channel, ""))
}

func TestRouteCredentialFingerprintAPIKeyCompatibility(t *testing.T) {
	channel := &Channel{Channel: &ent.Channel{Credentials: objects.ChannelCredentials{APIKey: "api-key"}}}
	require.Equal(t, CredentialFingerprint("api-key"), RouteCredentialFingerprint(channel, "api-key"))
}
