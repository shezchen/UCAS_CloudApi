package biz

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/emailverificationchallenge"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/pkg/xredis"
	servermail "github.com/looplj/axonhub/internal/server/mail"
)

func TestHashPassword(t *testing.T) {
	password := "test-password-123"

	hashedPassword, err := HashPassword(password)
	require.NoError(t, err)
	require.NotEmpty(t, hashedPassword)
	require.NotEqual(t, password, hashedPassword)

	// Test that same password produces different hashes (due to salt)
	hashedPassword2, err := HashPassword(password)
	require.NoError(t, err)
	require.NotEqual(t, hashedPassword, hashedPassword2)
}

func TestVerifyPassword(t *testing.T) {
	password := "test-password-123"
	wrongPassword := "wrong-password"

	hashedPassword, err := HashPassword(password)
	require.NoError(t, err)

	// Test correct password
	err = VerifyPassword(hashedPassword, password)
	require.NoError(t, err)

	// Test wrong password
	err = VerifyPassword(hashedPassword, wrongPassword)
	require.Error(t, err)

	// Test invalid hash
	err = VerifyPassword("invalid-hash", password)
	require.Error(t, err)
}

func TestGenerateSecretKey(t *testing.T) {
	secretKey, err := GenerateSecretKey()
	require.NoError(t, err)
	require.NotEmpty(t, secretKey)
	require.Len(t, secretKey, 64) // 32 bytes * 2 (hex encoding)

	// Test that multiple calls produce different keys
	secretKey2, err := GenerateSecretKey()
	require.NoError(t, err)
	require.NotEqual(t, secretKey, secretKey2)
}

func setupTestDB(t *testing.T) *ent.Client {
	t.Helper()
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")

	return client
}

func setupTestAuthService(t *testing.T, cacheConfig xcache.Config) (*AuthService, *ent.Client, func()) {
	t.Helper()
	client := setupTestDB(t)

	// Create a mock system service
	systemService := &SystemService{
		Cache: xcache.NewFromConfig[ent.System](cacheConfig),
	}

	// Set up a test secret key in the system service
	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	// Create system entry for secret key
	secretKey, err := GenerateSecretKey()
	require.NoError(t, err)

	_, err = client.System.Create().
		SetKey(SystemKeySecretKey).
		SetValue(secretKey).
		Save(ctx)
	require.NoError(t, err)

	userService := &UserService{
		UserCache: xcache.NewFromConfig[ent.User](cacheConfig),
	}

	projectService := &ProjectService{
		ProjectCache: xcache.NewFromConfig[xcache.Entry[ent.Project]](cacheConfig),
	}

	apiKeyService := NewAPIKeyService(APIKeyServiceParams{
		CacheConfig:    cacheConfig,
		Ent:            client,
		ProjectService: projectService,
		KeyPrefix:      "ah",
	})

	authService := &AuthService{
		SystemService: systemService,
		APIKeyService: apiKeyService,
		UserService:   userService,
		AllowNoAuth:   false,
	}

	cleanup := func() {
		apiKeyService.Stop()
	}

	return authService, client, cleanup
}

type capturedVerificationMessage struct {
	to   string
	code string
	ttl  time.Duration
}

type capturingVerificationSender struct {
	mu       sync.Mutex
	messages []capturedVerificationMessage
	sendErr  error
}

var _ servermail.VerificationSender = (*capturingVerificationSender)(nil)

func (s *capturingVerificationSender) SendVerificationCode(_ context.Context, to, code string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sendErr != nil {
		return s.sendErr
	}
	s.messages = append(s.messages, capturedVerificationMessage{to: to, code: code, ttl: ttl})

	return nil
}

func (s *capturingVerificationSender) latestCode(t *testing.T, email string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.messages) - 1; i >= 0; i-- {
		if s.messages[i].to == email {
			return s.messages[i].code
		}
	}
	t.Fatalf("no verification message captured for requested email")

	return ""
}

func (s *capturingVerificationSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.messages)
}

func codeDifferentFrom(code string) string {
	if code == "000000" {
		return "000001"
	}

	return "000000"
}

type mutableAuthClock struct {
	now time.Time
}

