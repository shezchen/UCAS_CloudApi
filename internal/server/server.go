package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/backup"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/dependencies"
	"github.com/looplj/axonhub/internal/server/gc"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/gql/openapi"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/scheduler"
	"github.com/looplj/axonhub/internal/server/video_storage"
	"github.com/looplj/axonhub/internal/tracing"
)

func New(config Config) (*Server, error) {
	if !config.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	engine := gin.New()
	// The trailing-slash redirect is answered by the router before any
	// middleware runs, so a redirected request never reaches WithCORS and a
	// browser sees an opaque CORS failure instead of following the redirect.
	// Unmatched paths fall through to the SPA fallback on NoRoute instead,
	// which returns a JSON 404 for API prefixes and index.html otherwise.
	engine.RedirectTrailingSlash = false

	if err := engine.SetTrustedProxies(config.TrustedProxies); err != nil {
		return nil, fmt.Errorf("configure trusted proxies: %w", err)
	}
	engine.Use(middleware.Recovery())

	return &Server{
		Config: config,
		Engine: engine,
	}, nil
}

type Server struct {
	*gin.Engine

	Config Config
	server *http.Server
	addr   string
}

func (srv *Server) Run() error {
	log.Info(context.Background(), "run server",
		log.String("name", srv.Config.Name),
		log.String("host", srv.Config.Host),
		log.Int("port", srv.Config.Port),
	)
	addr := fmt.Sprintf("%s:%d", srv.Config.Host, srv.Config.Port)
	srv.server = &http.Server{
		Addr:         addr,
		Handler:      srv.Engine,
		ReadTimeout:  srv.Config.ReadTimeout,
		WriteTimeout: max(srv.Config.RequestTimeout, srv.Config.LLMRequestTimeout),
	}
	srv.addr = addr

	err := srv.server.ListenAndServe()
	if err != nil {
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}

		return err
	}

	return nil
}

func (srv *Server) Shutdown(ctx context.Context) error {
	return srv.server.Shutdown(ctx)
}

func Run(opts ...fx.Option) {
	constructors := []any{
		openapi.NewGraphqlHandlers,
		gql.NewGraphqlHandlers,
		gc.NewWorker,
		New,
	}

	app := fx.New(
		append([]fx.Option{
			fx.NopLogger,
			fx.Provide(constructors...),
			dependencies.Module,
			scheduler.Module,
			biz.Module,
			orchestrator.Module,
			backup.Module,
			video_storage.Module,
			api.Module,
			fx.Provide(fx.Annotate(func(cfg Config) string { return cfg.PublicURL }, fx.ResultTags(`name:"public_url"`))),
			fx.Invoke(func(cfg log.Config) {
				log.SetGlobalConfig(cfg)
				tracing.SetupLogger(log.GetGlobalLogger())
				slog.SetDefault(log.GetGlobalLogger().AsSlog())
			}),
			fx.Invoke(func(usageLogSvc *biz.UsageLogService) {
				usageLogSvc.OnUsageLogCreated = gql.InvalidateAllTimeTokenStatsCache
			}),
			fx.Invoke(func(cfg Config) {
				if cfg.Dashboard.AllTimeTokenStatsSoftTTL > 0 && cfg.Dashboard.AllTimeTokenStatsHardTTL > 0 {
					gql.SetTokenStatsCacheTTL(cfg.Dashboard.AllTimeTokenStatsSoftTTL, cfg.Dashboard.AllTimeTokenStatsHardTTL)
				}
			}),
			fx.Invoke(func(lc fx.Lifecycle, worker *gc.Worker, s *scheduler.Scheduler) {
				lc.Append(fx.Hook{
					OnStart: func(ctx context.Context) error {
						return worker.RegisterScheduledTasks(ctx, s)
					},
				})
			}),
			fx.Invoke(SetupRoutes),
		}, opts...)...,
	)
	app.Run()
}
