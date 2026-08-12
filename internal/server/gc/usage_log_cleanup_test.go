package gc

import (
	"context"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func setupUsageLogCleanupWorker(t *testing.T) (*Worker, context.Context, *ent.Client) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { client.Close() })

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	systemService := biz.NewSystemService(biz.SystemServiceParams{Ent: client})

	worker := &Worker{
		SystemService: systemService,
		QuotaService:  biz.NewQuotaService(client, systemService),
		Ent:           client,
		Config:        Config{CRON: "0 0 * * *"},
	}

	return worker, ctx, client
}

func createAPIKeyWithQuotaPeriod(
	t *testing.T,
	ctx context.Context,
	client *ent.Client,
	name string,
	period objects.APIKeyQuotaPeriod,
) {
	t.Helper()

	projectRow, err := client.Project.Create().SetName(name).SetStatus(project.StatusActive).Save(ctx)
	require.NoError(t, err)

	_, err = client.APIKey.Create().
		SetName(name).
		SetKey("sk-" + name).
		SetProjectID(projectRow.ID).
		SetProfiles(&objects.APIKeyProfiles{
			ActiveProfile: "default",
			Profiles: []objects.APIKeyProfile{{
				Name:  "default",
				Quota: &objects.APIKeyQuota{Requests: lo.ToPtr(int64(10)), Period: period},
			}},
		}).
		Save(ctx)
	require.NoError(t, err)
}

func createUsageLogAt(t *testing.T, ctx context.Context, client *ent.Client, createdAt time.Time) int {
	t.Helper()

	usageLog, err := client.UsageLog.Create().
		SetRequestID(1).
		SetAPIKeyID(1).
		SetProjectID(1).
		SetChannelID(1).
		SetModelID("m").
		SetCreatedAt(createdAt).
		Save(ctx)
	require.NoError(t, err)

	return usageLog.ID
}

func TestWorker_usageLogCleanupCutoff_ClampsToWeeklyAccountWindow(t *testing.T) {
	worker, ctx, _ := setupUsageLogCleanupWorker(t)

	cutoff, ok := worker.usageLogCleanupCutoff(ctx, 1)
	require.True(t, ok)

	weeklyStart := biz.CurrentWeeklyQuotaWindowStart(time.Now())
	require.Equal(t, weeklyStart, cutoff.Time)
	require.LessOrEqual(t, cutoff.RetentionDays, 7)
}

func TestWorker_usageLogCleanupCutoff_ClampsToCalendarMonthAPIKeyWindow(t *testing.T) {
	worker, ctx, client := setupUsageLogCleanupWorker(t)
	createAPIKeyWithQuotaPeriod(t, ctx, client, "monthly", objects.APIKeyQuotaPeriod{
		Type: objects.APIKeyQuotaPeriodTypeCalendarDuration,
		CalendarDuration: &objects.APIKeyQuotaCalendarDuration{
			Unit: objects.APIKeyQuotaCalendarDurationUnitMonth,
		},
	})

	now := time.Now()

	floor, err := worker.QuotaService.UsageLogRetentionFloor(ctx, now)
	require.NoError(t, err)
	require.False(t, floor.Unbounded)

	cutoff, ok := worker.usageLogCleanupCutoff(ctx, 8)
	require.True(t, ok)
	require.WithinDuration(t, floor.Start, cutoff.Time, time.Minute)
	require.True(t, cutoff.Time.Before(biz.CurrentWeeklyQuotaWindowStart(now)) ||
		cutoff.Time.Equal(biz.CurrentWeeklyQuotaWindowStart(now)))
}

func TestWorker_usageLogCleanupCutoff_RefusesWhenAQuotaIsAllTime(t *testing.T) {
	worker, ctx, client := setupUsageLogCleanupWorker(t)
	createAPIKeyWithQuotaPeriod(t, ctx, client, "all-time", objects.APIKeyQuotaPeriod{
		Type: objects.APIKeyQuotaPeriodTypeAllTime,
	})

	_, ok := worker.usageLogCleanupCutoff(ctx, 30)
	require.False(t, ok, "an all-time quota has no safe cutoff")
}

