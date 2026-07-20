package biz

import (
	"errors"

	"github.com/looplj/axonhub/llm/transformer"
)

var (
	ErrInvalidJWT              = errors.New("invalid jwt token")
	ErrInvalidToken            = errors.New("invalid token")
	ErrInvalidAPIKey           = errors.New("invalid api key")
	ErrInvalidPassword         = errors.New("invalid password")
	ErrInvalidModel            = transformer.ErrInvalidModel
	ErrInternal                = errors.New("server internal error, please try again later")
	ErrAPIKeyOwnerRequired     = errors.New("owner api key is required")
	ErrServiceAccountRequired  = errors.New("service account api key required")
	ErrAPIKeyScopeRequired     = errors.New("api key missing required scope")
	ErrAPIKeyNameRequired      = errors.New("api key name is required")
	ErrSystemNotInitialized    = errors.New("system not initialized")
	ErrOIDCLoginRequired       = errors.New("OIDC user without password, please login via OIDC or set a password")
	ErrProjectNotFound         = errors.New("project not found")
	ErrCampusEmailRequired     = errors.New("registration requires a UCAS email address")
	ErrEmailAlreadyRegistered  = errors.New("email is already registered")
	ErrInvalidNickname         = errors.New("nickname is invalid")
	ErrVerificationInvalid     = errors.New("verification code is invalid or expired")
	ErrVerificationRateLimit   = errors.New("verification email requested too frequently")
	ErrVerificationUnavailable = errors.New("email verification is temporarily unavailable")
)
