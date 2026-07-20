package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	servermail "github.com/looplj/axonhub/internal/server/mail"
)

type apiCapturedVerification struct {
	to   string
	code string
	ttl  time.Duration
}

type apiVerificationSender struct {
	mu       sync.Mutex
	messages []apiCapturedVerification
	sendErr  error
}

var _ servermail.VerificationSender = (*apiVerificationSender)(nil)

func (s *apiVerificationSender) SendVerificationCode(_ context.Context, to, code string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sendErr != nil {
		return s.sendErr
	}
	s.messages = append(s.messages, apiCapturedVerification{to: to, code: code, ttl: ttl})

	return nil
}

func (s *apiVerificationSender) latestCode(t *testing.T, email string) string {
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

type campusSignUpFixture struct {
	handler  *AuthHandlers
	client   *ent.Client
	setupCtx context.Context
	sender   *apiVerificationSender
}

func setupCampusSignUpHandler(
	t *testing.T,
	createActiveProject bool,
	config biz.CampusEmailVerificationConfig,
) *campusSignUpFixture {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:campus-signup?mode=memory&_fk=1")
	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	userService := biz.NewUserService(biz.UserServiceParams{
		CacheConfig: cacheConfig,
		Ent:         client,
	})
	systemService := biz.NewSystemService(biz.SystemServiceParams{
		CacheConfig: cacheConfig,
		Ent:         client,
	})
	sender := &apiVerificationSender{}
	authService := biz.NewAuthService(biz.AuthServiceParams{
		SystemService:           systemService,
		UserService:             userService,
		Ent:                     client,
		EmailVerificationConfig: config,
		VerificationSender:      sender,
	})
	setupCtx := ent.NewContext(authz.WithTestBypass(t.Context()), client)

	secretKey, err := biz.GenerateSecretKey()
	require.NoError(t, err)
	require.NoError(t, systemService.SetSecretKey(setupCtx, secretKey))

	if createActiveProject {
		_, err = client.Project.Create().
			SetName("Campus Sharing").
			SetStatus(project.StatusActive).
			Save(setupCtx)
		require.NoError(t, err)
	}

	return &campusSignUpFixture{
		handler:  NewAuthHandlers(AuthHandlersParams{AuthService: authService}),
		client:   client,
		setupCtx: setupCtx,
		sender:   sender,
	}
}

func performCampusVerification(t *testing.T, handler *AuthHandlers, email, sourceIP string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(SignUpVerificationRequest{Email: email})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/auth/signup/verification", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.RemoteAddr = sourceIP + ":42517"

	handler.RequestSignUpVerification(c)

	return recorder
}

func performCampusSignUp(
	t *testing.T,
	handler *AuthHandlers,
	email, password, nickname, verificationCode string,
) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(SignUpRequest{
		Email:            email,
		Password:         password,
		Nickname:         nickname,
		VerificationCode: verificationCode,
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/auth/signup", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SignUp(c)

	return recorder
}

func decodeAPIError(t *testing.T, recorder *httptest.ResponseRecorder) objects.ErrorResponse {
	t.Helper()

	var response objects.ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))

	return response
}

func TestAuthHandlers_TwoPhaseCampusSignUp(t *testing.T) {
	fixture := setupCampusSignUpHandler(t, true, biz.CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	email := "api-student@mails.ucas.ac.cn"
	verification := performCampusVerification(t, fixture.handler, email, "198.51.100.40")
	require.Equal(t, http.StatusAccepted, verification.Code, verification.Body.String())
	var verificationResponse SignUpVerificationResponse
	require.NoError(t, json.Unmarshal(verification.Body.Bytes(), &verificationResponse))
	require.Equal(t, "If the address can be used for registration, a verification email has been sent.", verificationResponse.Message)

	code := fixture.sender.latestCode(t, email)
	registered := performCampusSignUp(t, fixture.handler, email, "password-123", "  星河同学  ", code)
	require.Equal(t, http.StatusCreated, registered.Code, registered.Body.String())

	var response SignInResponse
	require.NoError(t, json.Unmarshal(registered.Body.Bytes(), &response))
	require.NotNil(t, response.User)
	require.Equal(t, email, response.User.Email)
	require.Equal(t, "星河同学", response.User.Nickname)
	require.False(t, response.User.IsOwner)
	require.NotEmpty(t, response.Token)

	created, err := fixture.client.User.Query().
		Where(user.EmailEQ(email)).
		Only(fixture.setupCtx)
	require.NoError(t, err)
	require.Equal(t, "星河同学", created.Nickname)
	require.Equal(t, int64(200_000_000), created.DailyTokenLimit)

	defaultProject, err := fixture.client.Project.Query().Only(fixture.setupCtx)
	require.NoError(t, err)
	membership, err := fixture.client.UserProject.Query().
		Where(userproject.UserIDEQ(created.ID)).
		Only(fixture.setupCtx)
	require.NoError(t, err)
	require.Equal(t, defaultProject.ID, membership.ProjectID)
	require.False(t, membership.IsOwner)
	require.ElementsMatch(t, []string{"read_api_keys", "write_api_keys"}, membership.Scopes)
}

func TestAuthHandlers_RequestSignUpVerification_StatusMapping(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		fixture := setupCampusSignUpHandler(t, true, biz.CampusEmailVerificationConfig{})
		defer fixture.client.Close()

		response := performCampusVerification(t, fixture.handler, "accepted@ucas.ac.cn", "198.51.100.41")
		require.Equal(t, http.StatusAccepted, response.Code)
	})

	t.Run("rate limited", func(t *testing.T) {
		fixture := setupCampusSignUpHandler(t, true, biz.CampusEmailVerificationConfig{
			EmailHourlyLimit: 1,
		})
		defer fixture.client.Close()

		email := "rate-limit@ucas.ac.cn"
		first := performCampusVerification(t, fixture.handler, email, "198.51.100.42")
		require.Equal(t, http.StatusAccepted, first.Code)
		limited := performCampusVerification(t, fixture.handler, email, "198.51.100.42")
		require.Equal(t, http.StatusTooManyRequests, limited.Code)
		require.Equal(t, biz.ErrVerificationRateLimit.Error(), decodeAPIError(t, limited).Error.Message)
	})

	t.Run("smtp unavailable", func(t *testing.T) {
		fixture := setupCampusSignUpHandler(t, true, biz.CampusEmailVerificationConfig{})
		defer fixture.client.Close()
		fixture.sender.sendErr = errors.New("smtp unavailable")

		response := performCampusVerification(t, fixture.handler, "smtp-error@ucas.edu.cn", "198.51.100.43")
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Equal(t, biz.ErrVerificationUnavailable.Error(), decodeAPIError(t, response).Error.Message)
	})
}

