package biz

import (
	"context"
	"net/http"
	"net/http/httptest"
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
