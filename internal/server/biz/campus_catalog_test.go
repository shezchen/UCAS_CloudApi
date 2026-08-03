package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

func campusTestModelFacade(id, source string, vision, toolCall, reasoning bool, contextLength int, maxOutputTokens *int) ModelFacade {
	metadata := &objects.ModelMetadataPatch{
		Vision:    &vision,
		ToolCall:  &toolCall,
		Reasoning: &objects.ModelCardReasoningPatch{Supported: &reasoning, Default: &reasoning},
		Limit:     &objects.ModelCardLimitPatch{Context: &contextLength},
	}
	if maxOutputTokens != nil {
		output := *maxOutputTokens
		metadata.Limit.Output = &output
	}

	return ModelFacade{
		ID:             id,
		Metadata:       metadata,
		MetadataSource: ModelMetadataSource(source),
	}
}

func TestCampusChannelTestHealthAggregatesOnlyCurrentExactRoutes(t *testing.T) {
	routes := NewUnifiedRouteState()
	svc := &CampusCatalogService{unifiedRoutes: routes}
	ch := &ent.Channel{
		ID:              7,
		Type:            channel.TypeOpenaiResponses,
		Credentials:     objects.ChannelCredentials{APIKeys: []string{"current-key-a", "current-key-b"}},
		SupportedModels: []string{"gpt-5.6-sol"},
		Settings:        &objects.ChannelSettings{},
	}
	apiFormat := DefaultEndpointsForChannelType(ch.Type)[0].APIFormat
	revision := RouteConfigRevision(&Channel{Channel: ch}, apiFormat, nil)
	key := RouteKey{
		ChannelID:      ch.ID,
		CredentialID:   CredentialFingerprint("current-key-a"),
		ActualModel:    "gpt-5.6-sol",
		APIFormat:      apiFormat,
		ConfigRevision: revision,
	}

	health := svc.channelTestHealth(ch)
	require.Equal(t, "unknown", health.State)
	require.False(t, health.Known)
	require.Equal(t, 2, health.RouteCount)
	require.Equal(t, 2, health.UnknownRouteCount)
	require.Len(t, health.Routes, 2)

	require.False(t, routes.RecordTestVerdict(key, RouteTestVerdict{Completed: false, Error: "tester crashed"}))
	health = svc.channelTestHealth(ch)
	require.Equal(t, "unknown", health.State)
	require.Equal(t, 2, health.UnknownRouteCount)

	require.True(t, routes.RecordTestVerdict(key, RouteTestVerdict{Completed: true, Pass: true}))
	health = svc.channelTestHealth(ch)
	require.Equal(t, "unknown", health.State, "an untested credential route must not be hidden behind one green route")
	require.False(t, health.Known)
	require.Equal(t, 1, health.AvailableRouteCount)
	require.Equal(t, 1, health.UnknownRouteCount)

	failedKey := key
	failedKey.CredentialID = CredentialFingerprint("current-key-b")
	require.True(t, routes.RecordTestVerdict(failedKey, RouteTestVerdict{
		Completed: true,
		Pass:      false,
		Error:     "Authorization: Bearer secret-token\nupstream rejected",
	}))

	staleRoutes := []RouteKey{
		{ChannelID: ch.ID, CredentialID: CredentialFingerprint("removed-key"), ActualModel: key.ActualModel, APIFormat: apiFormat, ConfigRevision: revision},
		{ChannelID: ch.ID, CredentialID: key.CredentialID, ActualModel: "retired-model", APIFormat: apiFormat, ConfigRevision: revision},
		{ChannelID: ch.ID, CredentialID: key.CredentialID, ActualModel: key.ActualModel, APIFormat: "retired/protocol", ConfigRevision: revision},
		{ChannelID: ch.ID, CredentialID: key.CredentialID, ActualModel: key.ActualModel, APIFormat: apiFormat, ConfigRevision: "old-config"},
	}
	for _, stale := range staleRoutes {
		require.True(t, routes.RecordTestVerdict(stale, RouteTestVerdict{Completed: true, Pass: true}))
	}

	health = svc.channelTestHealth(ch)
	require.True(t, health.Known)
	require.False(t, health.Available)
	require.Equal(t, "mixed", health.State)
	require.Equal(t, 2, health.RouteCount)
	require.Equal(t, 2, health.KnownRouteCount)
	require.Equal(t, 1, health.AvailableRouteCount)
	require.Equal(t, 1, health.UnavailableRouteCount)
	require.Zero(t, health.UnknownRouteCount)
	require.Len(t, health.Routes, 2)
	for _, route := range health.Routes {
		require.Equal(t, key.ActualModel, route.Model)
		require.Equal(t, apiFormat, route.Protocol)
		require.Positive(t, route.CredentialSlot)
		if !route.Available {
			require.NotContains(t, route.LastTestError, "secret-token")
			require.Contains(t, route.LastTestError, "upstream rejected")
		}
	}

	payload, err := json.Marshal(health)
	require.NoError(t, err)
	for _, secret := range []string{
		"current-key-a",
		"current-key-b",
		CredentialFingerprint("current-key-a"),
		CredentialFingerprint("current-key-b"),
		revision,
	} {
		require.NotContains(t, string(payload), secret)
	}
}

func TestCampusChannelTestHealthUsesRealInFlightSnapshot(t *testing.T) {
	routes := NewUnifiedRouteState()
	svc := &CampusCatalogService{unifiedRoutes: routes}
	ch := &ent.Channel{
		ID:              8,
		Type:            channel.TypeOpenaiResponses,
		Credentials:     objects.ChannelCredentials{APIKey: "current-key"},
		SupportedModels: []string{"gpt-5.6-sol"},
		Settings:        &objects.ChannelSettings{},
	}
	apiFormat := DefaultEndpointsForChannelType(ch.Type)[0].APIFormat
	key := RouteKey{
		ChannelID:      ch.ID,
		CredentialID:   CredentialFingerprint("current-key"),
		ActualModel:    "gpt-5.6-sol",
		APIFormat:      apiFormat,
		ConfigRevision: RouteConfigRevision(&Channel{Channel: ch}, apiFormat, nil),
	}
	started := make(chan struct{})
	release := make(chan struct{})
	routes.TriggerTest(context.Background(), key, func(context.Context) (RouteTestVerdict, error) {
		close(started)
		<-release
		return RouteTestVerdict{Completed: true, Pass: true}, nil
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("programmatic TestChannel did not start")
	}
	require.Eventually(t, func() bool {
		health := svc.channelTestHealth(ch)
		return health.TestInFlight && health.TestInFlightRouteCount == 1 &&
			health.State == "unknown" && health.UnknownRouteCount == 1
	}, time.Second, 10*time.Millisecond)

	close(release)
	require.Eventually(t, func() bool {
		health := svc.channelTestHealth(ch)
		return !health.TestInFlight && health.State == "available" &&
			health.AvailableRouteCount == 1
	}, time.Second, 10*time.Millisecond)
}

func TestCurrentCampusRouteCredentialSlotsUseStructuredRouteIdentity(t *testing.T) {
	ch := &ent.Channel{Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{
		ClientID:     "client",
		AccessToken:  "access-a",
		RefreshToken: "account-refresh-a",
	}}}

	before := currentCampusRouteCredentialSlots(ch)
	require.Len(t, before, 1)
	require.Equal(t, 1, before[RouteCredentialFingerprint(&Channel{Channel: ch}, "")])

	ch.Credentials.OAuth.AccessToken = "access-b"
	require.Equal(t, before, currentCampusRouteCredentialSlots(ch), "short-lived OAuth refresh must keep the same public slot")

	ch.Credentials.OAuth.RefreshToken = "account-refresh-b"
	require.NotEqual(t, before, currentCampusRouteCredentialSlots(ch), "account replacement must retire the old public slot")
}

