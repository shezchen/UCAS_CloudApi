package biz

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
)

func createVideoTestProject(t *testing.T, ctx context.Context, client *ent.Client) *ent.Project {
	t.Helper()

	name := uuid.NewString()

	proj, err := client.Project.Create().
		SetName(name).
		SetDescription(name).
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	return proj
}

func createVideoTestTask(t *testing.T, ctx context.Context, client *ent.Client, projectID int, externalID string) *ent.Request {
	t.Helper()

	req, err := client.Request.Create().
		SetProjectID(projectID).
		SetModelID("sora-2").
		SetFormat(string(llm.APIFormatOpenAIVideo)).
		SetStatus(request.StatusProcessing).
		SetExternalID(externalID).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(ctx)
	require.NoError(t, err)

	return req
}

// A task belonging to another project must be indistinguishable from a task
// that does not exist: the provider task IDs these endpoints take are
// guessable, so a different error would leak their existence.
func TestVideoService_FindTaskByExternalIDIsScopedToProject(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	projectA := createVideoTestProject(t, setupCtx, client)
	projectB := createVideoTestProject(t, setupCtx, client)
	task := createVideoTestTask(t, setupCtx, client, projectA.ID, "provider-task-1")

	svc := &VideoService{}

	ownerCtx := contexts.WithProjectID(setupCtx, projectA.ID)
	found, err := svc.findTaskByExternalID(ownerCtx, "provider-task-1")
	require.NoError(t, err)
	require.Equal(t, task.ID, found.ID)

	otherCtx := contexts.WithProjectID(setupCtx, projectB.ID)
	_, crossProjectErr := svc.findTaskByExternalID(otherCtx, "provider-task-1")
	require.Error(t, crossProjectErr)
	require.True(t, ent.IsNotFound(crossProjectErr))

	_, missingErr := svc.findTaskByExternalID(otherCtx, "no-such-task")
	require.Error(t, missingErr)
	require.Equal(t, missingErr.Error(), crossProjectErr.Error())
}

// The project filter is the only thing standing between an API key and every
// other project's tasks, so a context that carries neither a project nor a
// bypass must fail rather than run the query unfiltered.
func TestVideoService_TaskLookupFailsClosedWithoutProject(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	proj := createVideoTestProject(t, setupCtx, client)
	task := createVideoTestTask(t, setupCtx, client, proj.ID, "provider-task-2")

	svc := &VideoService{}

	unscopedCtx := ent.NewContext(context.Background(), client)

	_, err := svc.findTaskByExternalID(unscopedCtx, "provider-task-2")
	require.ErrorIs(t, err, ErrInternal)
	require.Contains(t, err.Error(), "requires a project in context")

	_, _, _, err = svc.loadTask(unscopedCtx, task.ID)
	require.ErrorIs(t, err, ErrInternal)
	require.Contains(t, err.Error(), "requires a project in context")
}

// Background flows such as the video storage worker run under a system bypass
// and legitimately need to see tasks across every project.
func TestVideoService_FindTaskByExternalIDAllowsBypassWithoutProject(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	proj := createVideoTestProject(t, setupCtx, client)
	task := createVideoTestTask(t, setupCtx, client, proj.ID, "provider-task-3")

	svc := &VideoService{}

	workerCtx := authz.WithSystemBypass(ent.NewContext(context.Background(), client), "video-storage-scan")
	found, err := svc.findTaskByExternalID(workerCtx, "provider-task-3")
	require.NoError(t, err)
	require.Equal(t, task.ID, found.ID)
}
