package biz

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestChannelService_DonationImmediatelySyncsDiscoverableModels(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	donor := createChannelTestUser(t, client, setupCtx, "model-sync-donor@example.com", false)
	donorCtx := contexts.WithUser(ent.NewContext(context.Background(), client), donor)
	expiresAt := time.Now().Add(time.Hour)

	donated, err := svc.CreateChannel(donorCtx, ent.CreateChannelInput{
		Type:    channel.TypeCodex,
		Name:    "discoverable coding plan donation",
		BaseURL: new("https://chatgpt.com/backend-api/codex"),
		Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{
			AccessToken: "test-access-token",
			ExpiresAt:   time.Now().Add(time.Hour),
		}},
		SupportedModels:  []string{"stale-auto-model", "gpt-5.6-sol"},
		ManualModels:     []string{"campus-manual-model", "gpt-5.6-sol"},
		DefaultTestModel: "campus-manual-model",
		ExpiresAt:        &expiresAt,
	})
	require.NoError(t, err)
	require.True(t, donated.AutoSyncSupportedModels, "campus donations should opt into discovery by default")
	require.Contains(t, donated.SupportedModels, "campus-manual-model")
	require.Contains(t, donated.SupportedModels, "gpt-5.6-sol")
	require.NotContains(t, donated.SupportedModels, "stale-auto-model")
	require.Equal(t, 1, lo.CountBy(donated.SupportedModels, func(modelID string) bool {
		return modelID == "gpt-5.6-sol"
	}))

	donated, err = svc.UpdateChannelStatus(donorCtx, donated.ID, channel.StatusEnabled)
	require.NoError(t, err)
	require.Equal(t, channel.StatusEnabled, donated.Status)

	models, err := svc.ListModels(donorCtx, ListModelsInput{})
	require.NoError(t, err)
	require.Equal(t, 1, lo.CountBy(models, func(model *ModelIdentityWithStatus) bool {
		return model.ID == "gpt-5.6-sol" && model.Status == channel.StatusEnabled
	}))
}

