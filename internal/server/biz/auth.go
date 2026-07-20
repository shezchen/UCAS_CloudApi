package biz

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/fx"
	"golang.org/x/crypto/bcrypt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/emailverificationchallenge"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/log"
	servermail "github.com/looplj/axonhub/internal/server/mail"
)

const OIDC_ONLY_PLACEHOLDER = "!OIDC_SSO_ONLY!"

type CampusEmailVerificationConfig struct {
	CodeTTL           time.Duration `conf:"code_ttl"            yaml:"code_ttl"            json:"code_ttl"`
	ResendCooldown    time.Duration `conf:"resend_cooldown"     yaml:"resend_cooldown"     json:"resend_cooldown"`
	EmailHourlyLimit  int           `conf:"email_hourly_limit"  yaml:"email_hourly_limit"  json:"email_hourly_limit"`
	SourceHourlyLimit int           `conf:"source_hourly_limit" yaml:"source_hourly_limit" json:"source_hourly_limit"`
	GlobalHourlyLimit int           `conf:"global_hourly_limit" yaml:"global_hourly_limit" json:"global_hourly_limit"`
	MaxAttempts       int           `conf:"max_attempts"        yaml:"max_attempts"        json:"max_attempts"`
}

func (c CampusEmailVerificationConfig) withDefaults() CampusEmailVerificationConfig {
	if c.CodeTTL <= 0 {
		c.CodeTTL = 10 * time.Minute
	}
	if c.ResendCooldown <= 0 {
		c.ResendCooldown = time.Minute
	}
	if c.EmailHourlyLimit <= 0 {
		c.EmailHourlyLimit = 3
	}
	if c.SourceHourlyLimit <= 0 {
		c.SourceHourlyLimit = 20
	}
	if c.GlobalHourlyLimit <= 0 {
		c.GlobalHourlyLimit = 200
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}

	return c
}

// HashPassword hashes a password using bcrypt.
func HashPassword(password string) (string, error) {
	if password == OIDC_ONLY_PLACEHOLDER {
		return OIDC_ONLY_PLACEHOLDER, nil
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash password: %w", err)
	}

	return hex.EncodeToString(hashedPassword), nil
}

// VerifyPassword verifies a password against a hash.
func VerifyPassword(hashedPassword, password string) error {
	if hashedPassword == OIDC_ONLY_PLACEHOLDER {
		return ErrOIDCLoginRequired
	}

	decodedHashedPassword, err := hex.DecodeString(hashedPassword)
	if err != nil {
		return fmt.Errorf("failed to decode hashed password: %w", err)
	}

	return bcrypt.CompareHashAndPassword(decodedHashedPassword, []byte(password))
}

type AuthServiceParams struct {
	fx.In

	SystemService           *SystemService
	APIKeyService           *APIKeyService
	UserService             *UserService
	OIDCService             *OIDCService
	Ent                     *ent.Client
	EmailVerificationConfig CampusEmailVerificationConfig
	VerificationSender      servermail.VerificationSender
	AllowNoAuth             bool `name:"allow_no_auth"`
}

func NewAuthService(params AuthServiceParams) *AuthService {
	return &AuthService{
		AbstractService: &AbstractService{
			db: params.Ent,
		},
		SystemService:           params.SystemService,
		APIKeyService:           params.APIKeyService,
		UserService:             params.UserService,
		OIDCService:             params.OIDCService,
		EmailVerificationConfig: params.EmailVerificationConfig.withDefaults(),
		VerificationSender:      params.VerificationSender,
		AllowNoAuth:             params.AllowNoAuth,
		now:                     time.Now,
	}
}

type AuthService struct {
	*AbstractService

	SystemService           *SystemService
	APIKeyService           *APIKeyService
	UserService             *UserService
	OIDCService             *OIDCService
	EmailVerificationConfig CampusEmailVerificationConfig
	VerificationSender      servermail.VerificationSender
	AllowNoAuth             bool

	verificationMu sync.Mutex
	now            func() time.Time
}

func generateEmailVerificationCode() (string, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("generate email verification code: %w", err)
	}

	return fmt.Sprintf("%06d", value.Int64()), nil
}

func verificationDigest(secret, purpose, email, value string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(purpose))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(email))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(value))

	return hex.EncodeToString(mac.Sum(nil))
}

func normalizeVerificationCode(code string) (string, error) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return "", ErrVerificationInvalid
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return "", ErrVerificationInvalid
		}
	}

	return code, nil
}