func TestCampusCatalogServiceOwnModelsAndSafeChannels(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:campus_catalog?mode=memory&_fk=1")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	projectRow := client.Project.Create().
		SetName("Campus").
		SetStatus(project.StatusActive).
		SetProfiles(&objects.ProjectProfiles{ActiveProfile: "campus"}).
		SaveX(setupCtx)
	member := client.User.Create().
		SetEmail("member@mails.ucas.ac.cn").
		SetPassword("hash").
		SetNickname("目录同学").
		SaveX(setupCtx)
	other := client.User.Create().
		SetEmail("other@mails.ucas.ac.cn").
		SetPassword("hash").
		SetNickname("Owner").
		SaveX(setupCtx)
	client.UserProject.Create().SetUser(member).SetProject(projectRow).SaveX(setupCtx)
	client.UserProject.Create().SetUser(other).SetProject(projectRow).SaveX(setupCtx)

	createKey := func(owner *ent.User, name, raw string, status apikey.Status, keyType apikey.Type, keyScopes []string, deletedAt int) {
		client.APIKey.Create().
			SetUser(owner).
			SetProject(projectRow).
			SetName(name).
			SetKey(raw).
			SetStatus(status).
			SetType(keyType).
			SetScopes(keyScopes).
			SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "default"}).
			SetDeletedAt(deletedAt).
			SaveX(setupCtx)
	}

	callScopes := []string{string(scopes.ScopeWriteRequests)}
	createKey(member, "Alpha", "sk-secret-alpha", apikey.StatusEnabled, apikey.TypeUser, callScopes, 0)
	createKey(member, "Beta", "sk-secret-beta", apikey.StatusEnabled, apikey.TypePersonal, callScopes, 0)
	createKey(member, "Disabled", "sk-secret-disabled", apikey.StatusDisabled, apikey.TypeUser, callScopes, 0)
	createKey(member, "NoAuth", "sk-secret-noauth", apikey.StatusEnabled, apikey.TypeNoauth, callScopes, 0)
	createKey(member, "ReadOnly", "sk-secret-readonly", apikey.StatusEnabled, apikey.TypeUser, []string{string(scopes.ScopeReadAPIKeys)}, 0)
	createKey(member, "Deleted", "sk-secret-deleted", apikey.StatusEnabled, apikey.TypeUser, callScopes, 123)
	createKey(other, "Other User", "sk-secret-other", apikey.StatusEnabled, apikey.TypeUser, callScopes, 0)

	now := time.Now()
	client.Channel.Create().
		SetType(channel.TypeCodex).
		SetName("项目公共渠道").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-owner"}).
		SetBaseURL("https://private-owner.invalid/v1").
		SetSupportedModels([]string{"gpt-5", "gpt-5", ""}).
		SetDefaultTestModel("gpt-5").
		SetRemark("owner internal note must not be public").
		SaveX(setupCtx)
	client.Channel.Create().
		SetType(channel.TypeSiliconflow).
		SetName("同学共享").
		SetStatus(channel.StatusEnabled).
		SetUser(member).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-donor"}).
		SetBaseURL("https://private-donor.invalid/v1").
		SetSupportedModels([]string{"kimi-k2.5", "kimi-k2.6"}).
		SetDefaultTestModel("kimi-k2.5").
		SetRemark("  公益\u202e\n共享说明  ").
		SetExpiresAt(now.Add(24 * time.Hour)).
		SaveX(setupCtx)
	client.Channel.Create().
		SetType(channel.TypeMoonshotCoding).
		SetName("暂时停用的共享").
		SetStatus(channel.StatusDisabled).
		SetUser(other).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-disabled"}).
		SetSupportedModels([]string{"kimi-for-coding"}).
		SetDefaultTestModel("kimi-for-coding").
		SaveX(setupCtx)
	client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("已归档").
		SetStatus(channel.StatusArchived).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-archived"}).
		SetSupportedModels([]string{"hidden"}).
		SetDefaultTestModel("hidden").
		SaveX(setupCtx)
	client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("已到期").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-expired"}).
		SetSupportedModels([]string{"hidden"}).
		SetDefaultTestModel("hidden").
		SetExpiresAt(now.Add(-time.Hour)).
		SaveX(setupCtx)
	client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("已删除").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-deleted"}).
		SetSupportedModels([]string{"hidden"}).
		SetDefaultTestModel("hidden").
		SetDeletedAt(456).
		SaveX(setupCtx)

	seenKeys := []string{}
	kimiOutput := 131072
	gptOutput := 128000
	svc := &CampusCatalogService{
		client: client,
		listEnabledModels: func(ctx context.Context) ([]ModelFacade, error) {
			key, ok := contexts.GetAPIKey(ctx)
			require.True(t, ok)
			require.Empty(t, key.Key, "the raw API key field must never be loaded")
			require.NotNil(t, key.Edges.Project)
			require.Equal(t, "campus", key.Edges.Project.Profiles.ActiveProfile)
			seenKeys = append(seenKeys, key.Name)
			switch key.Name {
			case "Alpha":
				return []ModelFacade{
					campusTestModelFacade("kimi-k2.6", "catalog", true, true, true, 262144, &kimiOutput),
					campusTestModelFacade("gpt-5", "default", true, true, true, 1_000_000, &gptOutput),
					campusTestModelFacade("gpt-5", "default", true, true, true, 1_000_000, &gptOutput),
				}, nil
			case "Beta":
				return []ModelFacade{
					campusTestModelFacade("kimi-k2.5", "catalog", true, true, true, 262144, &kimiOutput),
					campusTestModelFacade("gpt-5", "override", false, true, true, 128000, nil),
					{ID: " "},
				}, nil
			default:
				t.Fatalf("unexpected key passed to model lister: %s", key.Name)
				return nil, nil
			}
		},
	}

	requestCtx := authz.NewUserContext(context.Background(), member.ID)
	requestCtx = contexts.WithUser(requestCtx, member)
	requestCtx = contexts.WithProjectID(requestCtx, projectRow.ID)
	resources, err := svc.GetResources(requestCtx)
	require.NoError(t, err)
	require.Equal(t, []string{"Alpha", "Beta"}, seenKeys)
	require.Equal(t, []string{"gpt-5", "kimi-k2.5", "kimi-k2.6"}, resources.Models)
	require.Len(t, resources.APIKeys, 2)
	require.Equal(t, "Alpha", resources.APIKeys[0].Name)
	require.Equal(t, []string{"gpt-5", "kimi-k2.6"}, resources.APIKeys[0].Models)
	require.Equal(t, "Beta", resources.APIKeys[1].Name)
	require.Equal(t, []string{"gpt-5", "kimi-k2.5"}, resources.APIKeys[1].Models)
	require.Len(t, resources.ModelDetails, 3)
	detailsByID := make(map[string]CampusModelDetail, len(resources.ModelDetails))
	for _, detail := range resources.ModelDetails {
		detailsByID[detail.ID] = detail
	}
	gptDetail := detailsByID["gpt-5"]
	require.Equal(t, "mixed", gptDetail.Source)
	require.False(t, gptDetail.Vision)
	require.True(t, gptDetail.ToolCall)
	require.True(t, gptDetail.Reasoning)
	require.Equal(t, 128000, gptDetail.ContextLength)
	require.Nil(t, gptDetail.MaxOutputTokens, "an unknown per-key output limit must remain unknown")
	require.True(t, gptDetail.VariesByAPIKey)
	require.True(t, detailsByID["kimi-k2.5"].VariesByAPIKey, "a model missing from one key varies by key")
	require.True(t, detailsByID["kimi-k2.6"].VariesByAPIKey, "a model missing from one key varies by key")

	require.Len(t, resources.Channels, 3)
	byName := make(map[string]CampusChannelResource, len(resources.Channels))
	for _, resource := range resources.Channels {
		byName[resource.Name] = resource
	}

	projectChannel := byName["项目公共渠道"]
	require.Equal(t, "project", projectChannel.Source)
	require.Equal(t, "项目维护者", projectChannel.Contributor)
	require.Empty(t, projectChannel.Description)
	require.Equal(t, 1, projectChannel.ModelCount)
	require.Equal(t, []string{"gpt-5"}, projectChannel.Models)
	require.Zero(t, projectChannel.EffectiveTokens)

	donatedChannel := byName["同学共享"]
	require.Equal(t, "donated", donatedChannel.Source)
	require.Equal(t, "目录同学", donatedChannel.Contributor)
	require.Equal(t, "公益 共享说明", donatedChannel.Description)
	require.Equal(t, 2, donatedChannel.ModelCount)
	require.Equal(t, []string{"kimi-k2.5", "kimi-k2.6"}, donatedChannel.Models)
	require.Zero(t, donatedChannel.EffectiveTokens)

	disabledChannel := byName["暂时停用的共享"]
	require.Equal(t, "disabled", disabledChannel.Status)
	require.Equal(t, CampusPublicAlias(projectRow.ID, other.ID), disabledChannel.Contributor)

	payload, err := json.Marshal(resources)
	require.NoError(t, err)
	payloadText := string(payload)
	for _, forbidden := range []string{
		"sk-secret", "provider-secret", "private-owner.invalid", "private-donor.invalid",
		"base_url", "baseURL", "credentials", "settings", "policies", "endpoints",
		"error_message", "errorMessage", "email", "user_id", "userId", "channelId",
	} {
		require.NotContains(t, payloadText, forbidden)
	}
}

