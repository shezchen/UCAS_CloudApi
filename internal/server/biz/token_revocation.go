package biz

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
)

// JWT revocation is implemented with a per-user "token valid after" timestamp
// stored in the system key-value table. Tokens carry an "iat" claim; any token
// that cannot be proven to have been issued after the stored timestamp is
// rejected. This gives durable revocation (sign-out everywhere, password
// change, deactivation) without a schema change. The check reads the database
// directly instead of a cache so a revocation takes effect immediately.

// Timestamps are stored in milliseconds behind a prefix that names the unit,
// so an upgrade cannot read a value written in seconds as a 1970 millisecond
// timestamp and quietly un-revoke every token.
const userTokenValidAfterMillisPrefix = "ms:"

func userTokenValidAfterKey(userID int) string {
	return fmt.Sprintf("auth_token_valid_after:%d", userID)
}

func formatUserTokenValidAfter(ts time.Time) string {
	return userTokenValidAfterMillisPrefix + strconv.FormatInt(ts.UnixMilli(), 10)
}

func parseUserTokenValidAfter(value string) (time.Time, error) {
	if millis, ok := strings.CutPrefix(value, userTokenValidAfterMillisPrefix); ok {
		parsed, err := strconv.ParseInt(millis, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid token revocation timestamp %q: %w", value, err)
		}

		return time.UnixMilli(parsed), nil
	}

	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid token revocation timestamp %q: %w", value, err)
	}

	return time.Unix(seconds, 0), nil
}

// userTokenRevocationCutoff returns the earliest issued-at second a token may
// carry and still be served. An "iat" of T only proves the token was issued
// somewhere inside [T, T+1s), so a revocation recorded during that second
// rounds up: the token cannot be shown to be newer than the revocation.
func userTokenRevocationCutoff(validAfter time.Time) int64 {
	cutoff := validAfter.Unix()
	if validAfter.Nanosecond() > 0 {
		cutoff++
	}

	return cutoff
}

// setUserTokenValidAfter persists the revocation timestamp for a user. The
// write runs under a system bypass because the callers (sign-out, password
// change, user deactivation) are not necessarily allowed to mutate system
// settings.
func setUserTokenValidAfter(ctx context.Context, client *ent.Client, userID int, ts time.Time) error {
	return authz.RunWithSystemBypassVoid(ctx, "auth-token-revocation", func(bypassCtx context.Context) error {
		key := userTokenValidAfterKey(userID)

		// Revocation only moves forward. Without this a transaction that
		// started earlier but commits later would write its older timestamp
		// over a newer one and bring the tokens it revoked back to life.
		existing, err := client.System.Query().
			Where(system.KeyEQ(key)).
			Only(bypassCtx)

		switch {
		case err == nil:
			// A value that cannot be parsed is repaired by overwriting it.
			if current, parseErr := parseUserTokenValidAfter(existing.Value); parseErr == nil && !ts.After(current) {
				return nil
			}
		case !ent.IsNotFound(err):
			return fmt.Errorf("failed to load token revocation timestamp: %w", err)
		}

		err = client.System.Create().
			SetKey(key).
			SetValue(formatUserTokenValidAfter(ts)).
			OnConflict(sql.ConflictColumns("key")).
			UpdateNewValues().
			Exec(bypassCtx)
		if err != nil {
			return fmt.Errorf("failed to store token revocation timestamp: %w", err)
		}

		return nil
	})
}

// userTokenValidAfter loads the revocation timestamp for a user. A zero time
// means no revocation has been recorded.
func userTokenValidAfter(ctx context.Context, client *ent.Client, userID int) (time.Time, error) {
	return authz.RunWithSystemBypass(ctx, "auth-token-revocation", func(bypassCtx context.Context) (time.Time, error) {
		sys, err := client.System.Query().
			Where(system.KeyEQ(userTokenValidAfterKey(userID))).
			Only(bypassCtx)
		if err != nil {
			if ent.IsNotFound(err) {
				return time.Time{}, nil
			}

			return time.Time{}, fmt.Errorf("failed to load token revocation timestamp: %w", err)
		}

		return parseUserTokenValidAfter(sys.Value)
	})
}