func (c *mutableAuthClock) Now() time.Time {
	return c.now
}

type campusRegistrationFixture struct {
	auth     *AuthService
	client   *ent.Client
	setupCtx context.Context
	sender   *capturingVerificationSender
	clock    *mutableAuthClock
}

func setupCampusRegistrationAuthService(t *testing.T, config CampusEmailVerificationConfig) *campusRegistrationFixture {
	t.Helper()

	client := setupTestDB(t)
	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	userService := NewUserService(UserServiceParams{
		CacheConfig: cacheConfig,
		Ent:         client,
	})
	systemService := NewSystemService(SystemServiceParams{
		CacheConfig: cacheConfig,
		Ent:         client,
	})
	sender := &capturingVerificationSender{}
	authService := NewAuthService(AuthServiceParams{
		SystemService:           systemService,
		UserService:             userService,
		Ent:                     client,
		EmailVerificationConfig: config,
		VerificationSender:      sender,
	})
	setupCtx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	secretKey, err := GenerateSecretKey()
	require.NoError(t, err)
	require.NoError(t, systemService.SetSecretKey(setupCtx, secretKey))

	clock := &mutableAuthClock{now: time.Now().UTC()}
	authService.now = clock.Now

	return &campusRegistrationFixture{
		auth:     authService,
		client:   client,
		setupCtx: setupCtx,
		sender:   sender,
		clock:    clock,
	}
}

func createCampusRegistrationProject(t *testing.T, fixture *campusRegistrationFixture) *ent.Project {
	t.Helper()

	created, err := fixture.client.Project.Create().
		SetName("Campus Sharing").
		SetStatus(project.StatusActive).
		Save(fixture.setupCtx)
	require.NoError(t, err)

	return created
}

func TestAuthService_CampusEmailVerification_AllUCASDomainsAndNickname(t *testing.T) {
	fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	archivedProject, err := fixture.client.Project.Create().
		SetName("Archived").
		SetStatus(project.StatusArchived).
		Save(fixture.setupCtx)
	require.NoError(t, err)

	defaultProject := createCampusRegistrationProject(t, fixture)
	require.Greater(t, defaultProject.ID, archivedProject.ID)

	testCases := []struct {
		name            string
		email           string
		normalizedEmail string
	}{
		{name: "student ac cn", email: "student@mails.ucas.ac.cn", normalizedEmail: "student@mails.ucas.ac.cn"},
		{name: "staff ac cn", email: "staff@UCAS.AC.CN", normalizedEmail: "staff@ucas.ac.cn"},
		{name: "student edu cn", email: "student@mails.ucas.edu.cn", normalizedEmail: "student@mails.ucas.edu.cn"},
		{name: "staff edu cn", email: "staff@ucas.edu.cn", normalizedEmail: "staff@ucas.edu.cn"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := fixture.auth.RequestCampusEmailVerification(t.Context(), tc.email, "198.51.100.10")
			require.NoError(t, err)
			code := fixture.sender.latestCode(t, tc.normalizedEmail)

			created, err := fixture.auth.RegisterCampusUser(t.Context(), tc.email, "password-123", "  星河同学  ", code)
			require.NoError(t, err)
			require.Equal(t, tc.normalizedEmail, created.Email)
			require.Equal(t, "星河同学", created.Nickname)
			require.False(t, created.IsOwner)
			require.Equal(t, int64(200_000_000), created.DailyTokenLimit)
			require.Empty(t, created.Scopes)

			membership, err := fixture.client.UserProject.Query().
				Where(userproject.UserIDEQ(created.ID)).
				Only(fixture.setupCtx)
			require.NoError(t, err)
			require.Equal(t, defaultProject.ID, membership.ProjectID)
			require.False(t, membership.IsOwner)
			require.ElementsMatch(t, []string{"read_api_keys", "write_api_keys"}, membership.Scopes)
		})
	}
}