func TestCampusCatalogPublicChannelsExposeRequestModelsAndProjectEffectiveTokens(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:campus_catalog_channel_transparency?mode=memory&_fk=0")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	campusProject := client.Project.Create().
		SetName("Campus").
		SetStatus(project.StatusActive).
		SaveX(setupCtx)
	otherProject := client.Project.Create().
		SetName("Other").
		SetStatus(project.StatusActive).
		SaveX(setupCtx)

	mappedChannel := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("映射渠道").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "mapped-secret"}).
		SetSupportedModels([]string{"upstream-model", "other-model", ""}).
		SetSettings(&objects.ChannelSettings{
			ModelMappings: []objects.ModelMapping{
				{From: "z-model", To: "upstream-model"},
				{From: "a-model", To: "other-model"},
			},
			HideOriginalModels: true,
		}).
		SetDefaultTestModel("upstream-model").
		SaveX(setupCtx)
	directChannel := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("直连渠道").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "direct-secret"}).
		SetSupportedModels([]string{"beta", "alpha", "beta", " "}).
		SetDefaultTestModel("alpha").
		SaveX(setupCtx)

	createUsage := func(
		requestID, projectID, channelID int,
		source usagelog.Source,
		promptTokens, cachedTokens, completionTokens, totalTokens, effectiveTokens int64,
		cacheReadKnown bool,
	) {
		t.Helper()
		create := client.UsageLog.Create().
			SetRequestID(requestID).
			SetProjectID(projectID).
			SetChannelID(channelID).
			SetModelID("public-model").
			SetSource(source).
			SetFormat("openai/chat_completions").
			SetPromptTokens(promptTokens).
			SetPromptCachedTokens(cachedTokens).
			SetCompletionTokens(completionTokens).
			SetTotalTokens(totalTokens).
			SetEffectiveTokens(effectiveTokens)
		if cacheReadKnown {
			create.SetCacheReadTokensKnown(true)
		}
		create.SaveX(setupCtx)
	}

	// The current row contributes its stored cache-excluding value.
	createUsage(1, campusProject.ID, mappedChannel.ID, usagelog.SourceAPI, 100, 40, 20, 120, 80, true)
	// A pre-migration total-only row uses the shared conservative fallback.
	createUsage(2, campusProject.ID, mappedChannel.ID, usagelog.SourcePlayground, 0, 0, 0, 50, 0, false)
	// Manual channel tests never contribute to the public total.
	createUsage(3, campusProject.ID, mappedChannel.ID, usagelog.SourceTest, 1_000, 0, 0, 1_000, 1_000, true)
	// Usage from another project is not disclosed in this project's catalog.
	createUsage(4, otherProject.ID, mappedChannel.ID, usagelog.SourceAPI, 500, 0, 0, 500, 500, true)
	createUsage(5, campusProject.ID, directChannel.ID, usagelog.SourceAPI, 10, 0, 5, 15, 15, true)

	resources, err := (&CampusCatalogService{client: client}).listPublicChannels(
		setupCtx,
		campusProject.ID,
		time.Now(),
	)
	require.NoError(t, err)
	require.Len(t, resources, 2)

	byName := make(map[string]CampusChannelResource, len(resources))
	for _, resource := range resources {
		byName[resource.Name] = resource
	}

	require.Equal(t, []string{"a-model", "z-model"}, byName["映射渠道"].Models)
	require.Equal(t, 2, byName["映射渠道"].ModelCount)
	require.Equal(t, int64(130), byName["映射渠道"].EffectiveTokens)
	require.Equal(t, []string{"alpha", "beta"}, byName["直连渠道"].Models)
	require.Equal(t, 2, byName["直连渠道"].ModelCount)
	require.Equal(t, int64(15), byName["直连渠道"].EffectiveTokens)
}

