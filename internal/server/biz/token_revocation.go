package biz

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
)

// JWT revocation is implemented with a per-user "token valid after" timestamp
// stored in the system key-value table. Tokens carry an "iat" claim; any token
// issued strictly before the stored timestamp is rejected. This gives durable
// revocation (sign-out everywhere, password change, deactivation) without a
// schema change. The check reads the database directly instead of a cache so a
// revocation takes effect immediately.

func userTokenValidAfterKey(userID int) string {
	return fmt.Sprintf("auth_token_valid_after:%d", userID)
}

// setUserTokenValidAfter persists the revocation timestamp for a user. The
// write runs under a system bypass because the callers (sign-out, password
// change, user deactivation) are not necessarily allowed to mutate system
// settings.
func setUserTokenValidAfter(ctx context.Context, client *ent.Client, userID int, ts time.Time) error {
	return authz.RunWithSystemBypassVoid(ctx, "auth-token-revocation", func(bypassCtx context.Context) error {
		err := client.System.Create().
			SetKey(userTokenValidAfterKey(userID)).
			SetValue(strconv.FormatInt(ts.Unix(), 10)).
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

		unix, err := strconv.ParseInt(sys.Value, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid token revocation timestamp %q: %w", sys.Value, err)
		}

		return time.Unix(unix, 0), nil
	})
}