func TestAuthService_CampusEmailVerification_DoesNotPersistPlaintextSecrets(t *testing.T) {
	fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	email := "digest@mails.ucas.ac.cn"
	source := "203.0.113.42"
	require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), email, source))
	code := fixture.sender.latestCode(t, email)

	challenge, err := fixture.client.EmailVerificationChallenge.Query().
		Where(emailverificationchallenge.EmailEQ(email)).
		Only(fixture.setupCtx)
	require.NoError(t, err)
	require.NotEqual(t, code, challenge.CodeDigest)
	require.NotEqual(t, source, challenge.SourceHash)
	require.Len(t, challenge.CodeDigest, 64)
	require.Len(t, challenge.SourceHash, 64)
}

func TestAuthService_RegisterCampusUser_RejectsMissingWrongExpiredAndReusedCodes(t *testing.T) {
	t.Run("not requested", func(t *testing.T) {
		fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
		defer fixture.client.Close()
		createCampusRegistrationProject(t, fixture)

		created, err := fixture.auth.RegisterCampusUser(t.Context(), "unsent@mails.ucas.ac.cn", "password-123", "无邮件同学", "000000")
		require.ErrorIs(t, err, ErrVerificationInvalid)
		require.Nil(t, created)
		require.Equal(t, 0, fixture.sender.count())
	})

	t.Run("wrong code records attempt", func(t *testing.T) {
		fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
		defer fixture.client.Close()
		createCampusRegistrationProject(t, fixture)

		email := "wrong@mails.ucas.ac.cn"
		require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), email, "198.51.100.11"))
		correctCode := fixture.sender.latestCode(t, email)
		_, err := fixture.auth.RegisterCampusUser(t.Context(), email, "password-123", "错误码同学", codeDifferentFrom(correctCode))
		require.ErrorIs(t, err, ErrVerificationInvalid)

		challenge, queryErr := fixture.client.EmailVerificationChallenge.Query().
			Where(emailverificationchallenge.EmailEQ(email)).
			Only(fixture.setupCtx)
		require.NoError(t, queryErr)
		require.Equal(t, 1, challenge.Attempts)
	})

	t.Run("expired", func(t *testing.T) {
		fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{CodeTTL: time.Minute})
		defer fixture.client.Close()
		createCampusRegistrationProject(t, fixture)

		email := "expired@mails.ucas.ac.cn"
		require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), email, "198.51.100.12"))
		code := fixture.sender.latestCode(t, email)
		fixture.clock.now = fixture.clock.now.Add(time.Minute + time.Second)

		_, err := fixture.auth.RegisterCampusUser(t.Context(), email, "password-123", "过期码同学", code)
		require.ErrorIs(t, err, ErrVerificationInvalid)
	})

	t.Run("one time", func(t *testing.T) {
		fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
		defer fixture.client.Close()
		createCampusRegistrationProject(t, fixture)

		email := "once@mails.ucas.ac.cn"
		require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), email, "198.51.100.13"))
		code := fixture.sender.latestCode(t, email)
		_, err := fixture.auth.RegisterCampusUser(t.Context(), email, "password-123", "一次性同学", code)
		require.NoError(t, err)

		_, err = fixture.auth.RegisterCampusUser(t.Context(), email, "password-456", "重复使用同学", code)
		require.ErrorIs(t, err, ErrVerificationInvalid)

		challenge, queryErr := fixture.client.EmailVerificationChallenge.Query().
			Where(emailverificationchallenge.EmailEQ(email)).
			Only(fixture.setupCtx)
		require.NoError(t, queryErr)
		require.NotNil(t, challenge.ConsumedAt)
	})
}

func TestAuthService_RegisterCampusUser_MaxVerificationAttempts(t *testing.T) {
	fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{MaxAttempts: 3})
	defer fixture.client.Close()
	createCampusRegistrationProject(t, fixture)

	email := "attempts@mails.ucas.ac.cn"
	require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), email, "198.51.100.14"))
	correctCode := fixture.sender.latestCode(t, email)
	wrongCode := codeDifferentFrom(correctCode)
	for range 3 {
		_, err := fixture.auth.RegisterCampusUser(t.Context(), email, "password-123", "尝试次数同学", wrongCode)
		require.ErrorIs(t, err, ErrVerificationInvalid)
	}

	_, err := fixture.auth.RegisterCampusUser(t.Context(), email, "password-123", "尝试次数同学", correctCode)
	require.ErrorIs(t, err, ErrVerificationInvalid)
	challenge, queryErr := fixture.client.EmailVerificationChallenge.Query().
		Where(emailverificationchallenge.EmailEQ(email)).
		Only(fixture.setupCtx)
	require.NoError(t, queryErr)
	require.Equal(t, 3, challenge.Attempts)
}