func TestCampusCatalogServiceAuthorizationAndOwnerIsolation(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:campus_catalog_auth?mode=memory&_fk=1")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	projectRow := client.Project.Create().SetName("Campus").SetStatus(project.StatusActive).SaveX(setupCtx)
	owner := client.User.Create().SetEmail("owner@example.com").SetPassword("hash").SetIsOwner(true).SaveX(setupCtx)
	member := client.User.Create().SetEmail("member@mails.ucas.ac.cn").SetPassword("hash").SaveX(setupCtx)
	outsider := client.User.Create().SetEmail("outsider@mails.ucas.ac.cn").SetPassword("hash").SaveX(setupCtx)
	client.UserProject.Create().SetUser(member).SetProject(projectRow).SaveX(setupCtx)
	for _, item := range []struct {
		owner *ent.User
		name  string
		key   string
	}{
		{owner: owner, name: "Owner Key", key: "owner-secret"},
		{owner: member, name: "Member Key", key: "member-secret"},
	} {
		client.APIKey.Create().
			SetUser(item.owner).
			SetProject(projectRow).
			SetName(item.name).
			SetKey(item.key).
			SetScopes([]string{string(scopes.ScopeWriteRequests)}).
			SaveX(setupCtx)
	}

	seen := []string{}
	svc := &CampusCatalogService{
		client: client,
		listEnabledModels: func(ctx context.Context) ([]ModelFacade, error) {
			key, _ := contexts.GetAPIKey(ctx)
			seen = append(seen, key.Name)
			return []ModelFacade{{ID: key.Name}}, nil
		},
	}

	_, err := svc.GetResources(context.Background())
	require.ErrorIs(t, err, ErrCampusCatalogUnauthorized)

	missingProjectCtx := authz.NewUserContext(context.Background(), member.ID)
	missingProjectCtx = contexts.WithUser(missingProjectCtx, member)
	_, err = svc.GetResources(missingProjectCtx)
	require.ErrorIs(t, err, ErrCampusCatalogProjectRequired)

	outsiderCtx := authz.NewUserContext(context.Background(), outsider.ID)
	outsiderCtx = contexts.WithUser(outsiderCtx, outsider)
	outsiderCtx = contexts.WithProjectID(outsiderCtx, projectRow.ID)
	_, err = svc.GetResources(outsiderCtx)
	require.ErrorIs(t, err, ErrCampusCatalogForbidden)

	ownerCtx := authz.NewUserContext(context.Background(), owner.ID)
	ownerCtx = contexts.WithUser(ownerCtx, owner)
	ownerCtx = contexts.WithProjectID(ownerCtx, projectRow.ID)
	resources, err := svc.GetResources(ownerCtx)
	require.NoError(t, err)
	require.Equal(t, []string{"Owner Key"}, seen, "Owner policy must not mix in other users' API keys")
	require.Equal(t, []string{"Owner Key"}, resources.Models)
	require.Len(t, resources.APIKeys, 1)
	require.Equal(t, "Owner Key", resources.APIKeys[0].Name)
	require.Equal(t, []string{"Owner Key"}, resources.APIKeys[0].Models)
	require.Len(t, resources.APIKeys[0].ModelDetails, 1)
}

