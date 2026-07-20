package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
)

const (
	testUserIDHeader = "X-Test-User-ID"
	testKeyIDHeader  = "X-Test-Key-ID"
)

func withTestAPIKeyContext() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, _ := strconv.Atoi(c.GetHeader(testUserIDHeader))
		keyID, _ := strconv.Atoi(c.GetHeader(testKeyIDHeader))
		ctx := contexts.WithAPIKey(c.Request.Context(), &ent.APIKey{
			ID:     keyID,
			UserID: userID,
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func performUserConcurrencyRequest(router http.Handler, method, path string, userID, keyID int) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set(testUserIDHeader, strconv.Itoa(userID))
	req.Header.Set(testKeyIDHeader, strconv.Itoa(keyID))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	return recorder
}

func waitForConcurrencySignal(t *testing.T, signals <-chan struct{}) {
	t.Helper()

	select {
	case <-signals:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request to enter handler")
	}
}

func TestWithUserConcurrencyLimit_SharedAcrossKeysAndHeldThroughStream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	limiter := NewUserConcurrencyLimiter()
	router := gin.New()
	router.Use(withTestAPIKeyContext())
	router.Use(WithUserConcurrencyLimit(limiter))

	started := make(chan struct{}, UserConcurrencyLimit)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(releaseAll)
	router.POST("/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		c.Writer.Flush()
		started <- struct{}{}
		<-release
		_, _ = c.Writer.Write([]byte("data: done\n\n"))
	})
	router.Any("/fast", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	const userID = 41
	completed := make(chan int, UserConcurrencyLimit)
	for i := range UserConcurrencyLimit {
		go func(keyID int) {
			resp := performUserConcurrencyRequest(router, http.MethodPost, "/stream", userID, keyID)
			completed <- resp.Code
		}(100 + i)
	}

	for range UserConcurrencyLimit {
		waitForConcurrencySignal(t, started)
	}

	// A fifth key belonging to the same user is rejected without entering the
	// handler, even though all four occupied slots use different API keys.
	limited := performUserConcurrencyRequest(router, http.MethodPost, "/fast", userID, 999)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("expected status %d, got %d", http.StatusTooManyRequests, limited.Code)
	}

	var errorResponse objects.ErrorResponse
	if err := json.Unmarshal(limited.Body.Bytes(), &errorResponse); err != nil {
		t.Fatalf("decode concurrency error: %v", err)
	}
	if errorResponse.Error.Code != UserConcurrencyLimitExceededCode {
		t.Fatalf("expected error code %q, got %q", UserConcurrencyLimitExceededCode, errorResponse.Error.Code)
	}

	// A different user has an independent pool.
	otherUser := performUserConcurrencyRequest(router, http.MethodPost, "/fast", userID+1, 1000)
	if otherUser.Code != http.StatusNoContent {
		t.Fatalf("expected other user status %d, got %d", http.StatusNoContent, otherUser.Code)
	}

	// Safe methods never consume or require a slot, even while the account is
	// saturated by four streaming requests.
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		resp := performUserConcurrencyRequest(router, method, "/fast", userID, 999)
		if resp.Code != http.StatusNoContent {
			t.Fatalf("expected %s status %d, got %d", method, http.StatusNoContent, resp.Code)
		}
	}

	releaseAll()
	for range UserConcurrencyLimit {
		if status := <-completed; status != http.StatusOK {
			t.Fatalf("expected streaming status %d, got %d", http.StatusOK, status)
		}
	}

	// Returning from all streaming handlers releases the four account slots.
	afterRelease := performUserConcurrencyRequest(router, http.MethodPost, "/fast", userID, 1001)
	if afterRelease.Code != http.StatusNoContent {
		t.Fatalf("expected status %d after release, got %d", http.StatusNoContent, afterRelease.Code)
	}
}

func TestWithUserConcurrencyLimit_ReleasesAfterPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	limiter := NewUserConcurrencyLimiter()
	router := gin.New()
	router.Use(Recovery())
	router.Use(withTestAPIKeyContext())
	router.Use(WithUserConcurrencyLimit(limiter))
	router.POST("/panic", func(c *gin.Context) {
		panic("handler failed")
	})
	router.POST("/ok", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	const userID = 52
	for i := range UserConcurrencyLimit {
		resp := performUserConcurrencyRequest(router, http.MethodPost, "/panic", userID, 200+i)
		if resp.Code != http.StatusInternalServerError {
			t.Fatalf("expected panic status %d, got %d", http.StatusInternalServerError, resp.Code)
		}
	}

	// Four leaked panic slots would reject this request. A successful response
	// proves the middleware's defer released on every exceptional exit.
	resp := performUserConcurrencyRequest(router, http.MethodPost, "/ok", userID, 300)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("expected status %d after panic releases, got %d", http.StatusNoContent, resp.Code)
	}
}

func TestWithUserConcurrencyLimit_UnownedKeysBypassLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	limiter := NewUserConcurrencyLimiter()
	router := gin.New()
	router.Use(withTestAPIKeyContext())
	router.Use(WithUserConcurrencyLimit(limiter))

	started := make(chan struct{}, UserConcurrencyLimit+1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(releaseAll)
	router.POST("/request", func(c *gin.Context) {
		started <- struct{}{}
		<-release
		c.Status(http.StatusNoContent)
	})

	completed := make(chan int, UserConcurrencyLimit+1)
	for i := range UserConcurrencyLimit + 1 {
		go func(keyID int) {
			resp := performUserConcurrencyRequest(router, http.MethodPost, "/request", 0, keyID)
			completed <- resp.Code
		}(400 + i)
	}

	for range UserConcurrencyLimit + 1 {
		waitForConcurrencySignal(t, started)
	}

	releaseAll()
	for range UserConcurrencyLimit + 1 {
		if status := <-completed; status != http.StatusNoContent {
			t.Fatalf("expected unowned key status %d, got %d", http.StatusNoContent, status)
		}
	}
}