func TestAuthHandlers_SignUp_InvalidVerificationIsGeneric(t *testing.T) {
	fixture := setupCampusSignUpHandler(t, true, biz.CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	response := performCampusSignUp(
		t,
		fixture.handler,
		"unverified@mails.ucas.edu.cn",
		"password-123",
		"未验证同学",
		"000000",
	)
	require.Equal(t, http.StatusBadRequest, response.Code)
	errorResponse := decodeAPIError(t, response)
	require.Equal(t, biz.ErrVerificationInvalid.Error(), errorResponse.Error.Message)
	require.NotContains(t, errorResponse.Error.Message, "registered")
}

func TestAuthHandlers_SignUp_DuplicateAccountDoesNotLeakExistence(t *testing.T) {
	fixture := setupCampusSignUpHandler(t, true, biz.CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	hashedPassword, err := biz.HashPassword("existing-password")
	require.NoError(t, err)
	_, err = fixture.client.User.Create().
		SetEmail("existing@ucas.ac.cn").
		SetPassword(hashedPassword).
		Save(fixture.setupCtx)
	require.NoError(t, err)

	availableRequest := performCampusVerification(t, fixture.handler, "available@ucas.ac.cn", "198.51.100.44")
	existingRequest := performCampusVerification(t, fixture.handler, "existing@ucas.ac.cn", "198.51.100.45")
	require.Equal(t, http.StatusAccepted, availableRequest.Code)
	require.Equal(t, http.StatusAccepted, existingRequest.Code)
	require.JSONEq(t, availableRequest.Body.String(), existingRequest.Body.String())

	code := fixture.sender.latestCode(t, "existing@ucas.ac.cn")
	response := performCampusSignUp(t, fixture.handler, "existing@ucas.ac.cn", "password-123", "已有账户同学", code)
	require.Equal(t, http.StatusBadRequest, response.Code)
	errorResponse := decodeAPIError(t, response)
	require.Equal(t, biz.ErrVerificationInvalid.Error(), errorResponse.Error.Message)
	require.NotContains(t, errorResponse.Error.Message, "already")
	require.NotContains(t, errorResponse.Error.Message, "registered")
}

func TestAuthHandlers_RequestSignUpVerification_RejectsNonUCASEmail(t *testing.T) {
	fixture := setupCampusSignUpHandler(t, true, biz.CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	response := performCampusVerification(t, fixture.handler, "student@example.com", "198.51.100.46")
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, decodeAPIError(t, response).Error.Message, biz.ErrCampusEmailRequired.Error())
}

func TestAuthHandlers_SignUp_FailsWithoutActiveProject(t *testing.T) {
	fixture := setupCampusSignUpHandler(t, false, biz.CampusEmailVerificationConfig{})
	defer fixture.client.Close()

	_, err := fixture.client.Project.Create().
		SetName("Archived").
		SetStatus(project.StatusArchived).
		Save(fixture.setupCtx)
	require.NoError(t, err)

	email := "no-project@mails.ucas.edu.cn"
	verification := performCampusVerification(t, fixture.handler, email, "198.51.100.47")
	require.Equal(t, http.StatusAccepted, verification.Code)
	code := fixture.sender.latestCode(t, email)
	response := performCampusSignUp(t, fixture.handler, email, "password-123", "无项目同学", code)
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.Equal(t, "Failed to create account", decodeAPIError(t, response).Error.Message)

	count, err := fixture.client.User.Query().
		Where(user.EmailEQ(email)).
		Count(fixture.setupCtx)
	require.NoError(t, err)
	require.Zero(t, count, "the failed registration must not leave an orphaned user")
}