func (s *AuthService) verificationSecret(ctx context.Context) (string, error) {
	secret, err := authz.RunWithSystemBypass(ctx, "campus-email-verification-secret", func(bypassCtx context.Context) (string, error) {
		return s.SystemService.SecretKey(bypassCtx)
	})
	if err != nil {
		return "", fmt.Errorf("%w: verification secret is unavailable", ErrVerificationUnavailable)
	}

	return secret, nil
}

// RequestCampusEmailVerification persists a rate-limited one-time challenge
// before sending it. Neither the plaintext code nor the client address is
// stored in the database or written to logs.
func (s *AuthService) RequestCampusEmailVerification(ctx context.Context, email, source string) error {
	normalizedEmail, err := normalizeCampusRegistrationEmail(email)
	if err != nil {
		return err
	}

	secret, err := s.verificationSecret(ctx)
	if err != nil {
		return err
	}
	code, err := generateEmailVerificationCode()
	if err != nil {
		return err
	}

	config := s.EmailVerificationConfig
	now := s.now()
	if strings.TrimSpace(source) == "" {
		source = "unknown"
	}
	sourceHash := verificationDigest(secret, "registration-source", "", source)
	codeDigest := verificationDigest(secret, "registration-code", normalizedEmail, code)

	// Serialize check-and-create within this process. Database rows keep the
	// limits durable across restarts; the mutex closes the common concurrent
	// request race for a single AxonHub instance.
	func() {
		s.verificationMu.Lock()
		defer s.verificationMu.Unlock()

		err = authz.RunWithSystemBypassVoid(ctx, "campus-email-verification-request", func(bypassCtx context.Context) error {
			return s.RunInTransaction(bypassCtx, func(txCtx context.Context) error {
				client := s.entFromContext(txCtx)
				hourAgo := now.Add(-time.Hour)

				latest, queryErr := client.EmailVerificationChallenge.Query().
					Where(emailverificationchallenge.EmailEQ(normalizedEmail)).
					Order(ent.Desc(emailverificationchallenge.FieldCreatedAt)).
					First(txCtx)
				if queryErr != nil && !ent.IsNotFound(queryErr) {
					return fmt.Errorf("query latest email verification challenge: %w", queryErr)
				}
				if queryErr == nil && now.Sub(latest.CreatedAt) < config.ResendCooldown {
					return ErrVerificationRateLimit
				}

				emailCount, queryErr := client.EmailVerificationChallenge.Query().
					Where(
						emailverificationchallenge.EmailEQ(normalizedEmail),
						emailverificationchallenge.CreatedAtGTE(hourAgo),
					).
					Count(txCtx)
				if queryErr != nil {
					return fmt.Errorf("count email verification challenges: %w", queryErr)
				}
				if emailCount >= config.EmailHourlyLimit {
					return ErrVerificationRateLimit
				}

				sourceCount, queryErr := client.EmailVerificationChallenge.Query().
					Where(
						emailverificationchallenge.SourceHashEQ(sourceHash),
						emailverificationchallenge.CreatedAtGTE(hourAgo),
					).
					Count(txCtx)
				if queryErr != nil {
					return fmt.Errorf("count source verification challenges: %w", queryErr)
				}
				if sourceCount >= config.SourceHourlyLimit {
					return ErrVerificationRateLimit
				}

				globalCount, queryErr := client.EmailVerificationChallenge.Query().
					Where(emailverificationchallenge.CreatedAtGTE(hourAgo)).
					Count(txCtx)
				if queryErr != nil {
					return fmt.Errorf("count global verification challenges: %w", queryErr)
				}
				if globalCount >= config.GlobalHourlyLimit {
					return ErrVerificationRateLimit
				}

				_, createErr := client.EmailVerificationChallenge.Create().
					SetEmail(normalizedEmail).
					SetCodeDigest(codeDigest).
					SetSourceHash(sourceHash).
					SetExpiresAt(now.Add(config.CodeTTL)).
					Save(txCtx)
				if createErr != nil {
					return fmt.Errorf("create email verification challenge: %w", createErr)
				}

				return nil
			})
		})
	}()
	if err != nil {
		return err
	}

	if err := s.VerificationSender.SendVerificationCode(ctx, normalizedEmail, code, config.CodeTTL); err != nil {
		return fmt.Errorf("%w: failed to send verification email", ErrVerificationUnavailable)
	}

	return nil
}

