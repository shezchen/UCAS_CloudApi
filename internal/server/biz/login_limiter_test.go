package biz

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stubClock struct {
	now time.Time
}

func (c *stubClock) Now() time.Time {
	return c.now
}

func (c *stubClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func newTestSignInLimiter() (*signInLimiter, *stubClock) {
	clock := &stubClock{now: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)}

	return newSignInLimiter(clock.Now), clock
}

func TestSignInLimiter_LocksClientAccountPair(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	const (
		email  = "victim@example.com"
		source = "198.51.100.10"
	)

	for range signInMaxClientAccountFailures {
		require.NoError(t, limiter.check(email, source))
		limiter.recordFailure(email, source)
	}

	require.ErrorIs(t, limiter.check(email, source), ErrTooManyLoginAttempts)
}

func TestSignInLimiter_LockoutDoesNotDenyOtherClients(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	const (
		email    = "victim@example.com"
		attacker = "198.51.100.10"
		owner    = "203.0.113.7"
	)

	for range signInMaxClientAccountFailures {
		limiter.recordFailure(email, attacker)
	}

	require.ErrorIs(t, limiter.check(email, attacker), ErrTooManyLoginAttempts)
	require.NoError(t, limiter.check(email, owner), "the account owner must not be locked out by someone else's guesses")
}

func TestSignInLimiter_LocksClientAfterSprayingAccounts(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	const source = "198.51.100.11"

	for i := range signInMaxClientFailures {
		require.NoError(t, limiter.check(sprayEmail(i), source))
		limiter.recordFailure(sprayEmail(i), source)
	}

	require.ErrorIs(t, limiter.check("another@example.com", source), ErrTooManyLoginAttempts)
	require.NoError(t, limiter.check("another@example.com", "203.0.113.8"))
}

func TestSignInLimiter_UnattributableSourceNeverLocksOut(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	// An empty source stands for a client address that resolves to a shared
	// reverse proxy: no amount of failures may deny anyone.
	for i := range signInMaxClientFailures * 5 {
		limiter.recordFailure(sprayEmail(i), "")
	}

	for range signInMaxClientAccountFailures * 5 {
		limiter.recordFailure("victim@example.com", "")
	}

	require.NoError(t, limiter.check("victim@example.com", ""))
	require.NoError(t, limiter.check(sprayEmail(0), ""))
	require.Empty(t, limiter.clients)
	require.Empty(t, limiter.clientAccounts)
}

func TestSignInLimiter_EscalatesDelayOnAccountFailures(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	const email = "victim@example.com"

	delays := make([]time.Duration, 0, 8)
	for range 8 {
		delays = append(delays, limiter.recordFailure(email, ""))
	}

	require.Equal(t, []time.Duration{
		0,
		0,
		0,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		signInMaxFailureDelay,
	}, delays)

	// The delay is capped so failures cannot pin unbounded in-flight requests.
	for range 20 {
		require.Equal(t, signInMaxFailureDelay, limiter.recordFailure(email, ""))
	}
}

func TestSignInLimiter_WindowResetClearsFailures(t *testing.T) {
	limiter, clock := newTestSignInLimiter()

	const (
		email  = "victim@example.com"
		source = "198.51.100.12"
	)

	for range signInMaxClientAccountFailures - 1 {
		limiter.recordFailure(email, source)
	}

	clock.advance(signInFailureWindow + time.Second)
	require.NoError(t, limiter.check(email, source))

	require.Zero(t, limiter.recordFailure(email, source), "the account delay restarts with the window")
	require.NoError(t, limiter.check(email, source), "the pair budget restarts with the window")
}

func TestSignInLimiter_LockoutExpires(t *testing.T) {
	limiter, clock := newTestSignInLimiter()

	const (
		email  = "victim@example.com"
		source = "198.51.100.13"
	)

	for range signInMaxClientAccountFailures {
		limiter.recordFailure(email, source)
	}

	require.ErrorIs(t, limiter.check(email, source), ErrTooManyLoginAttempts)

	clock.advance(signInLockoutDuration + time.Second)
	require.NoError(t, limiter.check(email, source))
}