func TestAuthService_RequestCampusEmailVerification_RateLimits(t *testing.T) {
	t.Run("email", func(t *testing.T) {
		fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{
			ResendCooldown:    time.Second,
			EmailHourlyLimit:  1,
			SourceHourlyLimit: 10,
			GlobalHourlyLimit: 10,
		})
		defer fixture.client.Close()

		email := "email-limit@mails.ucas.ac.cn"
		require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), email, "198.51.100.21"))
		fixture.clock.now = fixture.clock.now.Add(2 * time.Second)
		err := fixture.auth.RequestCampusEmailVerification(t.Context(), email, "198.51.100.22")
		require.ErrorIs(t, err, ErrVerificationRateLimit)
		require.Equal(t, 1, fixture.sender.count())
	})

	t.Run("source", func(t *testing.T) {
		fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{
			EmailHourlyLimit:  10,
			SourceHourlyLimit: 1,
			GlobalHourlyLimit: 10,
		})
		defer fixture.client.Close()

		source := "198.51.100.23"
		require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), "source-one@mails.ucas.ac.cn", source))
		err := fixture.auth.RequestCampusEmailVerification(t.Context(), "source-two@mails.ucas.ac.cn", source)
		require.ErrorIs(t, err, ErrVerificationRateLimit)
		require.Equal(t, 1, fixture.sender.count())
	})

	t.Run("global", func(t *testing.T) {
		fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{
			EmailHourlyLimit:  10,
			SourceHourlyLimit: 10,
			GlobalHourlyLimit: 1,
		})
		defer fixture.client.Close()

		require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), "global-one@mails.ucas.ac.cn", "198.51.100.24"))
		err := fixture.auth.RequestCampusEmailVerification(t.Context(), "global-two@mails.ucas.ac.cn", "198.51.100.25")
		require.ErrorIs(t, err, ErrVerificationRateLimit)
		require.Equal(t, 1, fixture.sender.count())
	})
}

func TestAuthService_RequestCampusEmailVerification_SMTPFailureFailsClosed(t *testing.T) {
	fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
	defer fixture.client.Close()
	fixture.sender.sendErr = errors.New("smtp unavailable")

	err := fixture.auth.RequestCampusEmailVerification(t.Context(), "smtp-failure@mails.ucas.ac.cn", "198.51.100.26")
	require.ErrorIs(t, err, ErrVerificationUnavailable)
	require.Equal(t, 0, fixture.sender.count())
	count, queryErr := fixture.client.EmailVerificationChallenge.Query().Count(fixture.setupCtx)
	require.NoError(t, queryErr)
	require.Equal(t, 1, count, "the failed sender must not fall back to unverified registration")
}

func TestAuthService_RegisterCampusUser_DuplicateIsGenericVerificationFailure(t *testing.T) {
	fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
	defer fixture.client.Close()
	createCampusRegistrationProject(t, fixture)

	hashedPassword, err := HashPassword("existing-password")
	require.NoError(t, err)
	_, err = fixture.client.User.Create().
		SetEmail("existing@mails.ucas.ac.cn").
		SetPassword(hashedPassword).
		Save(fixture.setupCtx)
	require.NoError(t, err)

	require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), "existing@mails.ucas.ac.cn", "198.51.100.27"))
	code := fixture.sender.latestCode(t, "existing@mails.ucas.ac.cn")
	_, err = fixture.auth.RegisterCampusUser(t.Context(), "existing@mails.ucas.ac.cn", "password-123", "已有账户同学", code)
	require.ErrorIs(t, err, ErrVerificationInvalid)
	require.NotErrorIs(t, err, ErrEmailAlreadyRegistered)
	require.NotContains(t, err.Error(), "already registered")
}

