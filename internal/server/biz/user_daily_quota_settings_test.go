package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func TestUserDailyQuotaSettings(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:user-daily-quota-settings?mode=memory&_fk=1")
	defer client.Close()

	settingsService := NewSystemService(SystemServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)

	settings, err := settingsService.UserDailyQuotaSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, DefaultUserDailyTokenLimit, settings.DailyTokenLimit)

	updated := UserDailyQuotaSettings{DailyTokenLimit: 345_000_000}
	require.NoError(t, settingsService.SetUserDailyQuotaSettings(ctx, updated))

	settings, err = settingsService.UserDailyQuotaSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, updated, *settings)

	require.ErrorContains(t, settingsService.SetUserDailyQuotaSettings(ctx, UserDailyQuotaSettings{DailyTokenLimit: -1}), "cannot be negative")
}
