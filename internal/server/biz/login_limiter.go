package biz

import (
	"strings"
	"sync"
	"time"
)

// Sign-in throttling guards password authentication against brute force with
// per-account and per-source (client IP) failure budgets. State is kept in
// process memory on purpose: the platform runs as a single instance by
// default, and a cache-backed limiter would silently degrade to no protection
// when no cache is configured (xcache falls back to a noop implementation).
const (
	signInFailureWindow      = 15 * time.Minute
	signInLockoutDuration    = 15 * time.Minute
	signInMaxAccountFailures = 5
	signInMaxSourceFailures  = 20
	signInPruneInterval      = 5 * time.Minute
)

type signInAttemptState struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

type signInLimiter struct {
	mu        sync.Mutex
	now       func() time.Time
	accounts  map[string]*signInAttemptState
	sources   map[string]*signInAttemptState
	lastPrune time.Time
}

func newSignInLimiter(now func() time.Time) *signInLimiter {
	return &signInLimiter{
		now:       now,
		accounts:  make(map[string]*signInAttemptState),
		sources:   make(map[string]*signInAttemptState),
		lastPrune: now(),
	}
}

func normalizeSignInAccount(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// check reports whether a sign-in attempt for the account/source pair is
// currently allowed.
func (l *signInLimiter) check(email, source string) error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneLocked(now)

	if isSignInLocked(l.accounts[normalizeSignInAccount(email)], now) ||
		isSignInLocked(l.sources[source], now) {
		return ErrTooManyLoginAttempts
	}

	return nil
}

// recordFailure counts a failed credential check and locks the account/source
// once its budget inside the rolling window is exhausted.
func (l *signInLimiter) recordFailure(email, source string) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	recordSignInFailureLocked(l.accounts, normalizeSignInAccount(email), signInMaxAccountFailures, now)
	recordSignInFailureLocked(l.sources, source, signInMaxSourceFailures, now)
}

// recordSuccess clears the failure history of the account. The source counter
// is kept so one valid account cannot reset an attacker's source budget.
func (l *signInLimiter) recordSuccess(email string) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.accounts, normalizeSignInAccount(email))
}

func isSignInLocked(state *signInAttemptState, now time.Time) bool {
	return state != nil && state.lockedUntil.After(now)
}

func recordSignInFailureLocked(states map[string]*signInAttemptState, key string, maxFailures int, now time.Time) {
	if key == "" {
		return
	}

	state := states[key]
	if state == nil || now.Sub(state.windowStart) > signInFailureWindow {
		state = &signInAttemptState{windowStart: now}
		states[key] = state
	}

	state.failures++
	if state.failures >= maxFailures {
		state.lockedUntil = now.Add(signInLockoutDuration)
	}
}

func (l *signInLimiter) pruneLocked(now time.Time) {
	if now.Sub(l.lastPrune) < signInPruneInterval {
		return
	}

	l.lastPrune = now

	for _, states := range []map[string]*signInAttemptState{l.accounts, l.sources} {
		for key, state := range states {
			if now.Sub(state.windowStart) > signInFailureWindow && !state.lockedUntil.After(now) {
				delete(states, key)
			}
		}
	}
}

// CheckSignInThrottle rejects a sign-in attempt with ErrTooManyLoginAttempts
// when the account or the client source is locked out after repeated failures.
func (s *AuthService) CheckSignInThrottle(email, source string) error {
	return s.signInLimiter.check(email, source)
}

// RecordSignInFailure counts a failed credential check towards the lockout.
func (s *AuthService) RecordSignInFailure(email, source string) {
	s.signInLimiter.recordFailure(email, source)
}

// RecordSignInSuccess resets the account failure budget after a successful
// sign-in.
func (s *AuthService) RecordSignInSuccess(email string) {
	s.signInLimiter.recordSuccess(email)
}