// RegisterCampusUser verifies and atomically consumes a challenge while
// creating a fixed-shape non-owner campus member. Callers cannot supply scopes,
// roles, ownership, or a quota override.
func (s *AuthService) RegisterCampusUser(ctx context.Context, email, password, nickname, verificationCode string) (*ent.User, error) {
	if len(password) < 8 {
		return nil, fmt.Errorf("password must be at least 8 characters")
	}

	normalizedEmail, err := normalizeCampusRegistrationEmail(email)
	if err != nil {
		return nil, err
	}
	normalizedNickname, err := NormalizeCampusNickname(nickname)
	if err != nil {
		return nil, err
	}
	code, err := normalizeVerificationCode(verificationCode)
	if err != nil {
		return nil, err
	}
	secret, err := s.verificationSecret(ctx)
	if err != nil {
		return nil, err
	}

	config := s.EmailVerificationConfig
	now := s.now()
	expectedDigest := verificationDigest(secret, "registration-code", normalizedEmail, code)
	verified := false
	var createdUser *ent.User

	err = authz.RunWithSystemBypassVoid(ctx, "campus-user-registration", func(registerCtx context.Context) error {
		return s.RunInTransaction(registerCtx, func(txCtx context.Context) error {
			client := s.entFromContext(txCtx)
			challenges, queryErr := client.EmailVerificationChallenge.Query().
				Where(
					emailverificationchallenge.EmailEQ(normalizedEmail),
					emailverificationchallenge.ConsumedAtIsNil(),
					emailverificationchallenge.ExpiresAtGT(now),
					emailverificationchallenge.AttemptsLT(config.MaxAttempts),
				).
				Order(ent.Desc(emailverificationchallenge.FieldCreatedAt)).
				All(txCtx)
			if queryErr != nil {
				return fmt.Errorf("query email verification challenges: %w", queryErr)
			}

			matchedID := 0
			challengeIDs := make([]int, 0, len(challenges))
			for _, challenge := range challenges {
				challengeIDs = append(challengeIDs, challenge.ID)
				matches := hmac.Equal([]byte(challenge.CodeDigest), []byte(expectedDigest))
				if matches && matchedID == 0 {
					matchedID = challenge.ID
				}
			}

			if matchedID == 0 {
				if len(challengeIDs) > 0 {
					_, updateErr := client.EmailVerificationChallenge.Update().
						Where(emailverificationchallenge.IDIn(challengeIDs...)).
						AddAttempts(1).
						Save(txCtx)
					if updateErr != nil {
						return fmt.Errorf("record verification failure: %w", updateErr)
					}
				}

				return nil
			}

			updated, updateErr := client.EmailVerificationChallenge.Update().
				Where(
					emailverificationchallenge.IDEQ(matchedID),
					emailverificationchallenge.ConsumedAtIsNil(),
					emailverificationchallenge.ExpiresAtGT(now),
					emailverificationchallenge.AttemptsLT(config.MaxAttempts),
				).
				SetConsumedAt(now).
				Save(txCtx)
			if updateErr != nil {
				return fmt.Errorf("consume verification challenge: %w", updateErr)
			}
			if updated != 1 {
				return nil
			}

			_, updateErr = client.EmailVerificationChallenge.Update().
				Where(
					emailverificationchallenge.EmailEQ(normalizedEmail),
					emailverificationchallenge.ConsumedAtIsNil(),
				).
				SetConsumedAt(now).
				Save(txCtx)
			if updateErr != nil {
				return fmt.Errorf("consume prior verification challenges: %w", updateErr)
			}

			createdUser, updateErr = s.UserService.CreateUser(txCtx, ent.CreateUserInput{
				Email:    normalizedEmail,
				Password: password,
				Nickname: &normalizedNickname,
			})
			if updateErr != nil {
				return updateErr
			}

			verified = true
			return nil
		})
	})
	if err != nil {
		if errors.Is(err, ErrEmailAlreadyRegistered) {
			return nil, ErrVerificationInvalid
		}
		return nil, err
	}
	if !verified {
		return nil, ErrVerificationInvalid
	}

	return createdUser, nil
}

// GenerateSecretKey generates a random secret key for JWT.
func GenerateSecretKey() (string, error) {
	bytes := make([]byte, 32) // 256 bits

	_, err := rand.Read(bytes)
	if err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	return hex.EncodeToString(bytes), nil
}

// GenerateJWTToken generates a JWT token for a user.
func (s *AuthService) GenerateJWTToken(ctx context.Context, user *ent.User) (string, error) {
	secretKey, err := authz.RunWithSystemBypass(ctx, "auth-get-secret-key", func(bypassCtx context.Context) (string, error) {
		return s.SystemService.SecretKey(bypassCtx)
	})
	if err != nil {
		return "", fmt.Errorf("failed to get secret key: %w", err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": user.ID,
		"exp":     time.Now().Add(time.Hour * 24 * 7).Unix(), // 7 days
	})

	tokenString, err := token.SignedString([]byte(secretKey))
	if err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}

	return tokenString, nil
}

