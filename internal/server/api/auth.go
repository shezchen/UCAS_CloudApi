package api

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

type AuthHandlersParams struct {
	fx.In

	AuthService *biz.AuthService
}

func NewAuthHandlers(params AuthHandlersParams) *AuthHandlers {
	return &AuthHandlers{
		AuthService: params.AuthService,
	}
}

type AuthHandlers struct {
	AuthService *biz.AuthService
}

// SignInRequest 登录请求.
type SignInRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// SignInResponse 登录响应.
type SignInResponse struct {
	User  *objects.UserInfo `json:"user"`
	Token string            `json:"token"`
}

// SignUpRequest is the fixed-shape public campus registration request.
type SignUpRequest struct {
	Email            string `json:"email"            binding:"required,email"`
	Password         string `json:"password"         binding:"required,min=8"`
	Nickname         string `json:"nickname"`
	VerificationCode string `json:"verificationCode" binding:"required"`
}

type SignUpVerificationRequest struct {
	Email string `json:"email" binding:"required,email"`
}

type SignUpVerificationResponse struct {
	Message string `json:"message"`
}

// RequestSignUpVerification sends a rate-limited verification code. The
// success response intentionally does not disclose whether an account exists.
func (h *AuthHandlers) RequestSignUpVerification(c *gin.Context) {
	var req SignUpVerificationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid verification request"))
		return
	}

	err := h.AuthService.RequestCampusEmailVerification(c.Request.Context(), req.Email, c.ClientIP())
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrCampusEmailRequired):
			JSONError(c, http.StatusBadRequest, err)
		case errors.Is(err, biz.ErrVerificationRateLimit):
			JSONError(c, http.StatusTooManyRequests, biz.ErrVerificationRateLimit)
		case errors.Is(err, biz.ErrVerificationUnavailable):
			JSONError(c, http.StatusServiceUnavailable, biz.ErrVerificationUnavailable)
		default:
			JSONError(c, http.StatusInternalServerError, errors.New("Failed to request verification email"))
		}
		return
	}

	c.JSON(http.StatusAccepted, SignUpVerificationResponse{
		Message: "If the address can be used for registration, a verification email has been sent.",
	})
}

// SignUp creates a non-owner member account restricted to UCAS email domains.
func (h *AuthHandlers) SignUp(c *gin.Context) {
	var req SignUpRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid registration request"))
		return
	}

	ctx := c.Request.Context()
	user, err := h.AuthService.RegisterCampusUser(ctx, req.Email, req.Password, req.Nickname, req.VerificationCode)
	if err != nil {
		switch {
		case errors.Is(err, biz.ErrCampusEmailRequired), errors.Is(err, biz.ErrInvalidNickname):
			JSONError(c, http.StatusBadRequest, err)
		case errors.Is(err, biz.ErrVerificationInvalid):
			JSONError(c, http.StatusBadRequest, biz.ErrVerificationInvalid)
		case errors.Is(err, biz.ErrVerificationUnavailable):
			JSONError(c, http.StatusServiceUnavailable, biz.ErrVerificationUnavailable)
		default:
			JSONError(c, http.StatusInternalServerError, errors.New("Failed to create account"))
		}

		return
	}

	token, err := h.AuthService.GenerateJWTToken(ctx, user)
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Failed to create account"))
		return
	}

	c.JSON(http.StatusCreated, SignInResponse{
		User:  biz.ConvertUserToUserInfo(ctx, user),
		Token: token,
	})
}

// SignIn handles user authentication.
func (h *AuthHandlers) SignIn(c *gin.Context) {
	var (
		ctx = c.Request.Context()
		req SignInRequest
	)

	err := c.ShouldBindJSON(&req)
	if err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	// Authenticate user
	user, err := h.AuthService.AuthenticateUser(ctx, req.Email, req.Password)
	if err != nil {
		if errors.Is(err, biz.ErrInvalidPassword) {
			JSONError(c, http.StatusUnauthorized, errors.New("Invalid email or password"))
			return
		}

		JSONError(c, http.StatusInternalServerError, errors.New("Internal server error"))

		return
	}

	// Generate JWT token
	token, err := h.AuthService.GenerateJWTToken(ctx, user)
	if err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Internal server error"))
		return
	}

	response := SignInResponse{
		User:  biz.ConvertUserToUserInfo(ctx, user),
		Token: token,
	}

	c.JSON(http.StatusOK, response)
}
