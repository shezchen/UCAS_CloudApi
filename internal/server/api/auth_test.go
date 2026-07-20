package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
)

func setupCampusSignUpHandler(t *testing.T, createActiveProject bool) (*AuthHandlers, *ent.Client, context.Context) {
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
	authService := biz.NewAuthService(biz.AuthServiceParams{
		SystemService: systemService,
		UserService:   userService,
		Ent:           client,
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

	return NewAuthHandlers(AuthHandlersParams{AuthService: authService}), client, setupCtx
}

func performCampusSignUp(t *testing.T, handler *AuthHandlers, email, password string) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(SignUpRequest{Email: email, Password: password})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/auth/signup", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.SignUp(c)

	return recorder
}

func TestAuthHandlers_SignUp_AllowsUCASDomainsAndCreatesCampusMembers(t *testing.T) {
	handler, client, setupCtx := setupCampusSignUpHandler(t, true)
	defer client.Close()

	defaultProject, err := client.Project.Query().Only(setupCtx)
	require.NoError(t, err)

	testCases := []struct {
		name            string
		email           string
		normalizedEmail string
	}{
		{name: "student ac cn", email: "api-student@mails.ucas.ac.cn", normalizedEmail: "api-student@mails.ucas.ac.cn"},
		{name: "staff ac cn", email: "api-staff@UCAS.AC.CN", normalizedEmail: "api-staff@ucas.ac.cn"},
		{name: "student edu cn", email: "api-student@mails.ucas.edu.cn", normalizedEmail: "api-student@mails.ucas.edu.cn"},
		{name: "staff edu cn", email: "api-staff@ucas.edu.cn", normalizedEmail: "api-staff@ucas.edu.cn"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := performCampusSignUp(t, handler, tc.email, "password-123")
			require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

			var response SignInResponse
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			require.NotNil(t, response.User)
			require.Equal(t, tc.normalizedEmail, response.User.Email)
			require.False(t, response.User.IsOwner)
			require.NotEmpty(t, response.Token)

			created, err := client.User.Query().
				Where(user.EmailEQ(tc.normalizedEmail)).
				Only(setupCtx)
			require.NoError(t, err)
			require.False(t, created.IsOwner)
			require.Equal(t, int64(200_000_000), created.DailyTokenLimit)

			membership, err := client.UserProject.Query().
				Where(userproject.UserIDEQ(created.ID)).
				Only(setupCtx)
			require.NoError(t, err)
			require.Equal(t, defaultProject.ID, membership.ProjectID)
			require.False(t, membership.IsOwner)
			require.ElementsMatch(t, []string{"read_api_keys", "write_api_keys"}, membership.Scopes)
		})
	}
}

func TestAuthHandlers_SignUp_RejectsNonUCASEmail(t *testing.T) {
	handler, client, _ := setupCampusSignUpHandler(t, true)
	defer client.Close()

	recorder := performCampusSignUp(t, handler, "student@example.com", "password-123")
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	var response objects.ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Contains(t, response.Error.Message, biz.ErrCampusEmailRequired.Error())
}

func TestAuthHandlers_SignUp_MapsDuplicateEmailToConflict(t *testing.T) {
	handler, client, _ := setupCampusSignUpHandler(t, true)
	defer client.Close()

	first := performCampusSignUp(t, handler, "duplicate@ucas.ac.cn", "password-123")
	require.Equal(t, http.StatusCreated, first.Code, first.Body.String())

	duplicate := performCampusSignUp(t, handler, "DUPLICATE@UCAS.AC.CN", "password-456")
	require.Equal(t, http.StatusConflict, duplicate.Code)

	var response objects.ErrorResponse
	require.NoError(t, json.Unmarshal(duplicate.Body.Bytes(), &response))
	require.Contains(t, response.Error.Message, biz.ErrEmailAlreadyRegistered.Error())
}

func TestAuthHandlers_SignUp_FailsWithoutActiveProject(t *testing.T) {
	handler, client, setupCtx := setupCampusSignUpHandler(t, false)
	defer client.Close()

	_, err := client.Project.Create().
		SetName("Archived").
		SetStatus(project.StatusArchived).
		Save(setupCtx)
	require.NoError(t, err)

	recorder := performCampusSignUp(t, handler, "no-project@mails.ucas.edu.cn", "password-123")
	require.Equal(t, http.StatusInternalServerError, recorder.Code)

	var response objects.ErrorResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, "Failed to create account", response.Error.Message)

	count, err := client.User.Query().
		Where(user.EmailEQ("no-project@mails.ucas.edu.cn")).
		Count(setupCtx)
	require.NoError(t, err)
	require.Zero(t, count, "the failed registration must not leave an orphaned user")
}
