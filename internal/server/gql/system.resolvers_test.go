package gql

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

func setupTestSystemMutationResolver(t *testing.T) (*mutationResolver, context.Context, *ent.Client) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	systemService := &biz.SystemService{
		Cache: xcache.NewFromConfig[ent.System](xcache.Config{Mode: xcache.ModeMemory}),
	}

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	resolver := &mutationResolver{&Resolver{systemService: systemService}}
	return resolver, ctx, client
}

func TestMutationResolver_UpdateSystemChannelSettings_MergesAutoSyncWithoutOverwritingProbe(t *testing.T) {
	resolver, ctx, client := setupTestSystemMutationResolver(t)
	defer client.Close()

	err := resolver.systemService.SetChannelSetting(ctx, biz.SystemChannelSettings{
		Probe: biz.ChannelProbeSetting{
			Enabled:   true,
			Frequency: biz.ProbeFrequency5Min,
		},
		AutoSync: biz.ChannelModelAutoSyncSetting{
			Frequency: biz.AutoSyncFrequencyOneHour,
		},
	})
	require.NoError(t, err)

	ok, err := resolver.UpdateSystemChannelSettings(ctx, biz.SystemChannelSettings{
		AutoSync: biz.ChannelModelAutoSyncSetting{
			Frequency: biz.AutoSyncFrequencySixHours,
		},
	})
	require.NoError(t, err)
	require.True(t, ok)

	setting, err := resolver.systemService.ChannelSetting(ctx)
	require.NoError(t, err)
	require.True(t, setting.Probe.Enabled)
	require.Equal(t, biz.ProbeFrequency5Min, setting.Probe.Frequency)
	require.Equal(t, biz.AutoSyncFrequencySixHours, setting.AutoSync.Frequency)
}

func TestMutationResolver_UpdateSystemChannelSettings_MergesProbeWithoutOverwritingAutoSync(t *testing.T) {
	resolver, ctx, client := setupTestSystemMutationResolver(t)
	defer client.Close()

	err := resolver.systemService.SetChannelSetting(ctx, biz.SystemChannelSettings{
		Probe: biz.ChannelProbeSetting{
			Enabled:   true,
			Frequency: biz.ProbeFrequency5Min,
		},
		AutoSync: biz.ChannelModelAutoSyncSetting{
			Frequency: biz.AutoSyncFrequencySixHours,
		},
	})
	require.NoError(t, err)

	ok, err := resolver.UpdateSystemChannelSettings(ctx, biz.SystemChannelSettings{
		Probe: biz.ChannelProbeSetting{
			Enabled:   false,
			Frequency: biz.ProbeFrequency1Hour,
		},
	})
	require.NoError(t, err)
	require.True(t, ok)

	setting, err := resolver.systemService.ChannelSetting(ctx)
	require.NoError(t, err)
	require.False(t, setting.Probe.Enabled)
	require.Equal(t, biz.ProbeFrequency1Hour, setting.Probe.Frequency)
	require.Equal(t, biz.AutoSyncFrequencySixHours, setting.AutoSync.Frequency)
}

type providerQuotaResolverRoundTripFunc func(*http.Request) (*http.Response, error)

func (f providerQuotaResolverRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func providerQuotaResolverTestJWT(t *testing.T, accountID string) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	})
	signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	return signed
}

func providerQuotaResolverUserContext(ctx context.Context, currentUser *ent.User) context.Context {
	return contexts.WithUser(authz.NewUserContext(ctx, currentUser.ID), currentUser)
}

func TestProviderQuotaMutationsRequireSystemOwnerEvenWithWriteChannels(t *testing.T) {
	resolver := &mutationResolver{&Resolver{}}
	writer := &ent.User{
		ID:       42,
		IsOwner:  false,
		Scopes:   []string{string(scopes.ScopeWriteChannels)},
		Status:   user.StatusActivated,
		Email:    "writer@mails.ucas.ac.cn",
		Password: "hash",
	}
	ctx := providerQuotaResolverUserContext(context.Background(), writer)

	ok, err := resolver.CheckProviderQuotas(ctx)
	require.False(t, ok)
	require.ErrorIs(t, err, ErrNotOwner)

	ok, err = resolver.ResetChannelQuotaNow(ctx, objects.GUID{Type: ent.TypeChannel, ID: 1})
	require.False(t, ok)
	require.ErrorIs(t, err, ErrNotOwner)
}

func TestProviderQuotaMutationsAllowSystemOwner(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:provider-quota-resolvers?mode=memory&cache=shared&_fk=1")
	defer client.Close()

	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	owner := client.User.Create().
		SetEmail("owner@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		SaveX(setupCtx)

	accessToken := providerQuotaResolverTestJWT(t, "acct_owner")
	ch := client.Channel.Create().
		SetName("Owner Codex").
		SetType(channel.TypeCodex).
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{
			OAuth: &objects.OAuthCredentials{AccessToken: accessToken},
		}).
		SetSupportedModels([]string{"gpt-5.6-codex"}).
		SetDefaultTestModel("gpt-5.6-codex").
		SaveX(setupCtx)

	requests := make([]string, 0, 4)
	httpClient := httpclient.NewHttpClientWithClient(&http.Client{
		Transport: providerQuotaResolverRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req.Method+" "+req.URL.Path)

			var body string
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/backend-api/wham/usage":
				body = `{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":10}}}`
			case req.Method == http.MethodGet && req.URL.Path == "/backend-api/wham/rate-limit-reset-credits":
				body = `{"credits":[{"id":"credit_owner","status":"available"}],"available_count":1}`
			case req.Method == http.MethodPost && req.URL.Path == "/backend-api/wham/rate-limit-reset-credits/consume":
				body = `{"code":"reset","windows_reset":1,"credit":{"id":"credit_owner","status":"redeemed"}}`
			default:
				t.Fatalf("unexpected provider quota request: %s %s", req.Method, req.URL.Path)
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}),
	})
	systemService := biz.NewSystemService(biz.SystemServiceParams{
		Ent:         client,
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
	})
	providerQuotaService := biz.NewProviderQuotaService(biz.ProviderQuotaServiceParams{
		Ent:           client,
		SystemService: systemService,
		HttpClient:    httpClient,
	})
	resolver := &mutationResolver{&Resolver{
		systemService:        systemService,
		providerQuotaService: providerQuotaService,
	}}
	ownerCtx := providerQuotaResolverUserContext(ent.NewContext(context.Background(), client), owner)

	ok, err := resolver.CheckProviderQuotas(ownerCtx)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = resolver.ResetChannelQuotaNow(ownerCtx, objects.GUID{Type: ent.TypeChannel, ID: ch.ID})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{
		"GET /backend-api/wham/usage",
		"GET /backend-api/wham/rate-limit-reset-credits",
		"POST /backend-api/wham/rate-limit-reset-credits/consume",
		"GET /backend-api/wham/usage",
	}, requests)
}