func TestSignInLimiter_SuccessKeepsClientBudget(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	const source = "198.51.100.14"

	for i := range signInMaxClientFailures - 1 {
		limiter.recordFailure(sprayEmail(i), source)
	}

	limiter.recordFailure("owner@example.com", source)
	limiter.recordSuccess("owner@example.com", source)

	require.NotContains(
		t,
		limiter.accounts,
		signInAccountKey("owner@example.com"),
		"a valid credential clears the account delay",
	)
	require.NotContains(t, limiter.clientAccounts, signInClientAccountKey("owner@example.com", source))
	require.Equal(
		t,
		signInMaxClientFailures,
		limiter.clients[source].failures,
		"one valid account must not reset the client spraying budget",
	)
}

func TestSignInLimiter_NormalizesAccount(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	const source = "198.51.100.15"

	for range signInMaxClientAccountFailures {
		limiter.recordFailure("  Victim@Example.COM ", source)
	}

	require.ErrorIs(t, limiter.check("victim@example.com", source), ErrTooManyLoginAttempts)
}

func TestSignInLimiter_PrunesExpiredEntries(t *testing.T) {
	limiter, clock := newTestSignInLimiter()

	limiter.recordFailure("stale@example.com", "198.51.100.16")
	require.Len(t, limiter.accounts, 1)

	clock.advance(signInFailureWindow + signInPruneInterval)
	require.NoError(t, limiter.check("other@example.com", "203.0.113.9"))

	require.Empty(t, limiter.accounts)
	require.Empty(t, limiter.clients)
	require.Empty(t, limiter.clientAccounts)
}

// An unattributable client never reaches the lockout tables, so recording a
// failure has to prune as well or nothing ever would.
func TestSignInLimiter_PrunesOnFailurePath(t *testing.T) {
	limiter, clock := newTestSignInLimiter()

	limiter.recordFailure("stale@example.com", "")
	require.Len(t, limiter.accounts, 1)

	clock.advance(signInFailureWindow + signInPruneInterval)
	limiter.recordFailure("fresh@example.com", "")

	require.Len(t, limiter.accounts, 1)
	require.NotContains(t, limiter.accounts, signInAccountKey("stale@example.com"))
}

func TestSignInLimiter_KeysHaveFixedSize(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	overlong := strings.Repeat("a", 100_000) + "@example.com"
	limiter.recordFailure(overlong, strings.Repeat("b", 100_000))

	for key := range limiter.accounts {
		require.Len(t, key, sha256.Size)
	}

	for key := range limiter.clientAccounts {
		require.Len(t, key, sha256.Size)
	}
}

func TestSignInLimiter_BoundsTrackedAccounts(t *testing.T) {
	limiter, _ := newTestSignInLimiter()

	for i := range signInMaxTrackedKeys + 500 {
		limiter.recordFailure(fmt.Sprintf("flood-%d@example.com", i), "")
	}

	require.LessOrEqual(t, len(limiter.accounts), signInMaxTrackedKeys)
}

func TestSignInLimiter_EvictionKeepsLockouts(t *testing.T) {
	limiter, clock := newTestSignInLimiter()

	const (
		email  = "victim@example.com"
		source = "203.0.113.18"
	)

	for range signInMaxClientAccountFailures {
		limiter.recordFailure(email, source)
	}

	require.ErrorIs(t, limiter.check(email, source), ErrTooManyLoginAttempts)

	// Flooding the table with invented pairs must not release the lockout.
	for i := range signInMaxTrackedKeys + 500 {
		clock.advance(time.Millisecond)
		limiter.recordFailure(
			fmt.Sprintf("flood-%d@example.com", i),
			fmt.Sprintf("198.51.100.%d", i%254+1),
		)
	}

	require.LessOrEqual(t, len(limiter.clientAccounts), signInMaxTrackedKeys)
	require.ErrorIs(t, limiter.check(email, source), ErrTooManyLoginAttempts)
}

func TestSignInLimiter_NilLimiterIsInert(t *testing.T) {
	var limiter *signInLimiter

	require.NoError(t, limiter.check("victim@example.com", "198.51.100.17"))
	require.Zero(t, limiter.recordFailure("victim@example.com", "198.51.100.17"))
	require.NotPanics(t, func() { limiter.recordSuccess("victim@example.com", "198.51.100.17") })
}

func sprayEmail(i int) string {
	return string(rune('a'+i%26)) + "-sprayed@example.com"
}
