package dependencies

import (
	"context"
	"errors"
	"fmt"

	"github.com/zhenzou/executors"

	"github.com/looplj/axonhub/internal/log"
)

// runnableName reports the scheduled task's name when the runnable carries
// one. A bare closure reaches the executor as an anonymous
// executors.RunnableFunc, whose type name identifies nothing, so scheduler
// tasks implement Name (see scheduler.NamedRunnable).
func runnableName(runnable executors.Runnable) string {
	if named, ok := runnable.(interface{ Name() string }); ok {
		return named.Name()
	}

	return fmt.Sprintf("%T", runnable)
}

type ErrorHandler struct{}

func (h *ErrorHandler) CatchError(runnable executors.Runnable, err error) {
	if errors.Is(err, executors.ErrRejectedExecution) {
		log.Error(context.Background(), "task rejected by executor, queue is full",
			log.String("runnable", runnableName(runnable)))

		return
	}

	log.Error(context.Background(), "run runnable error",
		log.String("runnable", runnableName(runnable)),
		log.Cause(err))
}

type RejectionHandler struct{}

// RejectExecution propagates the rejection instead of swallowing it: returning
// nil would make Execute report success for a task that never ran. The drop is
// logged by CatchError, which every scheduled submission routes through, so
// logging here too would report each rejection twice.
func (h *RejectionHandler) RejectExecution(runnable executors.Runnable, e executors.Executor) error {
	return executors.ErrRejectedExecution
}

func NewExecutors(logger *log.Logger) executors.ScheduledExecutor {
	return executors.NewPoolScheduleExecutor(
		executors.WithMaxConcurrent(64),
		executors.WithMaxBlockingTasks(1024),
		executors.WithErrorHandler(&ErrorHandler{}),
		executors.WithRejectionHandler(&RejectionHandler{}),
		executors.WithLogger(logger.AsSlog()),
	)
}
