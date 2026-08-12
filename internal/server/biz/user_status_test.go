package biz

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func TestUserService_GetUserStatus(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	service := NewUserService(UserServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)

	created, err := client.User.Create().
		SetEmail("status@example.com").
		SetPassword("hashed").
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	status, err := service.GetUserStatus(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, user.StatusActivated, status)

	_, err = service.GetUserStatus(ctx, created.ID+1000)
	require.Error(t, err)
	require.True(t, ent.IsNotFound(err), "a missing user must stay distinguishable from a failure")

	// A status change invalidates the entry, so access ends without waiting
	// for the cache to expire.
	_, err = service.UpdateUserStatus(ctx, created.ID, user.StatusDeactivated)
	require.NoError(t, err)

	status, err = service.GetUserStatus(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, user.StatusDeactivated, status)
}

func TestUserService_GetUserStatus_UsesCache(t *testing.T) {
	client := setupTestDB(t)

	service := NewUserService(UserServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)

	created, err := client.User.Create().
		SetEmail("cached-status@example.com").
		SetPassword("hashed").
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	_, err = service.GetUserStatus(ctx, created.ID)
	require.NoError(t, err)

	require.NoError(t, client.Close())

	status, err := service.GetUserStatus(ctx, created.ID)
	require.NoError(t, err, "the hot path must be served without a query")
	require.Equal(t, user.StatusActivated, status)
}

func TestUserStatusCache(t *testing.T) {
	clock := &stubClock{now: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)}
	cache := newUserStatusCache(clock.Now)

	_, ok := cache.get(1)
	require.False(t, ok)

	cache.set(1, user.StatusActivated)
	status, ok := cache.get(1)
	require.True(t, ok)
	require.Equal(t, user.StatusActivated, status)

	clock.advance(userStatusCacheTTL + time.Second)
	_, ok = cache.get(1)
	require.False(t, ok, "the entry must expire on its own even without an invalidation")

	cache.set(2, user.StatusActivated)
	cache.invalidate(2)
	_, ok = cache.get(2)
	require.False(t, ok)

	cache.set(3, user.StatusActivated)
	cache.clear()
	_, ok = cache.get(3)
	require.False(t, ok)

	// Expired entries are dropped instead of accumulating.
	cache.set(4, user.StatusActivated)
	clock.advance(userStatusPruneInterval + userStatusCacheTTL)
	cache.set(5, user.StatusActivated)
	require.Len(t, cache.entries, 1)
}

func TestUserStatusCache_NilIsInert(t *testing.T) {
	var cache *userStatusCache

	_, ok := cache.get(1)
	require.False(t, ok)
	require.NotPanics(t, func() {
		cache.set(1, user.StatusActivated)
		cache.invalidate(1)
		cache.clear()
	})
}
