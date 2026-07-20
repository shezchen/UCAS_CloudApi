package middleware

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
)

const (
	// UserConcurrencyLimit is the fixed V1 request concurrency limit shared by
	// all API keys that belong to the same user.
	UserConcurrencyLimit = 4

	// UserConcurrencyLimitExceededCode is returned to clients when a user's
	// account-wide concurrency limit has been reached.
	UserConcurrencyLimitExceededCode = "concurrency_limit_exceeded"
)

var errUserConcurrencyLimitExceeded = fmt.Errorf(
	"user concurrency limit exceeded: maximum %d in-flight requests",
	UserConcurrencyLimit,
)

// UserConcurrencyLimiter tracks in-flight requests per API-key owner. It is
// intentionally process-local for the single-instance V1 deployment.
type UserConcurrencyLimiter struct {
	mu       sync.Mutex
	inFlight map[int]int
}

func NewUserConcurrencyLimiter() *UserConcurrencyLimiter {
	return &UserConcurrencyLimiter{
		inFlight: make(map[int]int),
	}
}

// TryAcquire reserves a slot without waiting. A non-positive user ID denotes a
// system/service key without an owner and is deliberately left unrestricted.
func (l *UserConcurrencyLimiter) TryAcquire(userID int) bool {
	if userID <= 0 {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.inFlight[userID] >= UserConcurrencyLimit {
		return false
	}

	l.inFlight[userID]++
	return true
}

// Release returns one slot and removes idle users from the map.
func (l *UserConcurrencyLimiter) Release(userID int) {
	if userID <= 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	remaining := l.inFlight[userID] - 1
	if remaining <= 0 {
		delete(l.inFlight, userID)
		return
	}

	l.inFlight[userID] = remaining
}

// WithUserConcurrencyLimit applies the account-wide limit after API-key
// authentication has populated the request context. Safe read/preflight methods
// do not consume a slot. The defer intentionally spans c.Next so streaming
// handlers retain their slot until the response handler fully returns.
func WithUserConcurrencyLimit(limiter *UserConcurrencyLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}

		apiKey, ok := contexts.GetAPIKey(c.Request.Context())
		if !ok || apiKey.UserID <= 0 {
			c.Next()
			return
		}

		if !limiter.TryAcquire(apiKey.UserID) {
			_ = c.Error(errUserConcurrencyLimitExceeded)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, objects.ErrorResponse{
				Error: objects.Error{
					Type:    "rate_limit_error",
					Message: errUserConcurrencyLimitExceeded.Error(),
					Code:    UserConcurrencyLimitExceededCode,
				},
			})
			return
		}

		defer limiter.Release(apiKey.UserID)
		c.Next()
	}
}
