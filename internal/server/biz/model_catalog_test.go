package biz

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func TestLookupModelMetadataExactCaseFoldSuffixAndConflict(t *testing.T) {
	exactName := "Exact"
	uniqueName := "Unique"
	sharedAName := "Shared A"
	sharedBName := "Shared B"
	catalog := map[string]*objects.ModelMetadataPatch{
		"Provider/Exact":   {Name: &exactName},
		"Vendor/Unique":    {Name: &uniqueName},
		"ProviderA/Shared": {Name: &sharedAName},
		"ProviderB/Shared": {Name: &sharedBName},
	}
	indexes := buildBuiltInModelCatalogIndexes(catalog)

	require.Equal(t, "Exact", *lookupModelMetadataFromCatalog(catalog, indexes, "Provider/Exact").Name)
	require.Equal(t, "Exact", *lookupModelMetadataFromCatalog(catalog, indexes, "provider/exact").Name)
	require.Equal(t, "Unique", *lookupModelMetadataFromCatalog(catalog, indexes, "unique").Name)
	require.Equal(t, "Unique", *lookupModelMetadataFromCatalog(catalog, indexes, "prefix/VENDOR/UNIQUE").Name)
	require.Nil(t, lookupModelMetadataFromCatalog(catalog, indexes, "shared"), "ambiguous suffixes must fall back instead of guessing")
	require.Nil(t, lookupModelMetadataFromCatalog(catalog, indexes, "proxy/shared"), "ambiguous prefixed suffixes must fall back instead of guessing")
}

func TestBuiltInModelCatalogResolvesProviderPrefixes(t *testing.T) {
	kimi := lookupBuiltInModelMetadata("moonshotai/KIMI-K2.5")
	require.NotNil(t, kimi)
	require.NotNil(t, kimi.Developer)
	require.Equal(t, "moonshot", *kimi.Developer)

	glm := lookupBuiltInModelMetadata("Pro/zai-org/GLM-5.1")
	require.NotNil(t, glm)
	require.NotNil(t, glm.Developer)
	require.Equal(t, "zai", *glm.Developer)
	require.NotNil(t, glm.Limit)
	require.NotNil(t, glm.Limit.Context)
	require.Equal(t, 200000, *glm.Limit.Context)
}

func TestResolveChannelModelFacadeUsesPermissiveUnknownDefault(t *testing.T) {
	createdAt := time.Unix(1712345000, 0)
	channelEntity := &ent.Channel{
		Type:      channel.TypeOpenai,
		CreatedAt: createdAt,
	}
	facade := (&ModelService{}).ResolveChannelModelFacade(
		&Channel{Channel: channelEntity},
		ChannelModelEntry{RequestModel: "school-new-model", ActualModel: "school-new-model"},
	)

	require.Equal(t, "school-new-model", facade.ID)
	require.Equal(t, "school-new-model", facade.DisplayName)
	require.Equal(t, "openai", facade.OwnedBy)
	require.Equal(t, createdAt.Unix(), facade.Created)
	require.Equal(t, ModelMetadataSourceDefault, facade.MetadataSource)
	require.NotNil(t, facade.Metadata)
	require.True(t, *facade.Metadata.Vision)
	require.True(t, *facade.Metadata.ToolCall)
	require.True(t, *facade.Metadata.Reasoning.Supported)
	require.True(t, *facade.Metadata.Reasoning.Default)
	require.Equal(t, 1_000_000, *facade.Metadata.Limit.Context)
	require.Nil(t, facade.Metadata.Limit.Output)
	require.Nil(t, facade.Metadata.Cost)
	require.Equal(t, []string{"text", "image"}, *facade.Metadata.Modalities.Input)
	require.Equal(t, []string{"text"}, *facade.Metadata.Modalities.Output)
}

func TestResolveChannelModelFacadePriority(t *testing.T) {
	actualVision := false
	actualContext := 700000
	requestToolCall := false
	requestContext := 400000
	channelEntity := &ent.Channel{
		Type: channel.TypeOpenai,
		Settings: &objects.ChannelSettings{
			ModelMetadataOverrides: map[string]*objects.ModelMetadataPatch{
				"kimi-k2.6": {
					Vision: &actualVision,
					Limit:  &objects.ModelCardLimitPatch{Context: &actualContext},
				},
				"school-kimi": {
					ToolCall: &requestToolCall,
					Limit:    &objects.ModelCardLimitPatch{Context: &requestContext},
				},
			},
		},
	}
	svc := &ModelService{}
	facade := svc.ResolveChannelModelFacade(
		&Channel{Channel: channelEntity},
		ChannelModelEntry{RequestModel: "school-kimi", ActualModel: "kimi-k2.6"},
	)

	require.Equal(t, ModelMetadataSourceOverride, facade.MetadataSource)
	require.NotNil(t, facade.Metadata.Name)
	require.Equal(t, "Kimi K2.6", *facade.Metadata.Name, "actual-model catalog should fill descriptive fields")
	require.Equal(t, "Moonshot", *facade.Metadata.Icon)
	require.False(t, *facade.Metadata.Vision, "actual-model override should beat catalog")
	require.False(t, *facade.Metadata.ToolCall, "request-model override should have highest priority")
	require.Equal(t, 400000, *facade.Metadata.Limit.Context)
	require.NotContains(t, *facade.Metadata.Modalities.Input, "image", "vision override should keep modalities consistent")
	require.Nil(t, facade.Metadata.Cost)

	requestCatalog := svc.ResolveChannelModelFacade(
		&Channel{Channel: &ent.Channel{Type: channel.TypeOpenai}},
		ChannelModelEntry{RequestModel: "gpt-4.1-mini", ActualModel: "kimi-k2.6"},
	)
	require.Equal(t, ModelMetadataSourceCatalog, requestCatalog.MetadataSource)
	require.Equal(t, "GPT-4.1 mini", *requestCatalog.Metadata.Name)
	require.Equal(t, 1047576, *requestCatalog.Metadata.Limit.Context, "request-model catalog should beat actual-model catalog")
}

