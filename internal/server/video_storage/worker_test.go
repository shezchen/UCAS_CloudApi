package video_storage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

func setupVideoWorker(t *testing.T) (*Worker, context.Context, *ent.Client, *ent.DataStorage, string) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { client.Close() })

	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	systemService := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cacheConfig, Ent: client})
	dataStorageService := biz.NewDataStorageService(biz.DataStorageServiceParams{
		SystemService: systemService,
		CacheConfig:   cacheConfig,
		Client:        client,
	})

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	dir := t.TempDir()
	dirCopy := dir

	ds, err := client.DataStorage.Create().
		SetName("fs-storage").
		SetDescription("test fs storage").
		SetPrimary(false).
		SetType(datastorage.TypeFs).
		SetSettings(&objects.DataStorageSettings{Directory: &dirCopy}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	worker := &Worker{
		ent:                client,
		systemService:      systemService,
		dataStorageService: dataStorageService,
	}

	return worker, ctx, client, ds, dir
}

func createVideoRequest(t *testing.T, ctx context.Context, client *ent.Client, videoURL string) *ent.Request {
	t.Helper()

	projectRow, err := client.Project.Create().SetName("video-project").Save(ctx)
	require.NoError(t, err)

	body, err := json.Marshal(llm.VideoResponse{ID: "vid_1", Status: "succeeded", VideoURL: videoURL})
	require.NoError(t, err)

	req, err := client.Request.Create().
		SetProjectID(projectRow.ID).
		SetModelID("sora-2").
		SetFormat(string(llm.APIFormatOpenAIVideo)).
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		SetResponseBody(objects.JSONRawMessage(body)).
		Save(ctx)
	require.NoError(t, err)

	return req
}

func shrinkArchiveCap(t *testing.T, maxBytes int64) {
	t.Helper()

	original := maxVideoArchiveBytes
	maxVideoArchiveBytes = maxBytes

	t.Cleanup(func() { maxVideoArchiveBytes = original })
}

// videoServer serves body and counts how many times it was fetched. When
// declareLength is false it responds chunked, so the size is only discoverable
// by reading the body.
func videoServer(t *testing.T, body []byte, declareLength bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		if declareLength {
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		}

		w.Header().Set("Content-Disposition", `attachment; filename="clip.mp4"`)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	return server, &hits
}

func storedFiles(t *testing.T, baseDir string) []string {
	t.Helper()

	var files []string

	require.NoError(t, filepath.Walk(baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			files = append(files, strings.TrimPrefix(path, baseDir))
		}

		return nil
	}))

	return files
}

func TestWorker_processOne_ArchivesVideoWithinCap(t *testing.T) {
	worker, ctx, client, ds, dir := setupVideoWorker(t)
	shrinkArchiveCap(t, 32)

	server, _ := videoServer(t, []byte("small video body"), true)
	req := createVideoRequest(t, ctx, client, server.URL+"/clip.mp4")

	require.NoError(t, worker.processOne(ctx, ds, req))

	stored, err := client.Request.Get(ctx, req.ID)
	require.NoError(t, err)
	require.True(t, stored.ContentSaved)
	require.NotNil(t, stored.ContentStorageKey)
	require.Equal(t, GenerateVideoKey(req.ProjectID, req.ID, "clip.mp4"), *stored.ContentStorageKey)
	require.NotNil(t, stored.ContentStorageID)
	require.Len(t, storedFiles(t, dir), 1)
}

func TestWorker_processOne_RetiresOversizedVideoByContentLength(t *testing.T) {
	worker, ctx, client, ds, dir := setupVideoWorker(t)
	shrinkArchiveCap(t, 8)

	server, hits := videoServer(t, []byte("this body is well past the cap"), true)
	req := createVideoRequest(t, ctx, client, server.URL+"/clip.mp4")

	require.NoError(t, worker.processOne(ctx, ds, req))

	stored, err := client.Request.Get(ctx, req.ID)
	require.NoError(t, err)
	require.True(t, stored.ContentSaved, "an unarchivable video must leave the scan queue")
	require.Nil(t, stored.ContentStorageKey, "no key must be published for content that was never stored")
	require.Nil(t, stored.ContentStorageID)
	require.NotNil(t, stored.ContentSavedAt)
	require.Empty(t, storedFiles(t, dir), "an advertised oversize must not be written to storage at all")
	require.Equal(t, int64(1), hits.Load())
}

func TestWorker_processOne_RetiresOversizedVideoWithoutContentLength(t *testing.T) {
	worker, ctx, client, ds, dir := setupVideoWorker(t)
	shrinkArchiveCap(t, 8)

	server, _ := videoServer(t, []byte("this body is well past the cap"), false)
	req := createVideoRequest(t, ctx, client, server.URL+"/clip.mp4")

	require.NoError(t, worker.processOne(ctx, ds, req))

	stored, err := client.Request.Get(ctx, req.ID)
	require.NoError(t, err)
	require.True(t, stored.ContentSaved)
	require.Nil(t, stored.ContentStorageKey)
	require.Nil(t, stored.ContentStorageID)
	require.Empty(t, storedFiles(t, dir), "the truncated upload must be removed again")
}

func TestWorker_scanAndSave_DoesNotRedownloadRetiredVideos(t *testing.T) {
	worker, ctx, client, ds, _ := setupVideoWorker(t)
	shrinkArchiveCap(t, 8)

	server, hits := videoServer(t, []byte("this body is well past the cap"), true)
	req := createVideoRequest(t, ctx, client, server.URL+"/clip.mp4")

	require.NoError(t, worker.systemService.SetVideoStorageSettings(ctx, biz.VideoStorageSettings{
		Enabled:             true,
		DataStorageID:       ds.ID,
		ScanIntervalMinutes: 1,
		ScanLimit:           50,
	}))

	for range 3 {
		require.NoError(t, worker.scanAndSave(ctx))
	}

	require.Equal(t, int64(1), hits.Load(), "an oversized video must be fetched once, not once per scan")

	pending, err := client.Request.Query().Where(request.ContentSaved(false)).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, pending, "the retired video must not block the scan window")

	stored, err := client.Request.Get(ctx, req.ID)
	require.NoError(t, err)
	require.True(t, stored.ContentSaved)
	require.WithinDuration(t, time.Now(), *stored.ContentSavedAt, time.Minute)
}