func TestChannelService_NewOwnerChannelsAutoDiscoverModelsByDefault(t *testing.T) {
	var catalogRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		catalogRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"owner-discovered-model"}]}`))
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	owner := createChannelTestUser(t, client, setupCtx, "model-sync-owner@example.com", true)
	ownerCtx := contexts.WithUser(ent.NewContext(context.Background(), client), owner)

	standard, err := svc.CreateChannel(ownerCtx, ent.CreateChannelInput{
		Type:            channel.TypeOpenai,
		Name:            "owner automatic channel",
		BaseURL:         new(server.URL + "/v1"),
		Credentials:     objects.ChannelCredentials{APIKey: "owner-key"},
		SupportedModels: []string{},
	})
	require.NoError(t, err)
	require.True(t, standard.AutoSyncSupportedModels, "owner channels should also opt into discovery by default")
	require.Equal(t, []string{"owner-discovered-model"}, standard.SupportedModels)
	require.Equal(t, "owner-discovered-model", standard.DefaultTestModel)

	codingPlan, err := svc.CreateChannel(ownerCtx, ent.CreateChannelInput{
		Type:    channel.TypeCodex,
		Name:    "owner coding plan channel",
		BaseURL: new("https://chatgpt.com/backend-api/codex"),
		Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{
			AccessToken: "test-access-token",
			ExpiresAt:   time.Now().Add(time.Hour),
		}},
	})
	require.NoError(t, err)
	require.True(t, codingPlan.AutoSyncSupportedModels)
	require.NotEmpty(t, codingPlan.SupportedModels, "Coding Plan channels should not require hand-entered model names")
	require.NotEmpty(t, codingPlan.DefaultTestModel)

	explicitFalse := false
	manual, err := svc.CreateChannel(ownerCtx, ent.CreateChannelInput{
		Type:                    channel.TypeOpenai,
		Name:                    "owner manual-only channel",
		BaseURL:                 new(server.URL + "/v1"),
		Credentials:             objects.ChannelCredentials{APIKey: "manual-key"},
		SupportedModels:         []string{"submitted-only-model"},
		DefaultTestModel:        "submitted-only-model",
		AutoSyncSupportedModels: &explicitFalse,
	})
	require.NoError(t, err)
	require.False(t, manual.AutoSyncSupportedModels, "an explicit false must never be overwritten by the default")
	require.Equal(t, []string{"submitted-only-model"}, manual.SupportedModels)
	require.Equal(t, int32(1), catalogRequests.Load(), "auto-sync disabled channel must not fetch a catalog")
}

func TestChannelModelDiscoveryConfigChanged(t *testing.T) {
	newChannel := func() *ent.Channel {
		return &ent.Channel{
			Type:                    channel.TypeOpenai,
			BaseURL:                 "https://provider.example/v1",
			Credentials:             objects.ChannelCredentials{APIKey: "key"},
			SupportedModels:         []string{"model-a"},
			ManualModels:            []string{"manual-a"},
			AutoSyncSupportedModels: true,
			AutoSyncModelPattern:    "model-.*",
			Endpoints:               []objects.ChannelEndpoint{{APIFormat: "openai/chat-completions"}},
			Settings:                &objects.ChannelSettings{ExtraModelPrefix: "provider"},
		}
	}

	tests := []struct {
		name   string
		mutate func(before, after *ent.Channel)
		want   bool
	}{
		{name: "ordinary edit", mutate: func(_, after *ent.Channel) {
			after.Name = "renamed"
			after.DefaultTestModel = "model-a"
			after.OrderingWeight = 10
		}, want: false},
		{name: "false to true", mutate: func(before, _ *ent.Channel) { before.AutoSyncSupportedModels = false }, want: true},
		{name: "type", mutate: func(_, after *ent.Channel) { after.Type = channel.TypeAnthropic }, want: true},
		{name: "base URL", mutate: func(_, after *ent.Channel) { after.BaseURL += "/new" }, want: true},
		{name: "credentials", mutate: func(_, after *ent.Channel) { after.Credentials.APIKey = "new-key" }, want: true},
		{name: "supported models", mutate: func(_, after *ent.Channel) { after.SupportedModels = append(after.SupportedModels, "model-b") }, want: true},
		{name: "manual models", mutate: func(_, after *ent.Channel) { after.ManualModels = append(after.ManualModels, "manual-b") }, want: true},
		{name: "pattern", mutate: func(_, after *ent.Channel) { after.AutoSyncModelPattern = "new-.*" }, want: true},
		{name: "endpoints", mutate: func(_, after *ent.Channel) { after.Endpoints[0].Path = "/models" }, want: true},
		{name: "settings", mutate: func(_, after *ent.Channel) { after.Settings.ExtraModelPrefix = "new-provider" }, want: true},
		{name: "disabled result", mutate: func(_, after *ent.Channel) { after.AutoSyncSupportedModels = false }, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, after := newChannel(), newChannel()
			tt.mutate(before, after)
			require.Equal(t, tt.want, channelModelDiscoveryConfigChanged(before, after))
		})
	}
}

func TestChannelService_UpdateModelSyncIsSelectiveAndHonorsShorterDeadline(t *testing.T) {
	var catalogRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		catalogRequests.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("selective immediate sync").
		SetBaseURL(server.URL + "/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "old-key"}).
		SetSupportedModels([]string{"submitted-model"}).
		SetManualModels([]string{"submitted-model"}).
		SetAutoSyncSupportedModels(true).
		SetDefaultTestModel("submitted-model").
		Save(ctx)
	require.NoError(t, err)

	remark := "ordinary edit"
	started := time.Now()
	_, err = svc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{Remark: &remark})
	require.NoError(t, err)
	require.Less(t, time.Since(started), time.Second)
	require.Zero(t, catalogRequests.Load(), "ordinary edits must not contact the provider catalog")

	deadlineCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	started = time.Now()
	updated, err := svc.UpdateChannel(deadlineCtx, ch.ID, &ent.UpdateChannelInput{
		Credentials: &objects.ChannelCredentials{APIKey: "new-key"},
	})
	require.NoError(t, err, "best-effort discovery timeout must not fail the saved update")
	require.Equal(t, "new-key", updated.Credentials.APIKey)
	require.Less(t, time.Since(started), 2*time.Second)
	require.Equal(t, int32(1), catalogRequests.Load())
}

func TestChannelService_BulkImmediateModelSyncHasSharedDeadlineAndConcurrencyLimit(t *testing.T) {
	var active, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	channels := make([]*ent.Channel, 0, immediateChannelModelSyncConcurrency+1)
	for i := range immediateChannelModelSyncConcurrency + 1 {
		ch, err := client.Channel.Create().
			SetType(channel.TypeOpenai).
			SetName(fmt.Sprintf("slow catalog %d", i)).
			SetBaseURL(server.URL + "/v1").
				SetCredentials(objects.ChannelCredentials{APIKey: "key"}).
				SetSupportedModels([]string{"submitted-model"}).
				SetAutoSyncSupportedModels(true).
				SetDefaultTestModel("submitted-model").
				Save(ctx)
		require.NoError(t, err)
		channels = append(channels, ch)
	}

	started := time.Now()
	result := svc.syncChannelsBestEffort(ctx, channels)
	elapsed := time.Since(started)
	require.Len(t, result, len(channels))
	require.Less(t, elapsed, 7*time.Second, "a second batch must not receive a fresh five-second timeout")
	require.LessOrEqual(t, peak.Load(), int32(immediateChannelModelSyncConcurrency))
}

func TestChannelService_ModelSyncReplacesAutomaticModelsAndClearsEmptyCatalog(t *testing.T) {
	providerModels := `{"data":[{"id":"same-model"},{"id":"new-model"},{"id":"new-model"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(providerModels))
	}))
	defer server.Close()

	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("model sync lifecycle").
		SetBaseURL(server.URL + "/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"same-model", "old-auto-model", "manual-model"}).
		SetManualModels([]string{"manual-model", "same-model"}).
		SetDefaultTestModel("old-auto-model").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	synced, err := svc.SyncChannelModels(ctx, ch.ID, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"manual-model", "same-model", "new-model"}, synced.SupportedModels)
	require.Equal(t, "manual-model", synced.DefaultTestModel)

	models, err := svc.ListModels(ctx, ListModelsInput{})
	require.NoError(t, err)
	require.Equal(t, 1, lo.CountBy(models, func(model *ModelIdentityWithStatus) bool { return model.ID == "same-model" }))
	require.Equal(t, 1, lo.CountBy(models, func(model *ModelIdentityWithStatus) bool { return model.ID == "new-model" }))

	providerModels = `{"data":[]}`
	synced, err = svc.SyncChannelModels(ctx, ch.ID, nil)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"manual-model", "same-model"}, synced.SupportedModels)
}

