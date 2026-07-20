package biz

import (
	"context"
	"encoding/json"
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
				return []ModelFacade{{ID: "kimi-k2.6"}, {ID: "gpt-5"}, {ID: "gpt-5"}}, nil
			case "Beta":
				return []ModelFacade{{ID: "kimi-k2.5"}, {ID: " "}}, nil
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
	require.Equal(t, []CampusAPIKeyResources{
		{Name: "Alpha", Models: []string{"gpt-5", "kimi-k2.6"}},
		{Name: "Beta", Models: []string{"kimi-k2.5"}},
	}, resources.APIKeys)

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
	require.Equal(t, []CampusAPIKeyResources{{Name: "Owner Key", Models: []string{"Owner Key"}}}, resources.APIKeys)
}

func TestSanitizeCampusChannelDescriptionCapsAndRemovesFormatting(t *testing.T) {
	input := strings.Repeat("界", campusChannelDescriptionMaxRunes+5) + "\u200b"
	got := sanitizeCampusChannelDescription(&input)
	require.Len(t, []rune(got), campusChannelDescriptionMaxRunes)
	require.NotContains(t, got, "\u200b")
}