func TestAuthService_RegisterCampusUser_RejectsNonUCASDomains(t *testing.T) {
	fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	createCampusRegistrationProject(t, fixture)

	for _, email := range []string{
		"student@example.com",
		"student@evilucas.ac.cn",
		"student@department.ucas.ac.cn",
	} {
		t.Run(email, func(t *testing.T) {
			err := fixture.auth.RequestCampusEmailVerification(t.Context(), email, "198.51.100.28")
			require.ErrorIs(t, err, ErrCampusEmailRequired)
		})
	}
}

func TestAuthService_RegisterCampusUser_RequiresActiveProject(t *testing.T) {
	fixture := setupCampusRegistrationAuthService(t, CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	_, err := fixture.client.Project.Create().
		SetName("Archived").
		SetStatus(project.StatusArchived).
		Save(fixture.setupCtx)
	require.NoError(t, err)

	email := "no-project@ucas.edu.cn"
	require.NoError(t, fixture.auth.RequestCampusEmailVerification(t.Context(), email, "198.51.100.29"))
	code := fixture.sender.latestCode(t, email)
	_, err = fixture.auth.RegisterCampusUser(t.Context(), email, "password-123", "无项目同学", code)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to find the campus sharing project")

	count, queryErr := fixture.client.User.Query().
		Where(user.EmailEQ(email)).
		Count(fixture.setupCtx)
	require.NoError(t, queryErr)
	require.Zero(t, count, "the user insert must roll back when project assignment fails")

	challenge, queryErr := fixture.client.EmailVerificationChallenge.Query().
		Where(emailverificationchallenge.EmailEQ(email)).
		Only(fixture.setupCtx)
	require.NoError(t, queryErr)
	require.Nil(t, challenge.ConsumedAt, "challenge consumption must roll back with account creation")
}

func TestAuthService_GenerateJWTToken(t *testing.T) {
	// Test with memory cache
	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}

	authService, client, cleanup := setupTestAuthService(t, cacheConfig)
	defer cleanup()
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	// Create a test user
	hashedPassword, err := HashPassword("test-password")
	require.NoError(t, err)

	testUser, err := client.User.Create().
		SetEmail("test@example.com").
		SetPassword(hashedPassword).
		SetFirstName("Test").
		SetLastName("User").
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	// Generate JWT token
	token, err := authService.GenerateJWTToken(ctx, testUser)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// Get the actual secret key for validation
	secretKey, err := authService.SystemService.SecretKey(ctx)
	require.NoError(t, err)

	// Verify token structure
	parsedToken, err := jwt.Parse(token, func(token *jwt.Token) (any, error) {
		return []byte(secretKey), nil
	})
	require.NoError(t, err)

	claims, ok := parsedToken.Claims.(jwt.MapClaims)
	require.True(t, ok)

	userID, ok := claims["user_id"].(float64)
	require.True(t, ok)
	require.Equal(t, float64(testUser.ID), userID)

	exp, ok := claims["exp"].(float64)
	require.True(t, ok)
	require.True(t, exp > float64(time.Now().Unix()))
}

func TestAuthService_AuthenticateUser(t *testing.T) {
	// Test with Redis cache using miniredis
	mr := miniredis.RunT(t)

	cacheConfig := xcache.Config{
		Mode: xcache.ModeRedis,
		Redis: xredis.Config{
			Addr: mr.Addr(),
		},
	}

	authService, client, cleanup := setupTestAuthService(t, cacheConfig)
	defer cleanup()
	defer mr.Close()
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	// Create a test user
	password := "test-password-123"
	hashedPassword, err := HashPassword(password)
	require.NoError(t, err)

	testUser, err := client.User.Create().
		SetEmail("test@example.com").
		SetPassword(hashedPassword).
		SetFirstName("Test").
		SetLastName("User").
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	// Test successful authentication
	authenticatedUser, err := authService.AuthenticateUser(ctx, "test@example.com", password)
	require.NoError(t, err)
	require.Equal(t, testUser.ID, authenticatedUser.ID)
	require.Equal(t, testUser.Email, authenticatedUser.Email)

	// Test wrong password
	_, err = authService.AuthenticateUser(ctx, "test@example.com", "wrong-password")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid email or password")

	// Test non-existent user
	_, err = authService.AuthenticateUser(ctx, "nonexistent@example.com", password)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid email or password")

	// Test deactivated user
	_, err = authService.UserService.UpdateUserStatus(ctx, testUser.ID, user.StatusDeactivated)
	require.NoError(t, err)

	_, err = authService.AuthenticateUser(ctx, "test@example.com", password)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid email or password")
}

