package gql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

func setupUserDailyQuotaSettingsResolver(t *testing.T) (*mutationResolver, *queryResolver, context.Context, *ent.Client) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:user-daily-quota-resolver?mode=memory&_fk=1")
	systemService := biz.NewSystemService(biz.SystemServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})
	resolver := &Resolver{systemService: systemService}

	return &mutationResolver{resolver}, &queryResolver{resolver}, ent.NewContext(context.Background(), client), client
}

func userDailyQuotaSettingsContext(ctx context.Context, currentUser *ent.User) context.Context {
	return contexts.WithUser(authz.NewUserContext(ctx, currentUser.ID), currentUser)
}

func TestUserDailyQuotaSettingsOwnerOnly(t *testing.T) {
	mutationResolver, queryResolver, baseCtx, client := setupUserDailyQuotaSettingsResolver(t)
	defer client.Close()

	setupCtx := authz.WithTestBypass(baseCtx)
	owner, err := client.User.Create().
		SetEmail("owner@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		Save(setupCtx)
	require.NoError(t, err)
	member, err := client.User.Create().
		SetEmail("member@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)

	memberCtx := userDailyQuotaSettingsContext(baseCtx, member)
	_, err = queryResolver.UserDailyQuotaSettings(memberCtx)
	require.ErrorIs(t, err, ErrNotOwner)
	_, err = mutationResolver.UpdateUserDailyQuotaSettings(memberCtx, UpdateUserDailyQuotaSettingsInput{DailyTokenLimit: 123})
	require.ErrorIs(t, err, ErrNotOwner)

	ownerCtx := userDailyQuotaSettingsContext(baseCtx, owner)
	settings, err := queryResolver.UserDailyQuotaSettings(ownerCtx)
	require.NoError(t, err)
	require.Equal(t, biz.DefaultUserDailyTokenLimit, settings.DailyTokenLimit)

	ok, err := mutationResolver.UpdateUserDailyQuotaSettings(ownerCtx, UpdateUserDailyQuotaSettingsInput{DailyTokenLimit: 123})
	require.NoError(t, err)
	require.True(t, ok)

	settings, err = queryResolver.UserDailyQuotaSettings(ownerCtx)
	require.NoError(t, err)
	require.Equal(t, int64(123), settings.DailyTokenLimit)
}