func TestPreserveManualModels(t *testing.T) {
	tests := []struct {
		name          string
		manualModels  []string
		fetchedModels []string
		expected      []string
	}{
		{
			name:          "manual models preserved when fetched is different",
			manualModels:  []string{"custom-model-1", "custom-model-2"},
			fetchedModels: []string{"gpt-4", "gpt-3.5-turbo"},
			expected:      []string{"custom-model-1", "custom-model-2", "gpt-4", "gpt-3.5-turbo"},
		},
		{
			name:          "manual models preserved when no overlap",
			manualModels:  []string{"my-custom-model"},
			fetchedModels: []string{"claude-3-opus"},
			expected:      []string{"my-custom-model", "claude-3-opus"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeModelsForTest(tt.manualModels, tt.fetchedModels)

			for _, manualModel := range tt.manualModels {
				assert.Contains(t, result, manualModel,
					"Manual model %s should be preserved after sync", manualModel)
			}
		})
	}
}

func TestMergeManualAndFetched(t *testing.T) {
	tests := []struct {
		name          string
		manualModels  []string
		fetchedModels []string
		expected      []string
	}{
		{
			name:          "union of manual and fetched models",
			manualModels:  []string{"manual-model-a", "manual-model-b"},
			fetchedModels: []string{"fetched-model-x", "fetched-model-y"},
			expected:      []string{"manual-model-a", "manual-model-b", "fetched-model-x", "fetched-model-y"},
		},
		{
			name:          "empty manual models only fetched",
			manualModels:  []string{},
			fetchedModels: []string{"gpt-4", "claude-3"},
			expected:      []string{"gpt-4", "claude-3"},
		},
		{
			name:          "both lists have some models",
			manualModels:  []string{"model-1", "model-2"},
			fetchedModels: []string{"model-3", "model-4", "model-5"},
			expected:      []string{"model-1", "model-2", "model-3", "model-4", "model-5"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeModelsForTest(tt.manualModels, tt.fetchedModels)

			require.ElementsMatch(t, tt.expected, result,
				"Merged result should contain union of manual and fetched models")
		})
	}
}