func TestListEnabledModelsConservativelyAggregatesEligibleChannels(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:model-metadata-aggregate?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	firstVision := false
	firstToolCall := true
	firstContext := 200000
	firstOutput := 50000
	first, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("First shared model channel").
		SetBaseURL("https://first.example/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "first"}).
		SetSupportedModels([]string{"shared-unknown-model"}).
		SetDefaultTestModel("shared-unknown-model").
		SetSettings(&objects.ChannelSettings{ModelMetadataOverrides: map[string]*objects.ModelMetadataPatch{
			"shared-unknown-model": {
				Vision:   &firstVision,
				ToolCall: &firstToolCall,
				Limit:    &objects.ModelCardLimitPatch{Context: &firstContext, Output: &firstOutput},
			},
		}}).
		SetStatus(channel.StatusEnabled).
		SetCreatedAt(time.Unix(100, 0)).
		Save(ctx)
	require.NoError(t, err)

	secondVision := true
	secondToolCall := false
	secondContext := 500000
	second, err := client.Channel.Create().
		SetType(channel.TypeAnthropic).
		SetName("Second shared model channel").
		SetBaseURL("https://second.example/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "second"}).
		SetSupportedModels([]string{"shared-unknown-model"}).
		SetDefaultTestModel("shared-unknown-model").
		SetSettings(&objects.ChannelSettings{ModelMetadataOverrides: map[string]*objects.ModelMetadataPatch{
			"shared-unknown-model": {
				Vision:   &secondVision,
				ToolCall: &secondToolCall,
				Limit:    &objects.ModelCardLimitPatch{Context: &secondContext},
			},
		}}).
		SetStatus(channel.StatusEnabled).
		SetCreatedAt(time.Unix(200, 0)).
		Save(ctx)
	require.NoError(t, err)

	channelSvc := NewChannelServiceForTest(client)
	t.Cleanup(channelSvc.Stop)
	channelSvc.SetEnabledChannelsForTest([]*Channel{{Channel: first}, {Channel: second}})
	systemSvc := NewSystemService(SystemServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})
	modelSvc := NewModelService(ModelServiceParams{ChannelService: channelSvc, SystemService: systemSvc, Ent: client})

	models, err := modelSvc.ListEnabledModels(ctx)
	require.NoError(t, err)
	require.Len(t, models, 1)
	metadata := models[0].Metadata
	require.NotNil(t, metadata)
	require.Equal(t, "shared-unknown-model", models[0].ID)
	require.Equal(t, "multiple", models[0].OwnedBy)
	require.Equal(t, time.Unix(100, 0), models[0].CreatedAt)
	require.Equal(t, ModelMetadataSourceOverride, models[0].MetadataSource)
	require.False(t, *metadata.Vision)
	require.False(t, *metadata.ToolCall)
	require.True(t, *metadata.Reasoning.Supported)
	require.Equal(t, 200000, *metadata.Limit.Context)
	require.Nil(t, metadata.Limit.Output, "unknown output on any eligible channel must remain unknown")
	require.Equal(t, []string{"text"}, *metadata.Modalities.Input)
	require.Nil(t, metadata.Cost)
}

func TestListEnabledModelsKeepsConfiguredModelAtomic(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:model-metadata-configured?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	channelEntity, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Configured model channel").
		SetBaseURL("https://configured.example/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "key"}).
		SetSupportedModels([]string{"atomic-model"}).
		SetDefaultTestModel("atomic-model").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.Model.Create().
		SetDeveloper("owner-developer").
		SetModelID("atomic-model").
		SetName("Owner Atomic Model").
		SetType(model.TypeChat).
		SetGroup("owner").
		SetIcon("Owner").
		SetModelCard(&objects.ModelCard{
			Vision: false,
			Limit:  objects.ModelCardLimit{Context: 8192, Output: 2048},
			Cost:   objects.ModelCardCost{Input: 1, Output: 2},
		}).
		SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{{
			Type: "channel_model",
			ChannelModel: &objects.ChannelModelAssociation{
				ChannelID: channelEntity.ID,
				ModelID:   "atomic-model",
			},
		}}}).
		SetStatus(model.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	channelSvc := NewChannelServiceForTest(client)
	t.Cleanup(channelSvc.Stop)
	channelSvc.SetEnabledChannelsForTest([]*Channel{{Channel: channelEntity}})
	systemSvc := NewSystemService(SystemServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client})
	modelSvc := NewModelService(ModelServiceParams{ChannelService: channelSvc, SystemService: systemSvc, Ent: client})

	models, err := modelSvc.ListEnabledModels(ctx)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, ModelMetadataSourceConfigured, models[0].MetadataSource)
	require.Equal(t, "owner", string(models[0].MetadataSource))
	require.Equal(t, "Owner Atomic Model", *models[0].Metadata.Name)
	require.False(t, *models[0].Metadata.Vision)
	require.Equal(t, 8192, *models[0].Metadata.Limit.Context)
	require.NotNil(t, models[0].Metadata.Cost, "owner-configured pricing must remain available")
	require.Equal(t, 1.0, *models[0].Metadata.Cost.Input)
}