func TestCampusChannelModelCapabilitiesOwnershipAndNarrowSettingsUpdate(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:campus_channel_model_capabilities?mode=memory&_fk=1")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	projectRow := client.Project.Create().SetName("Campus").SetStatus(project.StatusActive).SaveX(setupCtx)
	member := client.User.Create().SetEmail("member@mails.ucas.ac.cn").SetPassword("hash").SaveX(setupCtx)
	other := client.User.Create().SetEmail("other@mails.ucas.ac.cn").SetPassword("hash").SaveX(setupCtx)
	owner := client.User.Create().SetEmail("owner@example.com").SetPassword("hash").SetIsOwner(true).SaveX(setupCtx)
	client.UserProject.Create().SetUser(member).SetProject(projectRow).SaveX(setupCtx)
	client.UserProject.Create().SetUser(other).SetProject(projectRow).SaveX(setupCtx)

	passThrough := true
	staleVision := false
	staleOverride := &objects.ModelMetadataPatch{Vision: &staleVision}
	ownSettings := &objects.ChannelSettings{
		PassThroughBody:      &passThrough,
		RetryableStatusCodes: []int{408},
		ModelMetadataOverrides: map[string]*objects.ModelMetadataPatch{
			"previous-model": staleOverride,
		},
	}
	ownChannel := client.Channel.Create().
		SetType(channel.TypeMoonshotCoding).
		SetName("Own enabled donation").
		SetStatus(channel.StatusEnabled).
		SetUser(member).
		SetBaseURL("https://private-own.invalid/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-own"}).
		SetSupportedModels([]string{"kimi-k3"}).
		SetDefaultTestModel("kimi-k3").
		SetSettings(ownSettings).
		SetExpiresAt(time.Now().Add(24 * time.Hour)).
		SaveX(setupCtx)
	disabledChannel := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Own disabled donation").
		SetStatus(channel.StatusDisabled).
		SetUser(member).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-disabled"}).
		SetSupportedModels([]string{"disabled-model"}).
		SetDefaultTestModel("disabled-model").
		SetExpiresAt(time.Now().Add(24 * time.Hour)).
		SaveX(setupCtx)
	otherChannel := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Other donation").
		SetStatus(channel.StatusEnabled).
		SetUser(other).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-other"}).
		SetSupportedModels([]string{"other-model"}).
		SetDefaultTestModel("other-model").
		SetExpiresAt(time.Now().Add(24 * time.Hour)).
		SaveX(setupCtx)
	expiredChannel := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Expired own donation").
		SetStatus(channel.StatusEnabled).
		SetUser(member).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-expired"}).
		SetSupportedModels([]string{"expired-model"}).
		SetDefaultTestModel("expired-model").
		SetExpiresAt(time.Now().Add(-time.Hour)).
		SaveX(setupCtx)
	client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Archived own donation").
		SetStatus(channel.StatusArchived).
		SetUser(member).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret-archived"}).
		SetSupportedModels([]string{"archived-model"}).
		SetDefaultTestModel("archived-model").
		SaveX(setupCtx)

	channelSvc := NewChannelServiceForTest(client)
	defer channelSvc.Stop()
	svc := &CampusCatalogService{
		client: client,
		resolveChannelModelFacade: func(ch *Channel, entry ChannelModelEntry) ModelFacade {
			if ch.Settings != nil {
				if override := ch.Settings.ModelMetadataOverrides[entry.RequestModel]; override != nil {
					return ModelFacade{ID: entry.RequestModel, Metadata: override, MetadataSource: ModelMetadataSource("override")}
				}
			}
			return campusTestModelFacade(entry.RequestModel, "default", true, true, true, 1_000_000, nil)
		},
		updateChannelModelMetadataOverride: channelSvc.UpdateChannelModelMetadataOverride,
	}

	memberCtx := authz.NewUserContext(context.Background(), member.ID)
	memberCtx = contexts.WithUser(memberCtx, member)
	memberCtx = contexts.WithProjectID(memberCtx, projectRow.ID)
	capabilities, err := svc.GetChannelModelCapabilities(memberCtx)
	require.NoError(t, err)
	require.Len(t, capabilities.Channels, 2, "enabled and disabled own donations remain editable")
	require.Equal(t, fmt.Sprintf("gid://axonhub/Channel/%d", disabledChannel.ID), capabilities.Channels[0].ID)
	require.Equal(t, fmt.Sprintf("gid://axonhub/Channel/%d", ownChannel.ID), capabilities.Channels[1].ID)
	payload, err := json.Marshal(capabilities)
	require.NoError(t, err)
	for _, forbidden := range []string{"provider-secret", "private-own.invalid", "credentials", "baseURL", "settings", "userId", "email", "Other donation", "Expired own donation"} {
		require.NotContains(t, string(payload), forbidden)
	}

	maxOutput := 128000
	err = svc.UpdateChannelModelCapabilities(memberCtx, UpdateCampusChannelModelCapabilitiesInput{
		ChannelID: fmt.Sprintf("gid://axonhub/Channel/%d", ownChannel.ID),
		ModelID:   "kimi-k3",
		Override: &CampusModelCapabilityOverride{
			Vision: true, ToolCall: false, Reasoning: true, ContextLength: 1_000_000, MaxOutputTokens: &maxOutput,
		},
	})
	require.NoError(t, err)
	updated := client.Channel.GetX(setupCtx, ownChannel.ID)
	require.Equal(t, "https://private-own.invalid/v1", updated.BaseURL)
	require.Equal(t, "provider-secret-own", updated.Credentials.APIKey)
	require.NotNil(t, updated.Settings.PassThroughBody)
	require.True(t, *updated.Settings.PassThroughBody)
	require.Equal(t, []int{408}, updated.Settings.RetryableStatusCodes)
	require.Same(t, staleOverride, ownSettings.ModelMetadataOverrides["previous-model"], "the caller's original settings object must not be mutated")
	require.NotNil(t, updated.Settings.ModelMetadataOverrides["previous-model"])
	savedOverride := updated.Settings.ModelMetadataOverrides["kimi-k3"]
	require.NotNil(t, savedOverride)
	require.True(t, *savedOverride.Vision)
	require.False(t, *savedOverride.ToolCall)
	require.Equal(t, 1_000_000, *savedOverride.Limit.Context)
	require.Equal(t, 128000, *savedOverride.Limit.Output)

	err = svc.UpdateChannelModelCapabilities(memberCtx, UpdateCampusChannelModelCapabilitiesInput{
		ChannelID: fmt.Sprintf("gid://axonhub/Channel/%d", ownChannel.ID), ModelID: "kimi-k3", Override: nil,
	})
	require.NoError(t, err)
	updated = client.Channel.GetX(setupCtx, ownChannel.ID)
	require.NotContains(t, updated.Settings.ModelMetadataOverrides, "kimi-k3")
	require.Contains(t, updated.Settings.ModelMetadataOverrides, "previous-model")

	atomicWriter := svc.updateChannelModelMetadataOverride
	svc.updateChannelModelMetadataOverride = func(context.Context, int, int, string, *objects.ModelMetadataPatch) (*ent.Channel, error) {
		return nil, ErrChannelModelMetadataTargetUnavailable
	}
	err = svc.UpdateChannelModelCapabilities(memberCtx, UpdateCampusChannelModelCapabilitiesInput{
		ChannelID: fmt.Sprintf("gid://axonhub/Channel/%d", ownChannel.ID), ModelID: "kimi-k3", Override: nil,
	})
	require.ErrorIs(t, err, ErrCampusChannelNotFound, "a target invalidated after the outer ownership query must remain a privacy-safe 404")
	svc.updateChannelModelMetadataOverride = atomicWriter

	for _, denied := range []UpdateCampusChannelModelCapabilitiesInput{
		{ChannelID: fmt.Sprintf("gid://axonhub/Channel/%d", otherChannel.ID), ModelID: "other-model"},
		{ChannelID: fmt.Sprintf("gid://axonhub/Channel/%d", expiredChannel.ID), ModelID: "expired-model"},
		{ChannelID: fmt.Sprintf("gid://axonhub/Channel/%d", ownChannel.ID), ModelID: "not-on-channel"},
		{ChannelID: "gid://axonhub/Channel/999999", ModelID: "missing"},
	} {
		err = svc.UpdateChannelModelCapabilities(memberCtx, denied)
		require.ErrorIs(t, err, ErrCampusChannelNotFound)
	}
	require.ErrorIs(t, svc.UpdateChannelModelCapabilities(memberCtx, UpdateCampusChannelModelCapabilitiesInput{
		ChannelID: fmt.Sprintf("gid://axonhub/User/%d", member.ID), ModelID: "kimi-k3",
	}), ErrCampusCatalogInvalidInput)

	ownerCtx := authz.NewUserContext(context.Background(), owner.ID)
	ownerCtx = contexts.WithUser(ownerCtx, owner)
	ownerCtx = contexts.WithProjectID(ownerCtx, projectRow.ID)
	ownerCapabilities, err := svc.GetChannelModelCapabilities(ownerCtx)
	require.NoError(t, err)
	require.Empty(t, ownerCapabilities.Channels)
	require.ErrorIs(t, svc.UpdateChannelModelCapabilities(ownerCtx, UpdateCampusChannelModelCapabilitiesInput{
		ChannelID: fmt.Sprintf("gid://axonhub/Channel/%d", ownChannel.ID), ModelID: "kimi-k3",
	}), ErrCampusOwnerOverrideForbidden)
}

