package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhenzou/executors"
)

// captureExecutor records what the scheduler hands to the executor.
type captureExecutor struct {
	executors.ScheduledExecutor

	fixRate []executors.Runnable
	cron    []executors.Runnable
}

func (e *captureExecutor) ScheduleAtFixRate(r executors.Runnable, _ time.Duration) (executors.CancelFunc, error) {
	e.fixRate = append(e.fixRate, r)
	return func() {}, nil
}

func (e *captureExecutor) ScheduleAtCronRate(r executors.Runnable, _ executors.CRONRule) (executors.CancelFunc, error) {
	e.cron = append(e.cron, r)
	return func() {}, nil
}

func TestScheduler_SubmitsNamedRunnables(t *testing.T) {
	exec := &captureExecutor{}
	s := New(exec)
	ctx := context.Background()

	require.NoError(t, s.Register(ctx, TaskSpec{Name: "gc", CronExpr: "0 2 * * *"}, func(context.Context) {}))
	require.NoError(t, s.Register(ctx, TaskSpec{Name: "video-storage", FixRate: time.Minute}, func(context.Context) {}))

	require.Len(t, exec.cron, 1)
	require.Len(t, exec.fixRate, 1)

	// The shared error and rejection handlers can only name the task if the
	// scheduler passes a Runnable that carries the name.
	require.Equal(t, "gc", exec.cron[0].(NamedRunnable).Name())
	require.Equal(t, "video-storage", exec.fixRate[0].(NamedRunnable).Name())
}

func TestNamedRunnable_RunsTheTask(t *testing.T) {
	var gotName string

	runnable := NamedRunnable{
		name: "backup",
		fn:   func(context.Context) { gotName = "backup" },
	}

	runnable.Run(context.Background())

	require.Equal(t, "backup", gotName)
	require.Equal(t, "backup", runnable.Name())
}
