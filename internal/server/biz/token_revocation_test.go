package biz

import (
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func TestParseUserTokenValidAfter(t *testing.T) {
	ts := time.UnixMilli(1_772_000_000_123)

	parsed, err := parseUserTokenValidAfter(formatUserTokenValidAfter(ts))
	require.NoError(t, err)
	require.True(t, parsed.Equal(ts))

	// Values written before the unit was recorded are whole seconds.
	legacy, err := parseUserTokenValidAfter("1772000000")
	require.NoError(t, err)
	require.True(t, legacy.Equal(time.Unix(1_772_000_000, 0)))

	_, err = parseUserTokenValidAfter("ms:not-a-number")
	require.Error(t, err)
}

func TestUserTokenRevocationCutoff(t *testing.T) {
	whole := time.Unix(1_772_000_000, 0)
	require.Equal(t, int64(1_772_000_000), userTokenRevocationCutoff(whole))

	// A token issued during the revocation second cannot be shown to be newer
	// than the revocation, so the cutoff rounds up.
	partial := time.Unix(1_772_000_000, 900_000_000)
	require.Equal(t, int64(1_772_000_001), userTokenRevocationCutoff(partial))
}

type revokedTokenFixture struct {
	auth   *AuthService
	client *ent.Client
	user   *ent.User
	secret string
}

func setupRevokedTokenFixture(t *testing.T) *revokedTokenFixture {
	t.Helper()

	authService, client, cleanup := setupTestAuthService(t, xcache.Config{Mode: xcache.ModeMemory})
	t.Cleanup(cleanup)
	t.Cleanup(func() { _ = client.Close() })

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)

	testUser, err := client.User.Create().
		SetEmail(fmt.Sprintf("revoked-%d@example.com", time.Now().UnixNano())).
		SetPassword("hashed").
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	secret, err := authService.SystemService.SecretKey(ctx)
	require.NoError(t, err)

	return &revokedTokenFixture{auth: authService, client: client, user: testUser, secret: secret}
}

func (f *revokedTokenFixture) tokenWithClaims(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()

	claims["user_id"] = float64(f.user.ID)
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(f.secret))
	require.NoError(t, err)

	return signed
}

func TestAuthenticateJWTToken_RevocationIsInclusiveOfTheRevokedSecond(t *testing.T) {
	fixture := setupRevokedTokenFixture(t)
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), fixture.client)

	// A revocation part way through a second: a token stamped with that same
	// second may have been issued before it.
	revokedAt := time.Now().UTC().Truncate(time.Second).Add(500 * time.Millisecond)
	require.NoError(t, setUserTokenValidAfter(ctx, fixture.client, fixture.user.ID, revokedAt))

	sameSecond := fixture.tokenWithClaims(t, jwt.MapClaims{"iat": revokedAt.Unix()})
	_, err := fixture.auth.AuthenticateJWTToken(ctx, sameSecond)
	require.ErrorIs(t, err, ErrInvalidJWT)
	require.Contains(t, err.Error(), "token has been revoked")

	nextSecond := fixture.tokenWithClaims(t, jwt.MapClaims{"iat": revokedAt.Add(time.Second).Unix()})
	authenticated, err := fixture.auth.AuthenticateJWTToken(ctx, nextSecond)
	require.NoError(t, err)
	require.Equal(t, fixture.user.ID, authenticated.ID)
}

func TestAuthenticateJWTToken_RejectsTokenWithoutIssuedAt(t *testing.T) {
	fixture := setupRevokedTokenFixture(t)
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), fixture.client)

	undated := fixture.tokenWithClaims(t, jwt.MapClaims{})

	// Without a revocation such a token is still usable.
	_, err := fixture.auth.AuthenticateJWTToken(ctx, undated)
	require.NoError(t, err)

	require.NoError(t, setUserTokenValidAfter(ctx, fixture.client, fixture.user.ID, time.Now()))

	_, err = fixture.auth.AuthenticateJWTToken(ctx, undated)
	require.ErrorIs(t, err, ErrInvalidJWT)
	require.Contains(t, err.Error(), "token has been revoked")
}

func TestAuthenticateJWTToken_FailsClosedWhenRevocationIsUnreadable(t *testing.T) {
	fixture := setupRevokedTokenFixture(t)
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), fixture.client)

	token := fixture.tokenWithClaims(t, jwt.MapClaims{"iat": time.Now().Unix()})

	// Warm the caches the check depends on so only the revocation lookup fails.
	_, err := fixture.auth.AuthenticateJWTToken(ctx, token)
	require.NoError(t, err)

	require.NoError(t, fixture.client.Close())

	_, err = fixture.auth.AuthenticateJWTToken(ctx, token)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidJWT, "an unverifiable revocation state must not read as a plain rejection")
	require.Contains(t, err.Error(), "failed to check token revocation")
}

func TestUserTokenValidAfter_ReadsLegacySeconds(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)

	const userID = 4244

	_, err := client.System.Create().
		SetKey(userTokenValidAfterKey(userID)).
		SetValue("1772000000").
		Save(ctx)
	require.NoError(t, err)

	stored, err := userTokenValidAfter(ctx, client, userID)
	require.NoError(t, err)
	require.True(t, stored.Equal(time.Unix(1_772_000_000, 0)))

	count, err := client.System.Query().
		Where(system.KeyEQ(userTokenValidAfterKey(userID))).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}
