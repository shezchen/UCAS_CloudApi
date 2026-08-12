package dependencies

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zhenzou/executors"
)

type namedTestRunnable struct{ name string }

func (r namedTestRunnable) Run(context.Context) {}
func (r namedTestRunnable) Name() string        { return r.name }

func TestRunnableName(t *testing.T) {
	require.Equal(t, "video-storage", runnableName(namedTestRunnable{name: "video-storage"}))

	// A bare closure carries no identity, so the type name is all there is.
	require.Equal(t, "executors.RunnableFunc", runnableName(executors.RunnableFunc(func(context.Context) {})))
}

func TestRejectionHandler_PropagatesTheRejection(t *testing.T) {
	// Returning nil would make Execute report success for a task that never ran.
	err := (&RejectionHandler{}).RejectExecution(namedTestRunnable{name: "gc"}, nil)
	require.ErrorIs(t, err, executors.ErrRejectedExecution)
}