func TestChannelModelMetadataOverrideRevalidatesTargetAndPreservesSettings(t *testing.T) {
	channelSvc, client := setupTestChannelService(t)
	defer channelSvc.Stop()
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	donor := client.User.Create().SetEmail("donor@mails.ucas.ac.cn").SetPassword("hash").SaveX(ctx)
	other := client.User.Create().SetEmail("other-donor@mails.ucas.ac.cn").SetPassword("hash").SaveX(ctx)
	expiresAt := time.Now().Add(time.Hour)
	row := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("guarded donation").
		SetStatus(channel.StatusEnabled).
		SetUser(donor).
		SetBaseURL("https://example.invalid/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"known-model"}).
		SetDefaultTestModel("known-model").
		SetExpiresAt(expiresAt).
		SaveX(ctx)

	vision := false
	patch := &objects.ModelMetadataPatch{Vision: &vision}
	_, err := channelSvc.UpdateChannelModelMetadataOverride(ctx, row.ID, other.ID, "known-model", patch)
	require.ErrorIs(t, err, ErrChannelModelMetadataTargetUnavailable)

	client.Channel.UpdateOneID(row.ID).SetStatus(channel.StatusArchived).SaveX(ctx)
	_, err = channelSvc.UpdateChannelModelMetadataOverride(ctx, row.ID, donor.ID, "known-model", patch)
	require.ErrorIs(t, err, ErrChannelModelMetadataTargetUnavailable)

	client.Channel.UpdateOneID(row.ID).SetStatus(channel.StatusEnabled).SetExpiresAt(time.Now().Add(-time.Minute)).SaveX(ctx)
	_, err = channelSvc.UpdateChannelModelMetadataOverride(ctx, row.ID, donor.ID, "known-model", patch)
	require.ErrorIs(t, err, ErrChannelModelMetadataTargetUnavailable)

	client.Channel.UpdateOneID(row.ID).SetExpiresAt(expiresAt).SaveX(ctx)
	_, err = channelSvc.UpdateChannelModelMetadataOverride(ctx, row.ID, donor.ID, "missing-model", patch)
	require.ErrorIs(t, err, ErrChannelModelMetadataTargetUnavailable)
	require.Empty(t, client.Channel.GetX(ctx, row.ID).Settings.ModelMetadataOverrides)

	_, err = channelSvc.UpdateChannelModelMetadataOverride(ctx, row.ID, donor.ID, "known-model", patch)
	require.NoError(t, err)
	require.NotNil(t, client.Channel.GetX(ctx, row.ID).Settings.ModelMetadataOverrides["known-model"])

	passThrough := true
	updated, err := channelSvc.UpdateChannel(ctx, row.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{PassThroughBody: &passThrough},
	})
	require.NoError(t, err)
	require.True(t, *updated.Settings.PassThroughBody)
	require.NotNil(t, updated.Settings.ModelMetadataOverrides["known-model"], "generic settings edits must preserve the hidden contributor override")

	_, err = channelSvc.UpdateChannelModelMetadataOverride(ctx, row.ID, donor.ID, "known-model", nil)
	require.NoError(t, err)
	require.Empty(t, client.Channel.GetX(ctx, row.ID).Settings.ModelMetadataOverrides)
}

func TestSanitizeCampusChannelDescriptionCapsAndRemovesFormatting(t *testing.T) {
	input := strings.Repeat("界", campusChannelDescriptionMaxRunes+5) + "\u200b"
	got := sanitizeCampusChannelDescription(&input)
	require.Len(t, []rune(got), campusChannelDescriptionMaxRunes)
	require.NotContains(t, got, "\u200b")
}

func TestSanitizeCampusDiagnosticErrorRedactsNamedSecrets(t *testing.T) {
	got := SanitizeCampusDiagnosticError(`{
		"access_token":"access-value",
		"refresh_token":"refresh-value",
		"client_secret":"client-value",
		"password":"pass-value",
		"x-api-key":"x-value",
		"headers":{"Authorization":"Bearer auth-value"}
	}`)

	for _, secret := range []string{
		"access-value",
		"refresh-value",
		"client-value",
		"pass-value",
		"x-value",
		"auth-value",
	} {
		require.NotContains(t, got, secret)
	}
	require.Contains(t, got, "[REDACTED]")
}

func TestSanitizeCampusDiagnosticErrorRedactsHeadersAndQueriesWithoutHidingDiagnosis(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		secrets     []string
		diagnostics []string
	}{
		{
			name: "basic authorization header",
			input: "request failed; Authorization: Basic YWxpY2U6c2VjcmV0; " +
				"reason: upstream rejected the account",
			secrets:     []string{"YWxpY2U6c2VjcmV0"},
			diagnostics: []string{"reason:", "upstream rejected the account"},
		},
		{
			name: "digest proxy authorization header",
			input: `proxy failed; Proxy-Authorization: Digest username="alice", realm="private", response="digest-secret"; ` +
				`status: proxy authentication required`,
			secrets:     []string{"alice", "private", "digest-secret"},
			diagnostics: []string{"status:", "proxy authentication required"},
		},
		{
			name: "one-line cookie text fails closed even with diagnostic-looking cookie names",
			input: "request failed; Cookie: session=session-secret; cf_clearance=clearance-secret; " +
				"reason=reason-cookie-secret; status=status-cookie-secret; provider rejected the model",
			secrets: []string{"session-secret", "clearance-secret", "reason-cookie-secret", "status-cookie-secret"},
		},
		{
			name:    "ambiguous cookie text fails closed",
			input:   "request failed; Set-Cookie: session=opaque-secret; Path=/; Secure; unrelated trailing prose",
			secrets: []string{"opaque-secret"},
		},
		{
			name: "structured headers",
			input: `{"headers":{"Cookie":"session=json-cookie-secret; Path=/","Set-Cookie":"auth=json-set-cookie-secret; HttpOnly",` +
				`"Authorization":"Basic json-basic-secret"},"message":"provider rejected the model"}`,
			secrets:     []string{"json-cookie-secret", "json-set-cookie-secret", "json-basic-secret"},
			diagnostics: []string{"provider rejected the model"},
		},
		{
			name: "header lines preserve following diagnosis",
			input: "Authorization: Bearer bearer-secret\nCookie: session=line-cookie-secret; Path=/\n" +
				"upstream TLS handshake failed",
			secrets:     []string{"bearer-secret", "line-cookie-secret"},
			diagnostics: []string{"upstream TLS handshake failed"},
		},
		{
			name: "absolute and relative URL queries",
			input: "GET https://api.example.com/v1/models?signature=absolute-secret&trace=1 failed; " +
				"POST /v1/chat/completions?sig=relative-secret&attempt=2 failed; " +
				"retry v1/responses?code=bare-relative-secret&attempt=3 status=502",
			secrets: []string{"absolute-secret", "relative-secret", "bare-relative-secret"},
			diagnostics: []string{
				"https://api.example.com/v1/models?[REDACTED]",
				"/v1/chat/completions?[REDACTED]",
				"v1/responses?[REDACTED]",
				"status=502",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeCampusDiagnosticError(tt.input)
			for _, secret := range tt.secrets {
				require.NotContains(t, got, secret)
			}
			for _, diagnostic := range tt.diagnostics {
				require.Contains(t, got, diagnostic)
			}
			require.Contains(t, got, "[REDACTED]")
		})
	}
}

func TestCampusFailureCategoryDistinguishesSharedUpstreamQuota(t *testing.T) {
	statusCode := http.StatusPaymentRequired

	require.Equal(t, "upstream_quota", campusFailureCategory(&statusCode, "You have exceeded your monthly quota"))
}

