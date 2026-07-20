package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestWithAPIKeyConfig_RejectsNoAuthKeyWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(WithAPIKeyConfig(&biz.AuthService{}, nil))
	router.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+biz.NoAuthAPIKeyValue)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, recorder.Code)
	}
}

func TestWithAPIKeyConfig_AllowsMissingAuthorizationWhenNoAuthAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		key, err := ExtractAPIKeyFromRequest(c.Request, &APIKeyConfig{
			Headers:       []string{"Authorization"},
			RequireBearer: true,
		})
		if errors.Is(err, ErrAPIKeyRequired) {
			c.Status(http.StatusNoContent)
			c.Abort()

			return
		}

		if err != nil || key != "" {
			c.Status(http.StatusTeapot)
			c.Abort()

			return
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, recorder.Code)
	}
}

func TestWithOwnerOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		user       *ent.User
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "owner allowed",
			user:       &ent.User{ID: 1, IsOwner: true},
			wantStatus: http.StatusNoContent,
			wantCalled: true,
		},
		{
			name:       "non-owner forbidden",
			user:       &ent.User{ID: 2},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "missing authenticated user",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			router := gin.New()
			router.Use(func(c *gin.Context) {
				if tt.user != nil {
					ctx := contexts.WithUser(c.Request.Context(), tt.user)
					c.Request = c.Request.WithContext(ctx)
				}
				c.Next()
			})
			router.POST("/playground", WithOwnerOnly(), func(c *gin.Context) {
				called = true
				c.Status(http.StatusNoContent)
			})

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/playground", nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d", tt.wantStatus, recorder.Code)
			}
			if called != tt.wantCalled {
				t.Fatalf("expected handler called=%t, got %t", tt.wantCalled, called)
			}
		})
	}
}
