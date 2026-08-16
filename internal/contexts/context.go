package contexts

import (
	"context"
	"slices"

	"github.com/looplj/axonhub/internal/ent"
)

// ContextKey defines the context key type.
type ContextKey string

const (
	// containerContextKey is used to store the context container in the context.
	containerContextKey ContextKey = "context_container"
)

// WithAPIKey stores the API key entity in the context.
//
// WARNING: this writes into the container already stored in ctx (in place),
// so the new identity is visible to every context derived from the same
// container — including the original request context. To impersonate a
// principal for a nested or scoped execution, first clone the container with
// WithIsolatedContainer (or DetachForAsync for background work).
func WithAPIKey(ctx context.Context, apiKey *ent.APIKey) context.Context {
	container := getContainer(ctx)
	container.mu.Lock()
	container.APIKey = apiKey
	container.mu.Unlock()

	return withContainer(ctx, container)
}

// GetAPIKey retrieves the API key entity from the context.
func GetAPIKey(ctx context.Context) (*ent.APIKey, bool) {
	container := getContainer(ctx)
	container.mu.RLock()
	defer container.mu.RUnlock()
	return container.APIKey, container.APIKey != nil
}

// GetAPIKeyString retrieves the API key string from the context (for backward compatibility).
func GetAPIKeyString(ctx context.Context) (string, bool) {
	apiKey, ok := GetAPIKey(ctx)
	if !ok || apiKey == nil {
		return "", false
	}

	return apiKey.Key, true
}

// WithUser stores the user entity in the context.
//
// WARNING: this writes into the container already stored in ctx (in place),
// so the new identity is visible to every context derived from the same
// container — including the original request context. To impersonate a
// principal for a nested or scoped execution, first clone the container with
// WithIsolatedContainer (or DetachForAsync for background work). For
// unauthenticated flows that need elevated database access, prefer
// authz.RunWithSystemBypass over injecting a synthetic user here.
func WithUser(ctx context.Context, user *ent.User) context.Context {
	container := getContainer(ctx)
	container.mu.Lock()
	container.User = user
	container.mu.Unlock()

	return withContainer(ctx, container)
}

// GetUser retrieves the user entity from the context.
func GetUser(ctx context.Context) (*ent.User, bool) {
	container := getContainer(ctx)
	container.mu.RLock()
	defer container.mu.RUnlock()
	return container.User, container.User != nil
}

// WithTraceID stores the trace id in the context.
func WithTraceID(ctx context.Context, traceID string) context.Context {
	container := getContainer(ctx)
	container.mu.Lock()
	container.TraceID = &traceID
	container.mu.Unlock()

	return withContainer(ctx, container)
}

// GetTraceID retrieves the trace id from the context.
func GetTraceID(ctx context.Context) (string, bool) {
	container := getContainer(ctx)
	container.mu.RLock()
	defer container.mu.RUnlock()
	if container.TraceID != nil {
		return *container.TraceID, true
	}

	return "", false
}

// WithOperationName stores the operation name in the context.
func WithOperationName(ctx context.Context, name string) context.Context {
	container := getContainer(ctx)
	container.mu.Lock()
	container.OperationName = &name
	container.mu.Unlock()

	return withContainer(ctx, container)
}

// GetOperationName retrieves the operation name from the context.
func GetOperationName(ctx context.Context) (string, bool) {
	container := getContainer(ctx)
	container.mu.RLock()
	defer container.mu.RUnlock()
	if container.OperationName != nil {
		return *container.OperationName, true
	}

	return "", false
}

// WithRequestID stores the request id in the context.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	container := getContainer(ctx)
	container.mu.Lock()
	container.RequestID = &requestID
	container.mu.Unlock()

	return withContainer(ctx, container)
}

// GetRequestID retrieves the request id from the context.
func GetRequestID(ctx context.Context) (string, bool) {
	container := getContainer(ctx)
	container.mu.RLock()
	defer container.mu.RUnlock()
	if container.RequestID != nil {
		return *container.RequestID, true
	}

	return "", false
}

// WithChannelAPIKey stores the channel API key in the context.
func WithChannelAPIKey(ctx context.Context, apiKey string) context.Context {
	container := getContainer(ctx)
	container.mu.Lock()
	container.ChannelAPIKey = &apiKey
	container.mu.Unlock()

	return withContainer(ctx, container)
}

// GetChannelAPIKey retrieves the channel API key from the context.
func GetChannelAPIKey(ctx context.Context) (string, bool) {
	container := getContainer(ctx)
	container.mu.RLock()
	defer container.mu.RUnlock()
	if container.ChannelAPIKey != nil {
		return *container.ChannelAPIKey, true
	}

	return "", false
}

// WithProjectID stores the project ID in the context.
func WithProjectID(ctx context.Context, projectID int) context.Context {
	container := getContainer(ctx)
	container.mu.Lock()
	container.ProjectID = &projectID
	container.mu.Unlock()

	return withContainer(ctx, container)
}

// GetProjectID retrieves the project ID from the context.
func GetProjectID(ctx context.Context) (int, bool) {
	container := getContainer(ctx)
	container.mu.RLock()
	defer container.mu.RUnlock()
	if container.ProjectID != nil {
		return *container.ProjectID, true
	}

	return 0, false
}

// AddError appends an error to the context's error list.
// Will do nothing if the context is not initialized.
// But in real world, it should be initialized.
func AddError(ctx context.Context, err error) {
	if err == nil {
		return
	}

	container := getContainer(ctx)

	container.mu.Lock()
	defer container.mu.Unlock()

	container.Errors = append(container.Errors, err)
}

// GetErrors retrieves all errors from the context.
// Will return nil if the context is not initialized.
// But in real world, it should be initialized.
func GetErrors(ctx context.Context) []error {
	container := getContainer(ctx)

	container.mu.RLock()
	defer container.mu.RUnlock()

	return slices.Clone(container.Errors)
}
