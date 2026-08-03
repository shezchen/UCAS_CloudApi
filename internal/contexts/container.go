package contexts

import (
	"context"
	"sync"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
)

// contextContainer contains all values in the context.
type contextContainer struct {
	ProjectID     *int
	TraceID       *string
	RequestID     *string
	OperationName *string
	APIKey        *ent.APIKey
	User          *ent.User
	Source        *request.Source
	Thread        *ent.Thread
	Trace         *ent.Trace
	Errors        []error
	mu            sync.RWMutex

	// ChannelAPIKey stores the API key used for the channel request (not the user's API key)
	ChannelAPIKey *string
}

// getContainer retrieves the existing container from context, or creates a new one and stores it in the context if it doesn't exist.
func getContainer(ctx context.Context) *contextContainer {
	if container, ok := ctx.Value(containerContextKey).(*contextContainer); ok {
		return container
	}

	// If container doesn't exist, create a new one and store it in the context
	container := &contextContainer{}

	return container
}

// withContainer stores the container in the context (if not already stored).
func withContainer(ctx context.Context, container *contextContainer) context.Context {
	if ctx.Value(containerContextKey) == nil {
		return context.WithValue(ctx, containerContextKey, container)
	}

	return ctx
}

// DetachForAsync preserves request identity and authorization values while
// installing a private mutable container. This is required before background
// work: context.WithoutCancel alone would retain the same container, allowing a
// provider API-key selection in the background to overwrite the production
// request's selected credential (and vice versa).
//
// ChannelAPIKey and Errors are intentionally not copied. They describe one
// concrete execution path and must never cross the production/background
// boundary.
func DetachForAsync(ctx context.Context) context.Context {
	return cloneContainerInto(context.WithoutCancel(ctx), ctx)
}

// WithIsolatedContainer keeps cancellation/deadline semantics but gives a
// nested execution (for example TestChannel) its own mutable values.
func WithIsolatedContainer(ctx context.Context) context.Context {
	return cloneContainerInto(ctx, ctx)
}

func cloneContainerInto(base context.Context, sourceContext context.Context) context.Context {
	source := getContainer(sourceContext)
	source.mu.RLock()
	detached := &contextContainer{
		ProjectID:     cloneValue(source.ProjectID),
		TraceID:       cloneValue(source.TraceID),
		RequestID:     cloneValue(source.RequestID),
		OperationName: cloneValue(source.OperationName),
		APIKey:        source.APIKey,
		User:          source.User,
		Source:        cloneValue(source.Source),
		Thread:        source.Thread,
		Trace:         source.Trace,
	}
	source.mu.RUnlock()

	return context.WithValue(base, containerContextKey, detached)
}

func cloneValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