func TestCampusAPIActivityUsesRequestFinalStatusAndIsolatesUsers(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:campus_activity_final_status?mode=memory&_fk=1")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	projectRow := client.Project.Create().
		SetName("Campus").
		SetStatus(project.StatusActive).
		SaveX(setupCtx)
	member := client.User.Create().
		SetEmail("member@mails.ucas.ac.cn").
		SetPassword("hash").
		SaveX(setupCtx)
	other := client.User.Create().
		SetEmail("other@mails.ucas.ac.cn").
		SetPassword("hash").
		SaveX(setupCtx)
	client.UserProject.Create().SetUser(member).SetProject(projectRow).SaveX(setupCtx)
	client.UserProject.Create().SetUser(other).SetProject(projectRow).SaveX(setupCtx)

	memberKey := client.APIKey.Create().
		SetUser(member).
		SetProject(projectRow).
		SetName("Member key").
		SetKey("sk-member-activity-key").
		SaveX(setupCtx)
	otherKey := client.APIKey.Create().
		SetUser(other).
		SetProject(projectRow).
		SetName("Other key").
		SetKey("sk-other-activity-key").
		SaveX(setupCtx)
	firstChannel := client.Channel.Create().
		SetType(channel.TypeOpenaiResponses).
		SetName("First route").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-a"}).
		SetSupportedModels([]string{"gpt-5.6-sol"}).
		SetDefaultTestModel("gpt-5.6-sol").
		SaveX(setupCtx)
	finalChannel := client.Channel.Create().
		SetType(channel.TypeOpenaiResponses).
		SetName("Final route").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-b"}).
		SetSupportedModels([]string{"gpt-5.6-sol"}).
		SetDefaultTestModel("gpt-5.6-sol").
		SaveX(setupCtx)

	baseTime := time.Now().Add(-time.Minute)
	createRequest := func(key *ent.APIKey, status request.Status, model string, createdAt time.Time) *ent.Request {
		t.Helper()
		return client.Request.Create().
			SetAPIKey(key).
			SetProject(projectRow).
			SetSource(request.SourceAPI).
			SetModelID(model).
			SetRequestBody(objects.JSONRawMessage(`{}`)).
			SetStatus(status).
			SetCreatedAt(createdAt).
			SetUpdatedAt(createdAt).
			SaveX(setupCtx)
	}
	createExecution := func(
		req *ent.Request,
		ch *ent.Channel,
		status requestexecution.Status,
		model string,
		statusCode *int,
		errorMessage string,
		createdAt time.Time,
	) {
		t.Helper()
		create := client.RequestExecution.Create().
			SetProjectID(projectRow.ID).
			SetRequest(req).
			SetChannel(ch).
			SetModelID(model).
			SetFormat("openai/responses").
			SetRequestBody(objects.JSONRawMessage(`{}`)).
			SetStatus(status).
			SetCreatedAt(createdAt).
			SetUpdatedAt(createdAt)
		if statusCode != nil {
			create.SetResponseStatusCode(*statusCode)
		}
		if errorMessage != "" {
			create.SetErrorMessage(errorMessage)
		}
		create.SaveX(setupCtx)
	}

	badGateway := http.StatusBadGateway
	ok := http.StatusOK
	recoveredRequest := createRequest(memberKey, request.StatusCompleted, "public-model", baseTime.Add(10*time.Second))
	createExecution(
		recoveredRequest,
		firstChannel,
		requestexecution.StatusFailed,
		"upstream-model-a",
		&badGateway,
		"Authorization: Bearer secret-value\nupstream failed",
		baseTime.Add(11*time.Second),
	)
	createExecution(
		recoveredRequest,
		finalChannel,
		requestexecution.StatusCompleted,
		"upstream-model-b",
		&ok,
		"",
		baseTime.Add(12*time.Second),
	)

	pendingRequest := createRequest(memberKey, request.StatusPending, "pending-public-model", baseTime.Add(20*time.Second))
	createExecution(
		pendingRequest,
		finalChannel,
		requestexecution.StatusCompleted,
		"pending-upstream-model",
		&ok,
		"",
		baseTime.Add(21*time.Second),
	)

	failedAfterCompletedExecution := createRequest(memberKey, request.StatusFailed, "delivery-model", baseTime.Add(30*time.Second))
	createExecution(
		failedAfterCompletedExecution,
		finalChannel,
		requestexecution.StatusCompleted,
		"delivery-upstream-model",
		&ok,
		"",
		baseTime.Add(31*time.Second),
	)

	attemptlessFailedRequest := createRequest(memberKey, request.StatusFailed, "attemptless-model", baseTime.Add(40*time.Second))
	otherRequest := createRequest(otherKey, request.StatusFailed, "other-private-model", baseTime.Add(50*time.Second))
	createExecution(
		otherRequest,
		firstChannel,
		requestexecution.StatusFailed,
		"other-upstream-model",
		&badGateway,
		"other user's private failure",
		baseTime.Add(51*time.Second),
	)

	requestCtx := authz.NewUserContext(context.Background(), member.ID)
	requestCtx = contexts.WithUser(requestCtx, member)
	requestCtx = contexts.WithProjectID(requestCtx, projectRow.ID)
	activity, err := (&CampusCatalogService{client: client}).GetAPIActivity(requestCtx, nil)
	require.NoError(t, err)
	require.Len(t, activity.APIKeys, 1)
	require.Equal(t, memberKey.Name, activity.APIKeys[0].Name)
	require.Equal(t, 1, activity.APIKeys[0].SuccessCount)
	require.Equal(t, 2, activity.APIKeys[0].ErrorCount)
	require.Len(t, activity.Events, 4)

	eventsByID := make(map[string]CampusAPIActivityEvent, len(activity.Events))
	for _, event := range activity.Events {
		eventsByID[event.RequestID] = event
		require.NotEqual(t, "other-private-model", event.Model)
		require.NotContains(t, event.ErrorMessage, "other user's private failure")
	}

	recoveredEvent := eventsByID[strconv.Itoa(recoveredRequest.ID)]
	require.Equal(t, "completed", recoveredEvent.Status)
	require.True(t, recoveredEvent.Recovered)
	require.Equal(t, "upstream-model-b", recoveredEvent.Model)
	require.Equal(t, "Final route", recoveredEvent.FinalChannel)
	require.Len(t, recoveredEvent.Attempts, 2)
	require.Contains(t, recoveredEvent.Attempts[0].ErrorMessage, "upstream failed")
	require.NotContains(t, recoveredEvent.Attempts[0].ErrorMessage, "secret-value")
	require.Empty(t, recoveredEvent.ErrorMessage)

	pendingEvent := eventsByID[strconv.Itoa(pendingRequest.ID)]
	require.Equal(t, "pending", pendingEvent.Status, "a completed execution must not promote a pending request")
	require.False(t, pendingEvent.Recovered)
	require.Equal(t, "pending-upstream-model", pendingEvent.Model)
	require.Equal(t, http.StatusOK, *pendingEvent.StatusCode)

	deliveryEvent := eventsByID[strconv.Itoa(failedAfterCompletedExecution.ID)]
	require.Equal(t, "failed", deliveryEvent.Status, "the request's final status is authoritative")
	require.False(t, deliveryEvent.Recovered)
	require.Equal(t, http.StatusOK, *deliveryEvent.StatusCode, "execution metadata may supplement, but not overwrite, final status")
	require.Empty(t, deliveryEvent.ErrorMessage)

	attemptlessEvent := eventsByID[strconv.Itoa(attemptlessFailedRequest.ID)]
	require.Equal(t, "failed", attemptlessEvent.Status)
	require.Empty(t, attemptlessEvent.Attempts)
	require.Empty(t, attemptlessEvent.ErrorCategory)
	require.Empty(t, attemptlessEvent.ErrorMessage, "the Request schema has no request-level error field; do not invent one")

	_, err = (&CampusCatalogService{client: client}).GetAPIActivity(requestCtx, &otherKey.ID)
	require.ErrorIs(t, err, ErrCampusCatalogForbidden)
}