func TestAuthService_AuthenticateJWTToken(t *testing.T) {
	// Test with two-level cache
	mr := miniredis.RunT(t)

	cacheConfig := xcache.Config{
		Mode: xcache.ModeTwoLevel,
		Redis: xredis.Config{
			Addr: mr.Addr(),
		},
	}

	authService, client, cleanup := setupTestAuthService(t, cacheConfig)
	defer cleanup()
	defer mr.Close()
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	// Create a test user
	hashedPassword, err := HashPassword("test-password")
	require.NoError(t, err)

	testUser, err := client.User.Create().
		SetEmail("test@example.com").
		SetPassword(hashedPassword).
		SetFirstName("Test").
		SetLastName("User").
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	// Generate a valid JWT token
	tokenString, err := authService.GenerateJWTToken(ctx, testUser)
	require.NoError(t, err)

	// Test successful JWT authentication
	authenticatedUser, err := authService.AuthenticateJWTToken(ctx, tokenString)
	require.NoError(t, err)
	require.Equal(t, testUser.ID, authenticatedUser.ID)
	require.Equal(t, testUser.Email, authenticatedUser.Email)

	// Test cache hit - second call should use cache
	authenticatedUser2, err := authService.AuthenticateJWTToken(ctx, tokenString)
	require.NoError(t, err)
	require.Equal(t, testUser.ID, authenticatedUser2.ID)

	// Test invalid token
	_, err = authService.AuthenticateJWTToken(ctx, "invalid-token")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to parse jwt token")

	// Test expired token (create manually)
	expiredClaims := jwt.MapClaims{
		"user_id": float64(testUser.ID),
		"exp":     time.Now().Add(-time.Hour).Unix(), // Expired 1 hour ago
	}

	// Get secret key for signing
	secretKey, err := authService.SystemService.SecretKey(ctx)
	require.NoError(t, err)

	expiredToken := jwt.NewWithClaims(jwt.SigningMethodHS256, expiredClaims)
	expiredTokenString, err := expiredToken.SignedString([]byte(secretKey))
	require.NoError(t, err)

	_, err = authService.AuthenticateJWTToken(ctx, expiredTokenString)
	require.Error(t, err)

	// Test deactivated user
	_, err = authService.UserService.UpdateUserStatus(ctx, testUser.ID, user.StatusDeactivated)
	require.NoError(t, err)

	// Generate new token for deactivated user
	newTokenString, err := authService.GenerateJWTToken(ctx, testUser)
	require.NoError(t, err)

	_, err = authService.AuthenticateJWTToken(ctx, newTokenString)
	require.Error(t, err)
	require.Contains(t, err.Error(), "user not activated")
}

