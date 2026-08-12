package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

type AuthHandlersParams struct {
	fx.In

	AuthService    *biz.AuthService
	TrustedProxies []string `name:"trusted_proxies"`
}

func NewAuthHandlers(params AuthHandlersParams) *AuthHandlers {
	trustedProxies := parseTrustedProxyNetworks(params.TrustedProxies)
	if len(trustedProxies) == 0 {
		log.Warn(
			context.Background(),
			"per-client sign-in throttling is disabled because server.trusted_proxies is empty; "+
				"configure it so failed sign-ins can be attributed to a client address",
		)
	}

	return &AuthHandlers{
		AuthService:    params.AuthService,
		trustedProxies: trustedProxies,
		withhold:       withholdSignInFailure,
	}
}

type AuthHandlers struct {
	AuthService *biz.AuthService

	trustedProxies []*net.IPNet
	withhold       func(ctx context.Context, delay time.Duration)
}

// parseTrustedProxyNetworks mirrors how gin interprets the same list. Entries
// that do not parse are dropped here because gin already rejects them while
// building the engine.
func parseTrustedProxyNetworks(values []string) []*net.IPNet {
	networks := make([]*net.IPNet, 0, len(values))

	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		if !strings.Contains(value, "/") {
			ip := net.ParseIP(value)
			if ip == nil {
				continue
			}

			bits := 32
			if ip.To4() == nil {
				bits = 128
			}

			value = fmt.Sprintf("%s/%d", value, bits)
		}

		_, network, err := net.ParseCIDR(value)
		if err != nil {
			continue
		}

		networks = append(networks, network)
	}

	return networks
}

// signInSource returns the client address the sign-in throttle may lock out,
// or an empty string when the address cannot be attributed to a single client.
// Without server.trusted_proxies gin resolves ClientIP to the TCP peer, which
// behind a reverse proxy is the proxy itself and therefore shared by every
// user: locking it out would lock out the whole deployment.
func (h *AuthHandlers) signInSource(c *gin.Context) string {
	if len(h.trustedProxies) == 0 {
		return ""
	}

	ip := net.ParseIP(c.ClientIP())
	if ip == nil {
		return ""
	}

	for _, network := range h.trustedProxies {
		if network.Contains(ip) {
			return ""
		}
	}

	return ip.String()
}

// withholdSignInFailure delays a failed sign-in response for the duration the
// throttle asked for, so repeated guessing gets progressively more expensive.
func withholdSignInFailure(ctx context.Context, delay time.Duration) {
	if delay <= 0 {
		return
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
	}
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

	// Brute-force protection: reject the attempt outright while this client is
	// locked out after repeated failures.
	source := h.signInSource(c)
	if err := h.AuthService.CheckSignInThrottle(req.Email, source); err != nil {
		JSONError(c, http.StatusTooManyRequests, err)
		return
	}

	// Authenticate user
	user, err := h.AuthService.AuthenticateUser(ctx, req.Email, req.Password)
	if err != nil {
		if errors.Is(err, biz.ErrInvalidPassword) {
			// Only failed credential checks are throttled; internal errors
			// must not count, and the delay is charged to the failure response
			// so a correct password is never held back.
			h.withhold(ctx, h.AuthService.RecordSignInFailure(req.Email, source))
			JSONError(c, http.StatusUnauthorized, errors.New("Invalid email or password"))

			return
		}

		JSONError(c, http.StatusInternalServerError, errors.New("Internal server error"))

		return
	}

	h.AuthService.RecordSignInSuccess(req.Email, source)

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

// SignOut revokes every JWT previously issued to the current user ("sign out
// everywhere"). The JWT scheme is stateless, so revocation is implemented with
// a per-user token-valid-after timestamp that is checked on every token
// validation.
func (h *AuthHandlers) SignOut(c *gin.Context) {
	currentUser, ok := contexts.GetUser(c.Request.Context())
	if !ok || currentUser == nil {
		JSONError(c, http.StatusUnauthorized, errors.New("Authentication required"))
		return
	}

	if err := h.AuthService.RevokeUserTokens(c.Request.Context(), currentUser.ID); err != nil {
		JSONError(c, http.StatusInternalServerError, errors.New("Failed to sign out"))
		return
	}

	c.Status(http.StatusNoContent)
}
