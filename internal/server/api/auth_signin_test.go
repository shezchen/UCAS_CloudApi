package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

const (
	signInTestEmail    = "member@ucas.ac.cn"
	signInTestPassword = "correct-horse-battery"
)

type signInFixture struct {
	engine *gin.Engine
	client *ent.Client

	// withheld records the delay each failed sign-in was charged instead of
	// actually waiting for it.
	withheld []time.Duration
}

func setupSignInHandler(t *testing.T, trustedProxies []string) *signInFixture {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:signin?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	systemService := biz.NewSystemService(biz.SystemServiceParams{
		CacheConfig: cacheConfig,
		Ent:         client,
	})
	authService := biz.NewAuthService(biz.AuthServiceParams{
		SystemService: systemService,
		UserService: biz.NewUserService(biz.UserServiceParams{
			CacheConfig: cacheConfig,
			Ent:         client,
		}),
		Ent: client,
	})

	setupCtx := ent.NewContext(authz.WithTestBypass(t.Context()), client)

	secretKey, err := biz.GenerateSecretKey()
	require.NoError(t, err)
	require.NoError(t, systemService.SetSecretKey(setupCtx, secretKey))

	hashedPassword, err := biz.HashPassword(signInTestPassword)
	require.NoError(t, err)
	_, err = client.User.Create().
		SetEmail(signInTestEmail).
		SetPassword(hashedPassword).
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)

	handler := NewAuthHandlers(AuthHandlersParams{
		AuthService:    authService,
		TrustedProxies: trustedProxies,
	})

	fixture := &signInFixture{client: client}
	handler.withhold = func(_ context.Context, delay time.Duration) {
		fixture.withheld = append(fixture.withheld, delay)
	}

	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(trustedProxies))
	engine.POST("/admin/auth/signin", handler.SignIn)
	fixture.engine = engine

	return fixture
}

func (f *signInFixture) signIn(t *testing.T, password, remoteAddr, forwardedFor string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(SignInRequest{Email: signInTestEmail, Password: password})
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodPost, "/admin/auth/signin", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = remoteAddr + ":51234"

	if forwardedFor != "" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}

	recorder := httptest.NewRecorder()
	f.engine.ServeHTTP(recorder, request)

	return recorder
}

// Without trusted_proxies the resolved client address is the TCP peer, which
// behind a reverse proxy is shared by every user. Failures must then never
// produce a lockout, whatever an attacker claims in X-Forwarded-For.
func TestAuthHandlers_SignIn_UnattributableClientIsNeverLockedOut(t *testing.T) {
	fixture := setupSignInHandler(t, nil)

	for i := range 6 {
		response := fixture.signIn(t, "wrong-password", "172.18.0.5", "203.0.113.99")
		require.Equal(t, http.StatusUnauthorized, response.Code, "attempt %d", i+1)
	}

	response := fixture.signIn(t, signInTestPassword, "172.18.0.5", "203.0.113.99")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	// Guessing still gets more expensive with every failure.
	require.Equal(t, []time.Duration{
		0,
		0,
		0,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
	}, fixture.withheld)
}

// A request whose client address resolves to a trusted proxy is equally
// unattributable: the proxy stands for every user behind it.
func TestAuthHandlers_SignIn_ProxyAddressIsNeverLockedOut(t *testing.T) {
	fixture := setupSignInHandler(t, []string{"192.0.2.100"})

	for i := range 6 {
		response := fixture.signIn(t, "wrong-password", "192.0.2.100", "")
		require.Equal(t, http.StatusUnauthorized, response.Code, "attempt %d", i+1)
	}

	response := fixture.signIn(t, signInTestPassword, "192.0.2.100", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestAuthHandlers_SignIn_LocksOutAttributableClientOnly(t *testing.T) {
	fixture := setupSignInHandler(t, []string{"192.0.2.100"})

	for i := range 5 {
		response := fixture.signIn(t, "wrong-password", "192.0.2.100", "203.0.113.5")
		require.Equal(t, http.StatusUnauthorized, response.Code, "attempt %d", i+1)
	}

	locked := fixture.signIn(t, signInTestPassword, "192.0.2.100", "203.0.113.5")
	require.Equal(t, http.StatusTooManyRequests, locked.Code)
	require.Equal(t, biz.ErrTooManyLoginAttempts.Error(), decodeAPIError(t, locked).Error.Message)

	// The account itself stays reachable: the lockout belongs to the client
	// that produced the failures.
	response := fixture.signIn(t, signInTestPassword, "192.0.2.100", "203.0.113.6")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}

func TestWithholdSignInFailure(t *testing.T) {
	start := time.Now()
	withholdSignInFailure(t.Context(), 20*time.Millisecond)
	require.GreaterOrEqual(t, time.Since(start), 20*time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	start = time.Now()
	withholdSignInFailure(ctx, time.Minute)
	require.Less(t, time.Since(start), 5*time.Second, "a cancelled request must not keep waiting")
}

func TestParseTrustedProxyNetworks(t *testing.T) {
	networks := parseTrustedProxyNetworks([]string{
		" 192.0.2.100 ",
		"10.0.0.0/8",
		"::1",
		"",
		"not-an-ip",
	})
	require.Len(t, networks, 3)

	contains := func(value string) bool {
		ip := net.ParseIP(value)
		for _, network := range networks {
			if network.Contains(ip) {
				return true
			}
		}

		return false
	}

	require.True(t, contains("192.0.2.100"))
	require.False(t, contains("192.0.2.101"))
	require.True(t, contains("10.1.2.3"))
	require.True(t, contains("::1"))
}