func TestAuthService_AuthenticateAPIKey(t *testing.T) {
	// Test with noop cache (no cache configured)
	cacheConfig := xcache.Config{} // Empty config = noop cache

	authService, client, cleanup := setupTestAuthService(t, cacheConfig)
	defer cleanup()
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	// Create a test user
	hashedPassword, err := HashPassword("test-password")
	require.NoError(t, err)

	testUser, err := client.User.Create().
		SetEmail(fmt.Sprintf("test-%d@example.com", time.Now().UnixNano())).
		SetPassword(hashedPassword).
		SetFirstName("Test").
		SetLastName("User").
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	projectName := uuid.NewString()
	testProject, err := client.Project.Create().SetName(
		projectName,
	).SetDescription(
		projectName,
	).SetStatus(
		project.StatusActive,
	).SetCreatedAt(
		time.Now(),
	).SetUpdatedAt(
		time.Now(),
	).Save(
		ctx,
	)
	require.NoError(t, err)

	// Generate API key
	apiKeyString, err := GenerateAPIKey("ah")
	require.NoError(t, err)

	// Create API key in database
	apiKey, err := client.APIKey.Create().
		SetKey(apiKeyString).
		SetName("Test API Key").
		SetUser(testUser).
		SetProject(testProject).
		Save(ctx)
	require.NoError(t, err)

	// Test successful API key authentication
	authenticatedAPIKey, err := authService.AuthenticateAPIKey(ctx, apiKeyString)
	require.NoError(t, err)
	require.Equal(t, apiKey.ID, authenticatedAPIKey.ID)
	require.Equal(t, apiKey.Key, authenticatedAPIKey.Key)

	// Test cache behavior - second call should still work (even with noop cache)
	authenticatedAPIKey2, err := authService.AuthenticateAPIKey(ctx, apiKeyString)
	require.NoError(t, err)
	require.Equal(t, apiKey.ID, authenticatedAPIKey2.ID)

	// Test invalid API key
	_, err = authService.AuthenticateAPIKey(ctx, "invalid-api-key")
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to get api key")

	// Test disabled API key
	_, err = authService.APIKeyService.UpdateAPIKeyStatus(ctx, apiKey.ID, "disabled")
	require.NoError(t, err)

	// Synchronously invalidate the cache for testing (async notification may not complete in time)
	authService.APIKeyService.APIKeyCache.Invalidate(buildAPIKeyCacheKey(apiKeyString))

	_, err = authService.AuthenticateAPIKey(ctx, apiKeyString)
	require.Error(t, err)
	require.Contains(t, err.Error(), "api key not enabled")

	// Test API key with inactive project
	// First, re-enable the API key
	_, err = authService.APIKeyService.UpdateAPIKeyStatus(ctx, apiKey.ID, "enabled")
	require.NoError(t, err)

	// Synchronously invalidate the cache for testing
	authService.APIKeyService.APIKeyCache.Invalidate(buildAPIKeyCacheKey(apiKeyString))

	// Then archive the project (making it inactive)
	_, err = client.Project.UpdateOneID(testProject.ID).
		SetStatus(project.StatusArchived).
		Save(ctx)
	require.NoError(t, err)

	_, err = authService.AuthenticateAPIKey(ctx, apiKeyString)
	require.Error(t, err)
	require.Contains(t, err.Error(), "api key project not valid")
}

func TestAuthService_AuthenticateNoAuth(t *testing.T) {
	cacheConfig := xcache.Config{}

	authService, client, cleanup := setupTestAuthService(t, cacheConfig)
	defer cleanup()
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)
	authService.AllowNoAuth = true

	hashedPassword, err := HashPassword("test-password")
	require.NoError(t, err)

	owner, err := client.User.Create().
		SetEmail(fmt.Sprintf("owner-%d@example.com", time.Now().UnixNano())).
		SetPassword(hashedPassword).
		SetFirstName("Owner").
		SetLastName("User").
		SetIsOwner(true).
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	defaultProject, err := client.Project.Create().
		SetName(uuid.NewString()).
		SetDescription("default").
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	noAuthKey, err := client.APIKey.Create().
		SetKey(NoAuthAPIKeyValue).
		SetName(NoAuthAPIKeyName).
		SetUserID(owner.ID).
		SetProjectID(defaultProject.ID).
		SetType(apikey.TypeNoauth).
		SetStatus(apikey.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	_, err = authService.AuthenticateAPIKey(ctx, NoAuthAPIKeyValue)
	require.Error(t, err)
	require.Contains(t, err.Error(), "noauth api key is only available when api auth is disabled")

	authenticatedAPIKey, err := authService.AuthenticateNoAuth(ctx)
	require.NoError(t, err)
	require.Equal(t, noAuthKey.ID, authenticatedAPIKey.ID)
	require.Equal(t, NoAuthAPIKeyValue, authenticatedAPIKey.Key)
}

func TestAuthService_AuthenticateNoAuth_DisabledByConfig(t *testing.T) {
	authService, client, cleanup := setupTestAuthService(t, xcache.Config{})
	defer cleanup()
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	_, err := authService.AuthenticateNoAuth(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "API key required")
}