// AuthenticateUser authenticates a user with email and password.
func (s *AuthService) AuthenticateUser(
	ctx context.Context,
	email, password string,
) (*ent.User, error) {
	u, err := authz.RunWithSystemBypass(ctx, "auth-lookup", func(bypassCtx context.Context) (*ent.User, error) {
		client := s.entFromContext(bypassCtx)

		return client.User.Query().
			Where(user.EmailEqualFold(strings.TrimSpace(email))).
			Where(user.StatusEQ(user.StatusActivated)).
			WithRoles().
			Only(bypassCtx)
	})
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("invalid email or password: %w", ErrInvalidPassword)
		}

		log.Error(ctx, "failed to get user", log.Cause(err))

		return nil, ErrInternal
	}

	if s.OIDCService != nil && s.OIDCService.IsUserRestrictedToOIDC(ctx, u) {
		return nil, ErrOIDCLoginRequired
	}

	err = VerifyPassword(u.Password, password)
	if err != nil {
		return nil, fmt.Errorf("invalid email or password %w", ErrInvalidPassword)
	}

	log.Debug(ctx, "user authenticated", log.Int("user_id", u.ID))

	return u, nil
}

// AuthenticateJWTToken validates a JWT token and returns the user.
func (s *AuthService) AuthenticateJWTToken(ctx context.Context, tokenString string) (*ent.User, error) {
	secretKey, err := authz.RunWithSystemBypass(ctx, "auth-get-secret-key", func(bypassCtx context.Context) (string, error) {
		return s.SystemService.SecretKey(bypassCtx)
	})
	if err != nil {
		if errors.Is(err, ErrSystemNotInitialized) {
			return nil, fmt.Errorf("%w: system not initialized", ErrInvalidJWT)
		}
		return nil, fmt.Errorf("failed to get secret key: %w", err)
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method: %v", ErrInvalidJWT, token.Header["alg"])
		}

		return []byte(secretKey), nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse jwt token: %w", ErrInvalidJWT, err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("%w: invalid token", ErrInvalidJWT)
	}

	userID, ok := claims["user_id"].(float64)
	if !ok {
		return nil, fmt.Errorf("%w: invalid token claims", ErrInvalidJWT)
	}

	u, err := authz.RunWithSystemBypass(ctx, "auth-lookup", func(bypassCtx context.Context) (*ent.User, error) {
		return s.UserService.GetUserByID(bypassCtx, int(userID))
	})
	if err != nil {
		return nil, fmt.Errorf("%w: failed to get user: %w", ErrInvalidJWT, err)
	}

	if u.Status != user.StatusActivated {
		return nil, fmt.Errorf("%w: user not activated", ErrInvalidJWT)
	}

	return u, nil
}

func (s *AuthService) AuthenticateAPIKey(ctx context.Context, key string) (*ent.APIKey, error) {
	apiKey, err := authz.RunWithSystemBypass(ctx, "auth-lookup", func(bypassCtx context.Context) (*ent.APIKey, error) {
		return s.APIKeyService.GetAPIKey(bypassCtx, key)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get api key: %w", err)
	}

	if apiKey.Status != apikey.StatusEnabled {
		return nil, fmt.Errorf("api key not enabled: %w", ErrInvalidAPIKey)
	}

	proj, err := apiKey.Project(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get api key project: %w", err)
	}

	if proj == nil || proj.Status != project.StatusActive {
		return nil, fmt.Errorf("api key project not valid: %w", ErrInvalidAPIKey)
	}

	if apiKey.Type == apikey.TypeNoauth {
		return nil, fmt.Errorf("noauth api key is only available when api auth is disabled: %w", ErrInvalidAPIKey)
	}

	return apiKey, nil
}

func (s *AuthService) AuthenticateNoAuth(ctx context.Context) (*ent.APIKey, error) {
	if !s.AllowNoAuth {
		return nil, fmt.Errorf("%w: API key required", ErrInvalidAPIKey)
	}

	apiKey, err := authz.RunWithSystemBypass(ctx, "auth-noauth", func(bypassCtx context.Context) (*ent.APIKey, error) {
		return s.APIKeyService.EnsureNoAuthAPIKey(bypassCtx)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to ensure noauth api key: %w", err)
	}

	if apiKey.Status != apikey.StatusEnabled {
		return nil, fmt.Errorf("api key not enabled: %w", ErrInvalidAPIKey)
	}

	proj, err := apiKey.Project(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get api key project: %w", err)
	}

	if proj == nil || proj.Status != project.StatusActive {
		return nil, fmt.Errorf("api key project not valid: %w", ErrInvalidAPIKey)
	}

	return apiKey, nil
}