func TestWorker_usageLogCleanupCutoff_KeepsRequestedCutoffWhenSafe(t *testing.T) {
	worker, ctx, _ := setupUsageLogCleanupWorker(t)

	cutoff, ok := worker.usageLogCleanupCutoff(ctx, 90)
	require.True(t, ok)
	require.Equal(t, 90, cutoff.RetentionDays)
	require.WithinDuration(t, time.Now().AddDate(0, 0, -90), cutoff.Time, time.Minute)
}

func TestWorker_cleanupUsageLogs_KeepsLogsInsideAnAPIKeyQuotaWindow(t *testing.T) {
	worker, ctx, client := setupUsageLogCleanupWorker(t)
	createAPIKeyWithQuotaPeriod(t, ctx, client, "past-60d", objects.APIKeyQuotaPeriod{
		Type: objects.APIKeyQuotaPeriodTypePastDuration,
		PastDuration: &objects.APIKeyQuotaPastDuration{
			Value: 60,
			Unit:  objects.APIKeyQuotaPastDurationUnitDay,
		},
	})

	now := time.Now()
	insideWindow := createUsageLogAt(t, ctx, client, now.AddDate(0, 0, -30))
	outsideWindow := createUsageLogAt(t, ctx, client, now.AddDate(0, 0, -100))

	require.NoError(t, worker.cleanupUsageLogs(ctx, 10, true))

	_, err := client.UsageLog.Get(ctx, insideWindow)
	require.NoError(t, err, "a usage log inside a live quota window must survive cleanup")

	_, err = client.UsageLog.Get(ctx, outsideWindow)
	require.True(t, ent.IsNotFound(err), "a usage log older than every quota window must be deleted")
}

func TestWorker_cleanupUsageLogs_DeletesNothingWhenAQuotaIsAllTime(t *testing.T) {
	worker, ctx, client := setupUsageLogCleanupWorker(t)
	createAPIKeyWithQuotaPeriod(t, ctx, client, "all-time", objects.APIKeyQuotaPeriod{
		Type: objects.APIKeyQuotaPeriodTypeAllTime,
	})

	ancient := createUsageLogAt(t, ctx, client, time.Now().AddDate(-3, 0, 0))

	require.NoError(t, worker.cleanupUsageLogs(ctx, 10, true))

	_, err := client.UsageLog.Get(ctx, ancient)
	require.NoError(t, err, "an all-time quota still aggregates the oldest usage log")
}

func TestWorker_PreviewCleanup_ReportsEffectiveRetention(t *testing.T) {
	worker, ctx, client := setupUsageLogCleanupWorker(t)

	now := time.Now()
	createUsageLogAt(t, ctx, client, now.AddDate(0, 0, -100))
	createUsageLogAt(t, ctx, client, now.Add(-time.Hour))

	items, err := worker.PreviewCleanup(ctx, TriggerGcCleanupInput{UsageLogsCleanupDays: 1})
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, "usage_logs", items[0].ResourceType)
	require.Equal(t, 1, items[0].EstimatedCount)

	// The requested single day is clamped, so echoing it back would contradict
	// the cutoff shown next to it.
	require.NotEqual(t, 1, items[0].RetentionDays)
	require.WithinDuration(
		t,
		items[0].CutoffTime,
		now.AddDate(0, 0, -items[0].RetentionDays),
		24*time.Hour,
	)
}

func TestWorker_PreviewCleanup_OmitsUsageLogsWhenNoCutoffIsSafe(t *testing.T) {
	worker, ctx, client := setupUsageLogCleanupWorker(t)
	createAPIKeyWithQuotaPeriod(t, ctx, client, "all-time", objects.APIKeyQuotaPeriod{
		Type: objects.APIKeyQuotaPeriodTypeAllTime,
	})
	createUsageLogAt(t, ctx, client, time.Now().AddDate(-3, 0, 0))

	items, err := worker.PreviewCleanup(ctx, TriggerGcCleanupInput{UsageLogsCleanupDays: 1})
	require.NoError(t, err)
	require.Empty(t, items, "the preview must not promise deletions that cleanup refuses to make")
}

func TestWorker_usageLogCleanupCutoff_RefusesWithoutQuotaService(t *testing.T) {
	worker := &Worker{Ent: nil, Config: Config{CRON: "0 0 * * *"}}

	_, ok := worker.usageLogCleanupCutoff(context.Background(), 30)
	require.False(t, ok)
}
