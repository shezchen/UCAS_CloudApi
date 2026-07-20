package gql

import (
	"context"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

func setupTestQueryResolver(t *testing.T) (*queryResolver, context.Context, *ent.Client) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	resolver := &queryResolver{&Resolver{client: client}}

	return resolver, ctx, client
}

func TestQueryResolver_AllChannelSummarys_ProjectProfileUsesIntersection(t *testing.T) {
	resolver, ctx, client := setupTestQueryResolver(t)
	defer client.Close()

	idOnlyChannel, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("ID Only").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-1"}).
		SetSupportedModels([]string{"id-only-model"}).
		SetDefaultTestModel("id-only-model").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	matchingChannel, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Matching").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-2"}).
		SetSupportedModels([]string{"matching-model"}).
		SetDefaultTestModel("matching-model").
		SetStatus(channel.StatusEnabled).
		SetTags([]string{"allowed"}).
		Save(ctx)
	require.NoError(t, err)

	projectEntity, err := client.Project.Create().
		SetName("Project A").
		SetDescription("test project").
		SetProfiles(&objects.ProjectProfiles{
			ActiveProfile: "production",
			Profiles: []objects.ProjectProfile{
				{
					Name:        "production",
					ChannelIDs:  []int{idOnlyChannel.ID, matchingChannel.ID},
					ChannelTags: []string{"allowed"},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	projectCtx := contexts.WithProjectID(ctx, projectEntity.ID)

	channels, err := resolver.AllChannelSummarys(projectCtx, nil)
	require.NoError(t, err)
	require.Len(t, channels, 1)
	require.Equal(t, matchingChannel.ID, channels[0].ID)
}

func TestQueryResolver_AllChannelTags_ProjectProfileFiltersVisibleTags(t *testing.T) {
	resolver, ctx, client := setupTestQueryResolver(t)
	defer client.Close()

	_, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Visible Channel").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-visible"}).
		SetSupportedModels([]string{"visible-model"}).
		SetDefaultTestModel("visible-model").
		SetStatus(channel.StatusEnabled).
		SetTags([]string{"shared", "visible"}).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Hidden Channel").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-hidden"}).
		SetSupportedModels([]string{"hidden-model"}).
		SetDefaultTestModel("hidden-model").
		SetStatus(channel.StatusEnabled).
		SetTags([]string{"shared", "hidden"}).
		Save(ctx)
	require.NoError(t, err)

	projectEntity, err := client.Project.Create().
		SetName("Project B").
		SetDescription("test project").
		SetProfiles(&objects.ProjectProfiles{
			ActiveProfile: "production",
			Profiles: []objects.ProjectProfile{
				{
					Name:        "production",
					ChannelTags: []string{"visible"},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	projectCtx := contexts.WithProjectID(ctx, projectEntity.ID)

	tags, err := resolver.AllChannelTags(projectCtx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"shared", "visible"}, lo.Uniq(tags))
}

func TestChannelResolver_SecretsVisibleOnlyToOwnerOrDonor(t *testing.T) {
	donorID := 41
	channelEntity := &ent.Channel{
		ID:              7,
		Type:            channel.TypeOpenai,
		UserID:          &donorID,
		Credentials:     objects.ChannelCredentials{APIKey: "donated-secret"},
		DisabledAPIKeys: []objects.DisabledAPIKey{{Key: "disabled-secret"}},
	}
	resolver := &channelResolver{&Resolver{}}

	tests := []struct {
		name       string
		user       *ent.User
		wantSecret bool
	}{
		{
			name:       "owner",
			user:       &ent.User{ID: 1, IsOwner: true},
			wantSecret: true,
		},
		{
			name:       "donor",
			user:       &ent.User{ID: donorID},
			wantSecret: true,
		},
		{
			name:       "another user even with legacy write scope",
			user:       &ent.User{ID: 99, Scopes: []string{"write_channels"}},
			wantSecret: false,
		},
		{
			name:       "no authenticated user",
			user:       nil,
			wantSecret: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.user != nil {
				ctx = contexts.WithUser(ctx, tt.user)
			}

			credentials, err := resolver.Credentials(ctx, channelEntity)
			require.NoError(t, err)
			disabledKeys, err := resolver.DisabledAPIKeys(ctx, channelEntity)
			require.NoError(t, err)

			if tt.wantSecret {
				require.NotNil(t, credentials)
				require.Equal(t, []string{"donated-secret"}, credentials.APIKeys)
				require.Len(t, disabledKeys, 1)
				require.Equal(t, "disabled-secret", disabledKeys[0].Key)
			} else {
				require.Nil(t, credentials)
				require.Nil(t, disabledKeys)
			}
		})
	}
}

func TestChannelResolver_ExpiredChannelSecretsAreHiddenBeforeCleanup(t *testing.T) {
	donorID := 41
	past := time.Now().Add(-time.Minute)
	channelEntity := &ent.Channel{
		ID:              8,
		Type:            channel.TypeOpenai,
		UserID:          &donorID,
		ExpiresAt:       &past,
		Credentials:     objects.ChannelCredentials{APIKey: "expired-secret"},
		DisabledAPIKeys: []objects.DisabledAPIKey{{Key: "expired-disabled-secret"}},
	}
	resolver := &channelResolver{&Resolver{}}
	donorCtx := contexts.WithUser(context.Background(), &ent.User{ID: donorID})

	credentials, err := resolver.Credentials(donorCtx, channelEntity)
	require.NoError(t, err)
	require.Nil(t, credentials)
	disabledKeys, err := resolver.DisabledAPIKeys(donorCtx, channelEntity)
	require.NoError(t, err)
	require.Nil(t, disabledKeys)
}

func TestMutationResolver_TestChannelRejectsDonorURLProxyBeforeOutbound(t *testing.T) {
	resolver := &mutationResolver{&Resolver{}}
	donorCtx := contexts.WithUser(context.Background(), &ent.User{ID: 41})

	_, err := resolver.TestChannel(donorCtx, TestChannelInput{
		ChannelID: objects.GUID{Type: ent.TypeChannel, ID: 8},
		Proxy: &httpclient.ProxyConfig{
			Type: httpclient.ProxyTypeURL,
			URL:  "http://127.0.0.1:8080",
		},
	})
	require.ErrorContains(t, err, "restricted address")
}

func TestQueryResolver_FetchModelsAllowsDonorWithSafeURLWithoutWriteScope(t *testing.T) {
	modelFetcher := biz.NewModelFetcher(httpclient.NewHttpClient(), nil)
	resolver := &queryResolver{&Resolver{modelFetcher: modelFetcher}}
	donor := &ent.User{ID: 41}
	donorCtx := authz.NewUserContext(context.Background(), donor.ID)
	donorCtx = contexts.WithUser(donorCtx, donor)

	payload, err := resolver.FetchModels(donorCtx, biz.FetchModelsInput{
		ChannelType: channel.TypeVolcengine.String(),
		BaseURL:     "https://8.8.8.8/v1",
	})
	require.NoError(t, err)
	require.NotNil(t, payload)
	require.Empty(t, payload.Models)

	legacyWriter := &ent.User{ID: 42, Scopes: []string{"write_channels"}}
	legacyWriterCtx := authz.NewUserContext(context.Background(), legacyWriter.ID)
	legacyWriterCtx = contexts.WithUser(legacyWriterCtx, legacyWriter)
	_, err = resolver.FetchModels(legacyWriterCtx, biz.FetchModelsInput{
		ChannelType: channel.TypeVolcengine.String(),
		BaseURL:     "https://169.254.169.254/latest/meta-data",
	})
	require.ErrorContains(t, err, "restricted address")

	privilegedCtx := authz.WithTestBypass(context.Background())
	payload, err = resolver.FetchModels(privilegedCtx, biz.FetchModelsInput{
		ChannelType: channel.TypeVolcengine.String(),
		BaseURL:     "http://127.0.0.1:8080/v1",
	})
	require.NoError(t, err, "non-user system principals keep the existing write-scope path")
	require.NotNil(t, payload)
}

func TestQueryResolver_FetchModelsChannelIDUsesDonationOwnership(t *testing.T) {
	resolver, setupCtx, client := setupTestQueryResolver(t)
	defer client.Close()

	donor, err := client.User.Create().
		SetEmail("fetch-donor@example.com").
		SetPassword("test-password-hash").
		Save(setupCtx)
	require.NoError(t, err)
	other, err := client.User.Create().
		SetEmail("fetch-other@example.com").
		SetPassword("test-password-hash").
		Save(setupCtx)
	require.NoError(t, err)

	future := time.Now().Add(time.Hour)
	otherChannel, err := client.Channel.Create().
		SetType(channel.TypeVolcengine).
		SetName("other donation").
		SetBaseURL("https://8.8.8.8/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "other-secret"}).
		SetSupportedModels([]string{"other-model"}).
		SetDefaultTestModel("other-model").
		SetUserID(other.ID).
		SetExpiresAt(future).
		Save(setupCtx)
	require.NoError(t, err)

	channelService := biz.NewChannelServiceForTest(client)
	defer channelService.Stop()
	resolver.modelFetcher = biz.NewModelFetcher(httpclient.NewHttpClient(), channelService)
	donorCtx := authz.NewUserContext(ent.NewContext(context.Background(), client), donor.ID)
	donorCtx = contexts.WithUser(donorCtx, donor)

	_, err = resolver.FetchModels(donorCtx, biz.FetchModelsInput{
		ChannelType: channel.TypeVolcengine.String(),
		BaseURL:     "https://8.8.8.8/v1",
		ChannelID:   &otherChannel.ID,
	})
	require.ErrorContains(t, err, "failed to get donated channel")
}
