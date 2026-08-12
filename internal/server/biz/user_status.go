package biz

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/looplj/axonhub/internal/ent/user"
)

// Every LLM request validates the status of the API key owner, so that lookup
// must not load the whole user aggregate. The status is held in a dedicated
// in-process cache instead of the shared user cache: it backs an access check,
// so it must not silently disappear when no cache is configured (xcache falls
// back to a noop), and its own short TTL bounds how long another replica keeps
// serving a deactivated owner. Status changes invalidate the entry directly.
const (
	userStatusCacheTTL      = 30 * time.Second
	userStatusPruneInterval = 5 * time.Minute
)

type userStatusEntry struct {
	status    user.Status
	expiresAt time.Time
}

type userStatusCache struct {
	mu sync.Mutex

	now     func() time.Time
	entries map[int]userStatusEntry

	lastPrune time.Time
}

func newUserStatusCache(now func() time.Time) *userStatusCache {
	return &userStatusCache{
		now:       now,
		entries:   make(map[int]userStatusEntry),
		lastPrune: now(),
	}
}

func (c *userStatusCache) get(id int) (user.Status, bool) {
	if c == nil {
		return "", false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[id]
	if !ok || !entry.expiresAt.After(c.now()) {
		return "", false
	}

	return entry.status, true
}

func (c *userStatusCache) set(id int, status user.Status) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	c.pruneLocked(now)

	c.entries[id] = userStatusEntry{status: status, expiresAt: now.Add(userStatusCacheTTL)}
}

func (c *userStatusCache) invalidate(id int) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, id)
}

func (c *userStatusCache) clear() {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	clear(c.entries)
}

func (c *userStatusCache) pruneLocked(now time.Time) {
	if now.Sub(c.lastPrune) < userStatusPruneInterval {
		return
	}

	c.lastPrune = now

	for id, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, id)
		}
	}
}

// GetUserStatus reads just the status of a user. Callers that only need to
// know whether an account is still usable must prefer it over GetUserByID,
// which eager-loads roles, projects and identities.
func (s *UserService) GetUserStatus(ctx context.Context, id int) (user.Status, error) {
	if status, ok := s.userStatusCache.get(id); ok {
		return status, nil
	}

	client := s.entFromContext(ctx)
	if client == nil {
		return "", fmt.Errorf("ent client not found in context")
	}

	u, err := client.User.Query().
		Where(user.IDEQ(id)).
		Select(user.FieldStatus).
		Only(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get user status: %w", err)
	}

	s.userStatusCache.set(id, u.Status)

	return u.Status, nil
}
