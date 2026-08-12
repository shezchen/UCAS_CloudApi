package biz

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

type revocationFixture struct {
	users  *UserService
	auth   *AuthService
	client *ent.Client
	ctx    context.Context
}

func TestUserService_DeactivationDisablesAPIKeys(t *testing.T) {
	fixture := setupRevocationFixture(t)

	owner, apiKey := fixture.createUserWithAPIKey(t, user.StatusActivated)

	authenticated, err := fixture.auth.AuthenticateAPIKey(fixture.ctx, apiKey.Key)
	require.NoError(t, err)
	require.Equal(t, apiKey.ID, authenticated.ID)

	_, err = fixture.users.UpdateUserStatus(fixture.ctx, owner.ID, user.StatusDeactivated)
	require.NoError(t, err)

	_, err = fixture.auth.AuthenticateAPIKey(fixture.ctx, apiKey.Key)
	require.ErrorIs(t, err, ErrInvalidAPIKey)

	stored, err := fixture.client.APIKey.Get(fixture.ctx, apiKey.ID)
	require.NoError(t, err)
	require.Equal(t, apikey.StatusDisabled, stored.Status)

	// Reactivation deliberately leaves the keys disabled, which only holds if
	// the cached copy of the key was invalidated as well.
	_, err = fixture.users.UpdateUserStatus(fixture.ctx, owner.ID, user.StatusActivated)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, err := fixture.auth.AuthenticateAPIKey(fixture.ctx, apiKey.Key)

		return err != nil
	}, 5*time.Second, 50*time.Millisecond, "a disabled key must stop working once the cache is invalidated")
}

func TestUserService_DeletionDisablesAPIKeys(t *testing.T) {
	fixture := setupRevocationFixture(t)

	owner, apiKey := fixture.createUserWithAPIKey(t, user.StatusActivated)

	_, err := fixture.auth.AuthenticateAPIKey(fixture.ctx, apiKey.Key)
	require.NoError(t, err)

	require.NoError(t, fixture.users.DeleteUser(fixture.asSystemOwner(t), owner.ID))

	_, err = fixture.auth.AuthenticateAPIKey(fixture.ctx, apiKey.Key)
	require.ErrorIs(t, err, ErrInvalidAPIKey)

	stored, err := fixture.client.APIKey.Get(fixture.ctx, apiKey.ID)
	require.NoError(t, err)
	require.Equal(t, apikey.StatusDisabled, stored.Status)
}

// A UserService without an API key service still has to revoke access.
func TestUserService_DeactivationWithoutAPIKeyService(t *testing.T) {
	fixture := setupRevocationFixture(t)
	fixture.users.APIKeyService = nil

	owner, apiKey := fixture.createUserWithAPIKey(t, user.StatusActivated)

	_, err := fixture.users.UpdateUserStatus(fixture.ctx, owner.ID, user.StatusDeactivated)
	require.NoError(t, err)

	stored, err := fixture.client.APIKey.Get(fixture.ctx, apiKey.ID)
	require.NoError(t, err)
	require.Equal(t, apikey.StatusDisabled, stored.Status)
}

func setupRevocationFixture(t *testing.T) *revocationFixture {
	t.Helper()

	client := setupTestDB(t)
	t.Cleanup(func() { _ = client.Close() })

	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	apiKeyService := NewAPIKeyService(APIKeyServiceParams{
		CacheConfig: cacheConfig,
		Ent:         client,
		ProjectService: NewProjectService(ProjectServiceParams{
			CacheConfig: cacheConfig,
			Ent:         client,
		}),
		KeyPrefix: "ah",
	})
	t.Cleanup(apiKeyService.Stop)

	userService := NewUserService(UserServiceParams{
		CacheConfig:   cacheConfig,
		Ent:           client,
		APIKeyService: apiKeyService,
	})

	authService := NewAuthService(AuthServiceParams{
		SystemService: NewSystemService(SystemServiceParams{
			CacheConfig: cacheConfig,
			Ent:         client,
		}),
		APIKeyService: apiKeyService,
		UserService:   userService,
		Ent:           client,
	})

	return &revocationFixture{
		users:  userService,
		auth:   authService,
		client: client,
		ctx:    ent.NewContext(authz.WithTestBypass(t.Context()), client),
	}
}

// asSystemOwner returns a context acting as a system owner, which deleting a
// user requires.
func (f *revocationFixture) asSystemOwner(t *testing.T) context.Context {
	t.Helper()

	owner, err := f.client.User.Create().
		SetEmail(fmt.Sprintf("system-owner-%d@example.com", time.Now().UnixNano())).
		SetPassword("hashed").
		SetIsOwner(true).
		SetStatus(user.StatusActivated).
		Save(f.ctx)
	require.NoError(t, err)

	return contexts.WithUser(f.ctx, owner)
}

func (f *revocationFixture) createUserWithAPIKey(t *testing.T, status user.Status) (*ent.User, *ent.APIKey) {
	t.Helper()

	owner, err := f.client.User.Create().
		SetEmail(fmt.Sprintf("revocation-%d@example.com", time.Now().UnixNano())).
		SetPassword("hashed").
		SetStatus(status).
		Save(f.ctx)
	require.NoError(t, err)

	proj, err := f.client.Project.Create().
		SetName(uuid.NewString()).
		SetStatus(project.StatusActive).
		Save(f.ctx)
	require.NoError(t, err)

	key, err := GenerateAPIKey("ah")
	require.NoError(t, err)

	created, err := f.client.APIKey.Create().
		SetName("Revocation Key").
		SetKey(key).
		SetUserID(owner.ID).
		SetProjectID(proj.ID).
		SetStatus(apikey.StatusEnabled).
		Save(f.ctx)
	require.NoError(t, err)

	return owner, created
}