func TestCampusProbeModelCandidatesPreferEvidenceOverArrayPosition(t *testing.T) {
	ch := &ent.Channel{
		DefaultTestModel: "gpt-5.5-codex",
		SupportedModels: []string{
			"gpt-5",
			"gpt-5.4",
			"gpt-5.6-sol",
			"gpt-5.5-codex",
			"gpt-5.6-terra",
			"",
		},
	}

	require.Equal(t, []string{
		"gpt-5.5-codex",
		"gpt-5.4",
		"gpt-5.6-terra",
		"gpt-5.6-sol",
		"gpt-5",
	}, campusProbeModelCandidates(ch, []string{"gpt-5.4", "gpt-5.5-codex", "gpt-5.4"}))

	require.Equal(t, "gpt-5.4", firstCampusProbeModel(&ent.Channel{
		SupportedModels: []string{"gpt-5", "gpt-5.4"},
	}))
	require.Equal(t, []string{"gpt-5.6-sol", "gpt-5.4", "gpt-5"}, campusProbeModelCandidates(&ent.Channel{
		SupportedModels: []string{"gpt-5", "gpt-5.6-sol", "gpt-5.4"},
	}, nil))
	require.Equal(t, []string{"gpt-5.6-sol", "gpt-5.4"}, campusProbeModelCandidates(&ent.Channel{
		DefaultTestModel: "retired-default",
		SupportedModels:  []string{"gpt-5.4", "gpt-5.6-sol"},
	}, []string{"retired-history"}))
}

func TestPrepareCampusChannelProbeUsesRecentProductionSuccessOnly(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:campus_probe_models?mode=memory&_fk=1")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	projectRow := client.Project.Create().
		SetName("Campus").
		SetStatus(project.StatusActive).
		SaveX(setupCtx)
	member := client.User.Create().
		SetEmail("member@mails.ucas.ac.cn").
		SetPassword("hash").
		SaveX(setupCtx)
	client.UserProject.Create().SetUser(member).SetProject(projectRow).SaveX(setupCtx)
	ch := client.Channel.Create().
		SetType(channel.TypeCodex).
		SetName("Adaptive probe").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret"}).
		SetSupportedModels([]string{"gpt-5", "gpt-5.6-sol", "gpt-5.4"}).
		SetDefaultTestModel("gpt-5").
		SaveX(setupCtx)

	createCompletedExecution := func(source request.Source, modelID string, createdAt time.Time) {
		req := client.Request.Create().
			SetProjectID(projectRow.ID).
			SetSource(source).
			SetModelID(modelID).
			SetRequestBody(objects.JSONRawMessage(`{}`)).
			SetStatus(request.StatusCompleted).
			SetChannelID(ch.ID).
			SetCreatedAt(createdAt).
			SetUpdatedAt(createdAt).
			SaveX(setupCtx)
		client.RequestExecution.Create().
			SetProjectID(projectRow.ID).
			SetRequest(req).
			SetChannel(ch).
			SetModelID(modelID).
			SetRequestBody(objects.JSONRawMessage(`{}`)).
			SetStatus(requestexecution.StatusCompleted).
			SetCreatedAt(createdAt).
			SetUpdatedAt(createdAt).
			SaveX(setupCtx)
	}
	createCompletedExecution(request.SourceAPI, "gpt-5.4", time.Now().Add(-time.Minute))
	createCompletedExecution(request.SourceTest, "test-only-model", time.Now())

	requestCtx := authz.NewUserContext(context.Background(), member.ID)
	requestCtx = contexts.WithUser(requestCtx, member)
	requestCtx = contexts.WithProjectID(requestCtx, projectRow.ID)
	svc := &CampusCatalogService{client: client}

	guid, models, err := svc.PrepareChannelProbe(requestCtx, ch.ID, nil)
	require.NoError(t, err)
	require.Equal(t, ch.ID, guid.ID)
	require.Equal(t, []string{"gpt-5", "gpt-5.4", "gpt-5.6-sol"}, models)
	require.NotContains(t, models, "test-only-model")
}

func TestPrepareCampusChannelProbeUsesOnlyExplicitSupportedModel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:campus_probe_explicit_model?mode=memory&_fk=1")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	projectRow := client.Project.Create().
		SetName("Campus").
		SetStatus(project.StatusActive).
		SaveX(setupCtx)
	member := client.User.Create().
		SetEmail("member@mails.ucas.ac.cn").
		SetPassword("hash").
		SaveX(setupCtx)
	client.UserProject.Create().SetUser(member).SetProject(projectRow).SaveX(setupCtx)
	ch := client.Channel.Create().
		SetType(channel.TypeCodex).
		SetName("Explicit model probe").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "provider-secret"}).
		SetSupportedModels([]string{"gpt-5.6-sol", "gpt-5.6-terra"}).
		SetDefaultTestModel("gpt-5.6-sol").
		SaveX(setupCtx)

	requestCtx := authz.NewUserContext(context.Background(), member.ID)
	requestCtx = contexts.WithUser(requestCtx, member)
	requestCtx = contexts.WithProjectID(requestCtx, projectRow.ID)
	svc := &CampusCatalogService{client: client}
	selected := "gpt-5.6-terra"

	guid, models, err := svc.PrepareChannelProbe(requestCtx, ch.ID, &selected)
	require.NoError(t, err)
	require.Equal(t, ch.ID, guid.ID)
	require.Equal(t, []string{"gpt-5.6-terra"}, models)

	unsupported := "gpt-5"
	_, _, err = svc.PrepareChannelProbe(requestCtx, ch.ID, &unsupported)
	require.ErrorIs(t, err, ErrCampusCatalogInvalidInput)
}
