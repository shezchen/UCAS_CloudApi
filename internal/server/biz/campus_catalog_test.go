package biz

import (
	"context"
	"encoding/json"
	"fmt"
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

	donatedChannel := byName["同学共享"]
	require.Equal(t, "donated", donatedChannel.Source)
	require.Equal(t, "目录同学", donatedChannel.Contributor)
	require.Equal(t, "公益 共享说明", donatedChannel.Description)
	require.Equal(t, 2, donatedChannel.ModelCount)

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