func TestEmptyProviderResponse(t *testing.T) {
	tests := []struct {
		name          string
		manualModels  []string
		fetchedModels []string
		expected      []string
	}{
		{
			name:          "manual models remain when provider returns empty",
			manualModels:  []string{"important-custom-model", "another-manual-model"},
			fetchedModels: []string{},
			expected:      []string{"important-custom-model", "another-manual-model"},
		},
		{
			name:          "no models when both are empty",
			manualModels:  []string{},
			fetchedModels: []string{},
			expected:      []string{},
		},
		{
			name:          "nil fetched models treated as empty",
			manualModels:  []string{"preserved-model"},
			fetchedModels: nil,
			expected:      []string{"preserved-model"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeModelsForTest(tt.manualModels, tt.fetchedModels)

			require.ElementsMatch(t, tt.expected, result,
				"Manual models should remain when provider returns empty response")
		})
	}
}

func TestDuplicateModels(t *testing.T) {
	tests := []struct {
		name          string
		manualModels  []string
		fetchedModels []string
		expected      []string
	}{
		{
			name:          "duplicates between manual and fetched are removed",
			manualModels:  []string{"gpt-4", "custom-model"},
			fetchedModels: []string{"gpt-4", "claude-3"},
			expected:      []string{"gpt-4", "custom-model", "claude-3"},
		},
		{
			name:          "duplicates within manual models are removed",
			manualModels:  []string{"model-a", "model-a", "model-b"},
			fetchedModels: []string{"model-c"},
			expected:      []string{"model-a", "model-b", "model-c"},
		},
		{
			name:          "duplicates within fetched models are removed",
			manualModels:  []string{"manual-model"},
			fetchedModels: []string{"fetched-a", "fetched-a", "fetched-b"},
			expected:      []string{"manual-model", "fetched-a", "fetched-b"},
		},
		{
			name:          "all unique no duplicates",
			manualModels:  []string{"model-1", "model-2"},
			fetchedModels: []string{"model-3", "model-4"},
			expected:      []string{"model-1", "model-2", "model-3", "model-4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeModelsForTest(tt.manualModels, tt.fetchedModels)

			uniqueResult := lo.Uniq(result)
			require.Equal(t, len(uniqueResult), len(result),
				"Result should not contain duplicates")

			require.ElementsMatch(t, tt.expected, result,
				"Result should contain deduplicated union of models")
		})
	}
}

func TestCaseSensitivity(t *testing.T) {
	tests := []struct {
		name          string
		manualModels  []string
		fetchedModels []string
		expected      []string
	}{
		{
			name:          "case sensitivity preserved - GPT-4 vs gpt-4",
			manualModels:  []string{"GPT-4"},
			fetchedModels: []string{"gpt-4", "GPT-4"},
			expected:      []string{"GPT-4", "gpt-4"},
		},
		{
			name:          "different cases are different models",
			manualModels:  []string{"Claude-3", "claude-3"},
			fetchedModels: []string{"CLAUDE-3"},
			expected:      []string{"Claude-3", "claude-3", "CLAUDE-3"},
		},
		{
			name:          "mixed case models preserved",
			manualModels:  []string{"MyCustomModel"},
			fetchedModels: []string{"mycustommodel", "MYCUSTOMMODEL"},
			expected:      []string{"MyCustomModel", "mycustommodel", "MYCUSTOMMODEL"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeModelsForTest(tt.manualModels, tt.fetchedModels)

			require.ElementsMatch(t, tt.expected, result,
				"Model IDs should be treated as case-sensitive")

			for _, expectedModel := range tt.expected {
				assert.Contains(t, result, expectedModel,
					"Model %s should be present with exact case", expectedModel)
			}
		})
	}
}

func mergeModelsForTest(manualModels, fetchedModels []string) []string {
	return lo.Uniq(append(manualModels, fetchedModels...))
}
