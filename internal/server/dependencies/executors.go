package dependencies

import (
	"context"
	"fmt"

	"github.com/zhenzou/executors"

	"github.com/looplj/axonhub/internal/log"
)

type ErrorHandler struct{}

func (h *ErrorHandler) CatchError(runnable executors.Runnable, err error) {
	log.Error(context.Background(), "run runnable error",
		log.String("runnable_type", fmt.Sprintf("%T", runnable)),
		log.Cause(err))
}

type RejectionHandler struct{}

// RejectExecution logs the dropped task's identity and propagates the
// rejection. Returning nil here would make Execute report success for a task
// that never ran; returning ErrRejectedExecution lets callers (and the
// scheduler's ErrorHandler path) observe the drop.
func (h *RejectionHandler) RejectExecution(runnable executors.Runnable, e executors.Executor) error {
	log.Error(context.Background(), "task rejected by executor, queue is full",
		log.String("runnable_type", fmt.Sprintf("%T", runnable)))

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