func TestAuthService_WithDifferentCacheConfigs(t *testing.T) {
	testCases := []struct {
		name         string
		cacheMode    string
		requireRedis bool
	}{
		{
			name:         "Memory Cache",
			cacheMode:    xcache.ModeMemory,
			requireRedis: false,
		},
		{
			name:         "Redis Cache",
			cacheMode:    xcache.ModeRedis,
			requireRedis: true,
		},
		{
			name:         "Two-Level Cache",
			cacheMode:    xcache.ModeTwoLevel,
			requireRedis: true,
		},
		{
			name:         "Noop Cache",
			cacheMode:    "",
			requireRedis: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var cacheConfig xcache.Config

			if tc.requireRedis {
				mr := miniredis.RunT(t)
				cacheConfig = xcache.Config{
					Mode: tc.cacheMode,
					Redis: xredis.Config{
						Addr: mr.Addr(),
					},
				}
			} else {
				cacheConfig = xcache.Config{Mode: tc.cacheMode}
			}

			authService, client, cleanup := setupTestAuthService(t, cacheConfig)
			defer cleanup()
			defer client.Close()

			ctx := context.Background()
			ctx = ent.NewContext(ctx, client)
			ctx = authz.WithTestBypass(ctx)

			// Create a test user
			hashedPassword, err := HashPassword("test-password")
			require.NoError(t, err)

			testUser, err := client.User.Create().
				SetEmail("test@example.com").
				SetPassword(hashedPassword).
				SetFirstName("Test").
				SetLastName("User").
				SetStatus(user.StatusActivated).
				Save(ctx)
			require.NoError(t, err)

			// Test JWT token generation and authentication
			tokenString, err := authService.GenerateJWTToken(ctx, testUser)
			require.NoError(t, err)

			authenticatedUser, err := authService.AuthenticateJWTToken(ctx, tokenString)
			require.NoError(t, err)
			require.Equal(t, testUser.ID, authenticatedUser.ID)

			// Test user authentication
			authenticatedUser2, err := authService.AuthenticateUser(ctx, "test@example.com", "test-password")
			require.NoError(t, err)
			require.Equal(t, testUser.ID, authenticatedUser2.ID)
		})
	}
}

func TestAuthService_CacheExpiration(t *testing.T) {
	mr := miniredis.RunT(t)

	cacheConfig := xcache.Config{
		Mode: xcache.ModeRedis,
		Redis: xredis.Config{
			Addr:       mr.Addr(),
			Expiration: 100 * time.Millisecond, // Very short for testing
		},
	}

	authService, client, cleanup := setupTestAuthService(t, cacheConfig)
	defer cleanup()
	defer mr.Close()
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	// Create a test user
	hashedPassword, err := HashPassword("test-password")
	require.NoError(t, err)

	testUser, err := client.User.Create().
		SetEmail(fmt.Sprintf("test-%d@example.com", time.Now().UnixNano())).
		SetPassword(hashedPassword).
		SetFirstName("Test").
		SetLastName("User").
		SetStatus(user.StatusActivated).
		Save(ctx)
	require.NoError(t, err)

	projectName := uuid.NewString()
	testProject, err := client.Project.Create().SetName(
		projectName,
	).SetDescription(
		projectName,
	).SetStatus(
		project.StatusActive,
	).SetCreatedAt(
		time.Now(),
	).SetUpdatedAt(
		time.Now(),
	).Save(
		ctx,
	)
	require.NoError(t, err)

	// Generate API key
	apiKeyString, err := GenerateAPIKey("ah")
	require.NoError(t, err)

	apiKey, err := client.APIKey.Create().
		SetKey(apiKeyString).
		SetName("Test API Key").
		SetUser(testUser).
		SetProject(testProject).
		Save(ctx)
	require.NoError(t, err)

	// First call - should cache the result
	authenticatedAPIKey, err := authService.AuthenticateAPIKey(ctx, apiKeyString)
	require.NoError(t, err)
	require.Equal(t, apiKey.ID, authenticatedAPIKey.ID)

	// Wait for cache expiration
	time.Sleep(150 * time.Millisecond)

	// Second call - cache should be expired, should hit database again
	authenticatedAPIKey2, err := authService.AuthenticateAPIKey(ctx, apiKeyString)
	require.NoError(t, err)
	require.Equal(t, apiKey.ID, authenticatedAPIKey2.ID)
}
