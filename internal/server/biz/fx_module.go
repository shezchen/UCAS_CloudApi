package biz

import (
	"context"
	"time"

	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/scheduler"
)

var Module = fx.Module("biz",
	fx.Provide(NewLiveStreamRegistry),
	fx.Provide(NewSystemService),
	fx.Provide(NewWebhookNotifier),
	fx.Provide(NewPasswordResetVerificationExecutor),
	fx.Provide(NewAuthService),
	fx.Provide(NewChannelService),
	fx.Provide(NewRequestService),
	fx.Provide(NewUsageLogService),
	fx.Provide(NewVideoService),
	fx.Provide(NewUserService),
	fx.Provide(NewAPIKeyService),
	fx.Provide(NewProjectService),
	fx.Provide(NewRoleService),
	fx.Provide(NewThreadService),
	fx.Provide(NewTraceService),
	fx.Provide(NewDataStorageService),
	fx.Provide(NewChannelOverrideTemplateService),
	fx.Provide(NewModelService),
	fx.Provide(NewCampusCatalogService),
	fx.Provide(NewChannelProbeService),
	fx.Provide(NewPromptService),
	fx.Provide(NewPromptProtectionRuleService),
	fx.Provide(NewQuotaService),
	fx.Provide(NewProviderQuotaService),
	fx.Provide(NewOIDCService),
	fx.Provide(NewAPIKeyProfileTemplateService),
	fx.Invoke(func(lc fx.Lifecycle, svc *APIKeyService) {
		lc.Append(fx.Hook{
			OnStop: func(ctx context.Context) error {
				svc.Stop()
				return nil
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, registry *LiveStreamRegistry) {
		var cancel context.CancelFunc
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				var bgCtx context.Context
				bgCtx, cancel = context.WithCancel(context.Background())
				registry.StartSweeper(bgCtx)
				return nil
			},
			OnStop: func(ctx context.Context) error {
				if cancel != nil {
					cancel()
				}
				return nil
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *SystemService) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return svc.EnsureCampusSharingPolicyV1(ctx)
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *ChannelService, s *scheduler.Scheduler) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Error(context.Background(), "initChannelPerformances panicked", log.Any("panic", r))
						}
					}()
					svc.initChannelPerformances(context.Background())
				}()
				return svc.RegisterScheduledTasks(ctx, s)
			},
			OnStop: func(ctx context.Context) error {
				svc.Stop()
				return nil
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *DataStorageService, s *scheduler.Scheduler) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return svc.RegisterScheduledTasks(ctx, s)
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *ChannelProbeService, s *scheduler.Scheduler) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return svc.RegisterScheduledTasks(ctx, s)
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *RequestService, s *scheduler.Scheduler) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				if _, err := authz.RunWithSystemBypass(ctx, "campus-error-upgrade-sanitize", func(bypassCtx context.Context) (int, error) {
					if _, err := svc.ScrubExpiredCampusExecutionErrors(bypassCtx); err != nil {
						return 0, err
					}
					return svc.SanitizeRetainedCampusExecutionErrors(bypassCtx)
				}); err != nil {
					return err
				}

				return s.Register(ctx, scheduler.TaskSpec{
					Name:        "campus-error-retention",
					Description: "Remove sanitized API diagnostics after their rolling six-hour retention window",
					CronExpr:    "*/15 * * * *",
					Timezone:    "Asia/Shanghai",
				}, func(taskCtx context.Context) {
					if _, err := svc.ScrubExpiredCampusExecutionErrors(taskCtx); err != nil {
						log.Error(taskCtx, "failed to scrub expired campus execution errors", log.Cause(err))
					}
				})
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *UsageLogService) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				_, err := svc.WalletService.EnsureStartedAt(ctx, time.Now().UTC())
				return err
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *PromptService, s *scheduler.Scheduler) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return svc.RegisterScheduledTasks(ctx, s)
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *PromptProtectionRuleService) {
		lc.Append(fx.Hook{
			OnStop: func(ctx context.Context) error {
				svc.Stop()
				return nil
			},
		})
	}),
	fx.Invoke(func(lc fx.Lifecycle, svc *ProviderQuotaService, s *scheduler.Scheduler) {
		lc.Append(fx.Hook{
			OnStart: func(ctx context.Context) error {
				return svc.RegisterScheduledTasks(ctx, s)
			},
		})
	}),
)
