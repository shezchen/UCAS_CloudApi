package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

type apiKeyAuthFixture struct {
	auth   *biz.AuthService
	client *ent.Client
	key    string
}

func setupAPIKeyAuthFixture(t *testing.T, allowNoAuth bool) *apiKeyAuthFixture {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:middleware-auth?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	setupCtx := ent.NewContext(authz.WithTestBypass(t.Context()), client)

	owner, err := client.User.Create().
		SetEmail("owner@example.com").
		SetPassword("hashed").
		SetIsOwner(true).
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)

	defaultProject, err := client.Project.Create().
		SetName("Default").
		SetStatus(project.StatusActive).
		Save(setupCtx)
	require.NoError(t, err)

	key, err := biz.GenerateAPIKey("ah")
	require.NoError(t, err)
	_, err = client.APIKey.Create().
		SetName("Test Key").
		SetKey(key).
		SetUserID(owner.ID).
		SetProjectID(defaultProject.ID).
		SetStatus(apikey.StatusEnabled).
		Save(setupCtx)
	require.NoError(t, err)

	if allowNoAuth {
		_, err = client.APIKey.Create().
			SetName(biz.NoAuthAPIKeyName).
			SetKey(biz.NoAuthAPIKeyValue).
			SetUserID(owner.ID).
			SetProjectID(defaultProject.ID).
			SetType(apikey.TypeNoauth).
			SetStatus(apikey.StatusEnabled).
			Save(setupCtx)
		require.NoError(t, err)
	}

	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	projectService := biz.NewProjectService(biz.ProjectServiceParams{
		CacheConfig: cacheConfig,
		Ent:         client,
	})
	apiKeyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:    cacheConfig,
		Ent:            client,
		ProjectService: projectService,
		KeyPrefix:      "ah",
	})
	t.Cleanup(apiKeyService.Stop)

	authService := biz.NewAuthService(biz.AuthServiceParams{
		SystemService: biz.NewSystemService(biz.SystemServiceParams{
			CacheConfig: cacheConfig,
			Ent:         client,
		}),
		APIKeyService: apiKeyService,
		UserService: biz.NewUserService(biz.UserServiceParams{
			CacheConfig:   cacheConfig,
			Ent:           client,
			APIKeyService: apiKeyService,
		}),
		Ent:         client,
		AllowNoAuth: allowNoAuth,
	})

	return &apiKeyAuthFixture{auth: authService, client: client, key: key}
}

func (f *apiKeyAuthFixture) call(t *testing.T, authorization string) (*httptest.ResponseRecorder, *ent.APIKey) {
	t.Helper()

	gin.SetMode(gin.TestMode)

	var authenticated *ent.APIKey

	router := gin.New()
	router.Use(WithAPIKeyConfig(f.auth, nil))
	router.GET("/v1/models", func(c *gin.Context) {
		authenticated, _ = contexts.GetAPIKey(c.Request.Context())
		c.Status(http.StatusOK)
	})

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request = request.WithContext(ent.NewContext(request.Context(), f.client))

	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	return recorder, authenticated
}

func TestWithAPIKeyConfig_AcceptsValidKey(t *testing.T) {
	fixture := setupAPIKeyAuthFixture(t, false)

	response, authenticated := fixture.call(t, "Bearer "+fixture.key)
	require.Equal(t, http.StatusOK, response.Code)
	require.NotNil(t, authenticated)
	require.Equal(t, fixture.key, authenticated.Key)
}

func TestWithAPIKeyConfig_RejectsUnknownKeyWhenNoAuthDisabled(t *testing.T) {
	fixture := setupAPIKeyAuthFixture(t, false)

	response, _ := fixture.call(t, "Bearer ah-not-a-real-key")
	require.Equal(t, http.StatusUnauthorized, response.Code)

	missing, _ := fixture.call(t, "")
	require.Equal(t, http.StatusUnauthorized, missing.Code)
}

// Clients of the OpenAI, Anthropic and Gemini SDKs cannot omit the credential,
// so a placeholder key has to keep working while API auth is disabled.
func TestWithAPIKeyConfig_FallsBackToNoAuthForUnusableKey(t *testing.T) {
	fixture := setupAPIKeyAuthFixture(t, true)

	response, authenticated := fixture.call(t, "Bearer sk-dummy")
	require.Equal(t, http.StatusOK, response.Code)
	require.NotNil(t, authenticated)
	require.Equal(t, biz.NoAuthAPIKeyValue, authenticated.Key)

	missing, missingKey := fixture.call(t, "")
	require.Equal(t, http.StatusOK, missing.Code)
	require.NotNil(t, missingKey)
	require.Equal(t, biz.NoAuthAPIKeyValue, missingKey.Key)
}

func TestWithAPIKeyConfig_RejectsNoAuthKeyEvenWhenNoAuthEnabled(t *testing.T) {
	fixture := setupAPIKeyAuthFixture(t, true)

	response, _ := fixture.call(t, "Bearer "+biz.NoAuthAPIKeyValue)
	require.Equal(t, http.StatusUnauthorized, response.Code)
}

// An authentication failure that is not a verdict on the credential must
// surface as an internal error instead of being masked as an invalid key or
// silently served as NoAuth.
func TestWithAPIKeyConfig_InternalFailureIsNotMaskedAsNoAuth(t *testing.T) {
	for _, allowNoAuth := range []bool{false, true} {
		fixture := setupAPIKeyAuthFixture(t, allowNoAuth)
		require.NoError(t, fixture.client.Close())

		response, authenticated := fixture.call(t, "Bearer ah-not-a-real-key")
		require.Equal(t, http.StatusInternalServerError, response.Code, "allow_no_auth=%t", allowNoAuth)
		require.Nil(t, authenticated)
	}
}

func TestIsInvalidAPIKeyError(t *testing.T) {
	require.False(t, isInvalidAPIKeyError(nil))
	require.True(t, isInvalidAPIKeyError(biz.ErrInvalidAPIKey))
	require.True(t, isInvalidAPIKeyError(&ent.NotFoundError{}))
	require.False(t, isInvalidAPIKeyError(context.DeadlineExceeded))
}
