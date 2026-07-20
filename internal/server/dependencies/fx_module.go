package dependencies

import (
	"context"
	"fmt"
	"time"

	"github.com/zhenzou/executors"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/db"
	servermail "github.com/looplj/axonhub/internal/server/mail"
	"github.com/looplj/axonhub/llm/httpclient"
)

type unavailableVerificationSender struct {
	cause error
}

func (s *unavailableVerificationSender) SendVerificationCode(context.Context, string, string, time.Duration) error {
	return fmt.Errorf("email sender is unavailable: %w", s.cause)
}

// NewConfiguredVerificationSender keeps non-campus deployments bootable when
// SMTP is absent while still failing the public verification request closed.
func NewConfiguredVerificationSender(config servermail.Config) servermail.VerificationSender {
	sender, err := servermail.NewVerificationSender(config)
	if err != nil {
		log.Warn(context.Background(), "Campus email verification is unavailable", log.Cause(err))
		return &unavailableVerificationSender{cause: err}
	}

	return sender
}

type NewHttpClientParams struct {
	fx.In

	DisableSSLVerify bool `name:"disable_ssl_verify"`
}

func NewHttpClient(params NewHttpClientParams) *httpclient.HttpClient {
	return httpclient.NewHttpClient(httpclient.WithInsecureSkipVerify(params.DisableSSLVerify))
}

var Module = fx.Module("dependencies",
	fx.Provide(log.New),
	fx.Provide(db.NewEntClient),
	fx.Provide(NewHttpClient),
	fx.Provide(NewConfiguredVerificationSender),
	fx.Provide(NewExecutors),
	fx.Invoke(func(lc fx.Lifecycle, executor executors.ScheduledExecutor) {
		lc.Append(fx.Hook{
			OnStop: func(ctx context.Context) error {
				return executor.Shutdown(ctx)
			},
		})
	}),
)
