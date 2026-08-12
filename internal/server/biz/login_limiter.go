package biz

import (
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

// Sign-in throttling guards password authentication against brute force.
//
// A hard lockout is only ever applied to a key that carries an attributable
// client address. Anyone can name someone else's account, so locking the
// account alone would be a remote account-denial primitive; and a client
// address that resolves to a shared reverse proxy stands for every user at
// once, so locking it would take the whole deployment down. The account
// dimension is throttled with an escalating delay on the failure response
// instead: it costs an attacker time on every guess while a correct credential
// is never withheld.
//
// State is kept in process memory on purpose: a cache-backed limiter would
// silently degrade to no protection when no cache is configured (xcache falls
// back to a noop implementation), and rate limiting a client across replicas
// belongs to the reverse proxy.
const (
	signInFailureWindow   = 15 * time.Minute
	signInLockoutDuration = 15 * time.Minute

	// Budgets for the dimensions that carry an attributable client address.
	signInMaxClientAccountFailures = 5
	signInMaxClientFailures        = 20

	// The account dimension only escalates the delay of failed sign-ins.
	signInDelayAfterFailures = 3
	signInBaseFailureDelay   = 200 * time.Millisecond
	signInMaxFailureDelay    = 2 * time.Second

	signInPruneInterval = 5 * time.Minute

	// Every key an unauthenticated caller can invent is bounded, both in size
	// (keys are digests) and in number.
	signInMaxTrackedKeys = 20_000
	signInEvictionSample = 64
)

type signInAttemptState struct {
	failures    int
	windowStart time.Time
	lockedUntil time.Time
}

type signInLimiter struct {
	mu sync.Mutex

	now func() time.Time

	// accounts counts failures per account and never locks out.
	accounts map[string]*signInAttemptState
	// clients counts failures per attributable client address.
	clients map[string]*signInAttemptState
	// clientAccounts counts failures per (account, client address) pair.
	clientAccounts map[string]*signInAttemptState

	lastPrune time.Time
}

func newSignInLimiter(now func() time.Time) *signInLimiter {
	return &signInLimiter{
		now:            now,
		accounts:       make(map[string]*signInAttemptState),
		clients:        make(map[string]*signInAttemptState),
		clientAccounts: make(map[string]*signInAttemptState),
		lastPrune:      now(),
	}
}

func normalizeSignInAccount(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// signInAccountKey digests the account so that the amount of memory an
// attempt can pin does not depend on the length of an attacker-supplied
// address. A collision only makes two accounts share a failure counter.
func signInAccountKey(email string) string {
	account := normalizeSignInAccount(email)
	if account == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(account))

	return string(sum[:])
}

func signInClientAccountKey(email, source string) string {
	account := normalizeSignInAccount(email)
	if account == "" || source == "" {
		return ""
	}

	sum := sha256.Sum256([]byte(account + "\x00" + source))

	return string(sum[:])
}

// check reports whether a sign-in attempt is currently allowed. An empty
// source means the client address could not be attributed to a single client
// (see AuthHandlers.signInSource); the per-client dimensions are then skipped
// entirely rather than charging every user that shares the address.
func (l *signInLimiter) check(email, source string) error {
	if l == nil || source == "" {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneLocked(now)

	if isSignInLocked(l.clients[source], now) ||
		isSignInLocked(l.clientAccounts[signInClientAccountKey(email, source)], now) {
		return ErrTooManyLoginAttempts
	}

	return nil
}

// recordFailure counts a failed credential check and returns how long the
// failure response must be withheld.
func (l *signInLimiter) recordFailure(email, source string) time.Duration {
	if l == nil {
		return 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()

	// Pruning happens on the write path as well: check returns early for an
	// unattributable client, so the tables would otherwise only ever shrink
	// once they hit the eviction cap.
	l.pruneLocked(now)

	accountFailures := recordSignInFailureLocked(l.accounts, signInAccountKey(email), 0, now)
	if source != "" {
		recordSignInFailureLocked(l.clients, source, signInMaxClientFailures, now)
		recordSignInFailureLocked(
			l.clientAccounts,
			signInClientAccountKey(email, source),
			signInMaxClientAccountFailures,
			now,
		)
	}

	return signInFailureDelay(accountFailures)
}

// recordSuccess clears the failure history of the account and of the client
// that proved it holds a valid credential. The client-wide counter is kept so
// one valid account cannot reset an attacker's spraying budget.
func (l *signInLimiter) recordSuccess(email, source string) {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	delete(l.accounts, signInAccountKey(email))

	if source != "" {
		delete(l.clientAccounts, signInClientAccountKey(email, source))
	}
}

func isSignInLocked(state *signInAttemptState, now time.Time) bool {
	return state != nil && state.lockedUntil.After(now)
}

// signInFailureDelay escalates the delay of a failed sign-in once the account
// has burnt through its free attempts. It is capped so a flood of guesses
// cannot pin an unbounded number of in-flight requests.
func signInFailureDelay(failures int) time.Duration {
	if failures <= signInDelayAfterFailures {
		return 0
	}

	shift := failures - signInDelayAfterFailures - 1
	if shift > 16 {
		return signInMaxFailureDelay
	}

	delay := signInBaseFailureDelay << shift
	if delay > signInMaxFailureDelay {
		return signInMaxFailureDelay
	}

	return delay
}

// recordSignInFailureLocked counts one failure and returns the failure total
// inside the current window. A maxFailures of zero counts without ever
// locking out.
func recordSignInFailureLocked(
	states map[string]*signInAttemptState,
	key string,
	maxFailures int,
	now time.Time,
) int {
	if key == "" {
		return 0
	}

	state := states[key]

	switch {
	case state == nil:
		if !reserveSignInSlot(states, now) {
			return 0
		}

		state = &signInAttemptState{windowStart: now}
		states[key] = state
	case now.Sub(state.windowStart) > signInFailureWindow:
		// A running lockout survives the window reset; it is released by its
		// own deadline.
		*state = signInAttemptState{windowStart: now, lockedUntil: state.lockedUntil}
	}

	state.failures++
	if maxFailures > 0 && state.failures >= maxFailures {
		state.lockedUntil = now.Add(signInLockoutDuration)
	}

	return state.failures
}

// reserveSignInSlot makes room for one more tracked key. A running lockout is
// never evicted, so flooding the table with invented accounts cannot clear a
// penalty; when everything tracked is locked out the new key is dropped
// instead of growing the table.
func reserveSignInSlot(states map[string]*signInAttemptState, now time.Time) bool {
	if len(states) < signInMaxTrackedKeys {
		return true
	}

	var (
		oldestKey   string
		oldestStart time.Time
		visited     int
	)

	// Map iteration is randomised, so a bounded sample is an unbiased pick.
	for key, state := range states {
		if !state.lockedUntil.After(now) && (oldestKey == "" || state.windowStart.Before(oldestStart)) {
			oldestKey, oldestStart = key, state.windowStart
		}

		visited++
		if visited >= signInEvictionSample {
			break
		}
	}

	if oldestKey == "" {
		return false
	}

	delete(states, oldestKey)

	return true
}

func (l *signInLimiter) pruneLocked(now time.Time) {
	if now.Sub(l.lastPrune) < signInPruneInterval {
		return
	}

	l.lastPrune = now

	for _, states := range []map[string]*signInAttemptState{l.accounts, l.clients, l.clientAccounts} {
		for key, state := range states {
			if now.Sub(state.windowStart) > signInFailureWindow && !state.lockedUntil.After(now) {
				delete(states, key)
			}
		}
	}
}

// CheckSignInThrottle rejects a sign-in attempt with ErrTooManyLoginAttempts
// while the client, or the account for that client, is locked out after
// repeated failures. Source must be empty when the client address cannot be
// attributed to a single client.
func (s *AuthService) CheckSignInThrottle(email, source string) error {
	return s.signInLimiter.check(email, source)
}

// RecordSignInFailure counts a failed credential check and returns how long
// the failure response must be withheld.
func (s *AuthService) RecordSignInFailure(email, source string) time.Duration {
	return s.signInLimiter.recordFailure(email, source)
}

// RecordSignInSuccess resets the failure budgets a valid credential clears.
func (s *AuthService) RecordSignInSuccess(email, source string) {
	s.signInLimiter.recordSuccess(email, source)
}
