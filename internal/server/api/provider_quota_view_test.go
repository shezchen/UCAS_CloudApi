package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/providerquotastatus"
	"github.com/looplj/axonhub/internal/objects"
)

func TestGetProviderQuotaViewAllowsAuthenticatedReadWithoutLeakingChannelSecrets(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := enttest.NewEntClient(t, "sqlite3", "file:provider-quota-view?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	viewer := client.User.Create().
		SetEmail("viewer@mails.ucas.ac.cn").
		SetPassword("hash").
		SaveX(setupCtx)
	donor := client.User.Create().
		SetEmail("donor@mails.ucas.ac.cn").
		SetPassword("hash").
		SaveX(setupCtx)
	campusProject := client.Project.Create().
		SetName("Campus").
		SetStatus(project.StatusActive).
		SaveX(setupCtx)
	client.UserProject.Create().
		SetUserID(viewer.ID).
		SetProjectID(campusProject.ID).
		SaveX(setupCtx)

	visible := client.Channel.Create().
		SetName("共享 Codex").
		SetType(channel.TypeCodex).
		SetStatus(channel.StatusEnabled).
		SetUserID(donor.ID).
		SetBaseURL("https://secret-upstream.example/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "super-secret-api-key"}).
		SetSupportedModels([]string{"gpt-5"}).
		SetDefaultTestModel("gpt-5").
		SaveX(setupCtx)
	disabled := client.Channel.Create().
		SetName("Disabled").
		SetType(channel.TypeCodex).
		SetStatus(channel.StatusDisabled).
		SetUserID(donor.ID).
		SetCredentials(objects.ChannelCredentials{APIKey: "disabled-secret"}).
		SetSupportedModels([]string{"gpt-5"}).
		SetDefaultTestModel("gpt-5").
		SaveX(setupCtx)

	nextReset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	for _, channelID := range []int{visible.ID, disabled.ID} {
		client.ProviderQuotaStatus.Create().
			SetChannelID(channelID).
			SetProviderType(providerquotastatus.ProviderTypeCodex).
			SetStatus(providerquotastatus.StatusWarning).
			SetReady(true).
			SetNextResetAt(nextReset).
			SetNextCheckAt(time.Now().Add(time.Minute)).
			SetQuotaData(map[string]any{
				"plan_type": "student",
				"rate_limit": map[string]any{
					"primary_window": map[string]any{
						"used_percent": 42,
						"x_api_key":    "nested-x-api-key",
						"headers": map[string]any{
							"Authorization": "Bearer nested-authorization-secret",
						},
					},
				},
				"error":         "request to https://secret-upstream.example failed with super-secret-api-key",
				"access_token":  "provider-access-token",
				"workspace_id":  "private-workspace",
				"documentation": "https://secret-upstream.example/quota",
				"x_api_key":     "top-level-x-api-key",
				"client_secret": "top-level-client-secret",
				"password":      "top-level-password",
				"headers": map[string]any{
					"Authorization": "Bearer top-level-authorization-secret",
				},
				"nested": map[string]any{
					"id":    "credential-id",
					"email": "owner@example.com",
					"items": []any{
						map[string]any{
							"token": "nested-secret-token",
							"safe":  "visible",
						},
					},
				},
			}).
			SaveX(setupCtx)
	}

	requestCtx := ent.NewContext(context.Background(), client)
	requestCtx = contexts.WithUser(requestCtx, viewer)
	requestCtx = contexts.WithProjectID(requestCtx, campusProject.ID)
	var err error
	requestCtx, err = authz.WithPrincipal(requestCtx, authz.Principal{
		Type:   authz.PrincipalTypeUser,
		UserID: &viewer.ID,
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/admin/provider-quotas", nil).WithContext(requestCtx)

	(&SystemHandlers{}).GetProviderQuotaView(ginCtx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "private, no-store", recorder.Header().Get("Cache-Control"))
	require.NotContains(t, recorder.Body.String(), "secret-upstream")
	require.NotContains(t, recorder.Body.String(), "super-secret")
	require.NotContains(t, recorder.Body.String(), "provider-access-token")
	require.NotContains(t, recorder.Body.String(), "private-workspace")
	require.NotContains(t, recorder.Body.String(), "credential-id")
	require.NotContains(t, recorder.Body.String(), "owner@example.com")
	require.NotContains(t, recorder.Body.String(), "nested-secret-token")
	require.NotContains(t, recorder.Body.String(), "nested-x-api-key")
	require.NotContains(t, recorder.Body.String(), "nested-authorization-secret")
	require.NotContains(t, recorder.Body.String(), "top-level-x-api-key")
	require.NotContains(t, recorder.Body.String(), "top-level-client-secret")
	require.NotContains(t, recorder.Body.String(), "top-level-password")
	require.NotContains(t, recorder.Body.String(), "top-level-authorization-secret")
	require.NotContains(t, recorder.Body.String(), "credentials")
	require.NotContains(t, recorder.Body.String(), "baseURL")

	var response ProviderQuotaViewResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Len(t, response.Channels, 1)
	require.Equal(t, "共享 Codex", response.Channels[0].Name)
	require.Equal(t, "codex", response.Channels[0].Type)
	require.Equal(t, "codex", response.Channels[0].ProviderQuota.ProviderType)
	require.Equal(t, "warning", response.Channels[0].ProviderQuota.Status)
	require.Equal(t, "student", response.Channels[0].ProviderQuota.QuotaData["plan_type"])
	require.NotContains(t, response.Channels[0].ProviderQuota.QuotaData, "error")
	require.NotContains(t, response.Channels[0].ProviderQuota.QuotaData, "access_token")
	require.NotContains(t, response.Channels[0].ProviderQuota.QuotaData, "workspace_id")
	require.NotContains(t, response.Channels[0].ProviderQuota.QuotaData, "documentation")
}

func TestGetProviderQuotaViewRequiresAuthenticatedUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/admin/provider-quotas", nil)

	(&SystemHandlers{}).GetProviderQuotaView(ginCtx)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "providerQuotaStatus")
}

func TestSanitizeProviderQuotaDataUsesRecursiveAllowlistAndRejectsSecretText(t *testing.T) {
	sanitized := sanitizeProviderQuotaData("codex", map[string]any{
		"plan_type": "safe-looking-token-value",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent":         37.5,
				"reset_at":             1_722_000_000,
				"reset_after_seconds":  120,
				"limit_window_seconds": 18_000,
				"x_api_key":            "nested-x-api-key",
				"client_secret":        "nested-client-secret",
				"password":             "nested-password",
				"headers": map[string]any{
					"Authorization": "Bearer nested-authorization-secret",
				},
			},
			"unknown_window": map[string]any{
				"used_percent": 99,
			},
		},
		"x_api_key":     "top-level-x-api-key",
		"client_secret": "top-level-client-secret",
		"password":      "top-level-password",
		"headers": map[string]any{
			"Authorization": "Bearer top-level-authorization-secret",
		},
		"unknown_status": "available",
	})

	require.NotContains(t, sanitized, "plan_type")
	require.NotContains(t, sanitized, "x_api_key")
	require.NotContains(t, sanitized, "client_secret")
	require.NotContains(t, sanitized, "password")
	require.NotContains(t, sanitized, "headers")
	require.NotContains(t, sanitized, "unknown_status")

	rateLimit := sanitized["rate_limit"].(map[string]any)
	require.NotContains(t, rateLimit, "unknown_window")
	primary := rateLimit["primary_window"].(map[string]any)
	require.Equal(t, 37.5, primary["used_percent"])
	require.Equal(t, 1_722_000_000, primary["reset_at"])
	require.Equal(t, 120, primary["reset_after_seconds"])
	require.Equal(t, 18_000, primary["limit_window_seconds"])
	require.NotContains(t, primary, "x_api_key")
	require.NotContains(t, primary, "client_secret")
	require.NotContains(t, primary, "password")
	require.NotContains(t, primary, "headers")

	encoded, err := json.Marshal(sanitized)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "token")
	require.NotContains(t, string(encoded), "secret")
	require.NotContains(t, string(encoded), "Authorization")
}

func TestSanitizeProviderQuotaDataPreservesOnlyKnownDisplayStatistics(t *testing.T) {
	sanitized := sanitizeProviderQuotaData("neuralwatt", map[string]any{
		"plan_type": "student",
		"balance": map[string]any{
			"credits_remaining_usd": 12.5,
			"total_credits_usd":     20.0,
			"account_id":            "private-account",
		},
		"subscription": map[string]any{
			"kwh_included":   100,
			"kwh_used":       25,
			"kwh_remaining":  75,
			"in_overage":     false,
			"status":         "active",
			"plan":           "campus",
			"kwh_reset_date": "2026-08-01T00:00:00Z",
			"api_key":        "private-api-key",
		},
		"provider_payload": map[string]any{"status": "available"},
	})

	require.Equal(t, "student", sanitized["plan_type"])
	require.NotContains(t, sanitized, "provider_payload")

	balance := sanitized["balance"].(map[string]any)
	require.Equal(t, 12.5, balance["credits_remaining_usd"])
	require.Equal(t, 20.0, balance["total_credits_usd"])
	require.NotContains(t, balance, "account_id")

	subscription := sanitized["subscription"].(map[string]any)
	require.Equal(t, "active", subscription["status"])
	require.Equal(t, "campus", subscription["plan"])
	require.Equal(t, "2026-08-01T00:00:00Z", subscription["kwh_reset_date"])
	require.NotContains(t, subscription, "api_key")

	require.Empty(t, sanitizeProviderQuotaData("unknown-provider", map[string]any{
		"plan_type": "student",
		"balance":   map[string]any{"credits_remaining_usd": 12.5},
	}))
}

func TestGetProviderQuotaViewRejectsNonMemberProject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := enttest.NewEntClient(t, "sqlite3", "file:provider-quota-project-boundary?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	viewer := client.User.Create().
		SetEmail("outsider@mails.ucas.ac.cn").
		SetPassword("hash").
		SaveX(setupCtx)
	campusProject := client.Project.Create().
		SetName("Members only").
		SetStatus(project.StatusActive).
		SaveX(setupCtx)

	requestCtx := ent.NewContext(context.Background(), client)
	requestCtx = contexts.WithUser(requestCtx, viewer)
	requestCtx = contexts.WithProjectID(requestCtx, campusProject.ID)
	var err error
	requestCtx, err = authz.WithPrincipal(requestCtx, authz.Principal{
		Type:   authz.PrincipalTypeUser,
		UserID: &viewer.ID,
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/admin/provider-quotas", nil).WithContext(requestCtx)

	(&SystemHandlers{}).GetProviderQuotaView(ginCtx)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "providerQuotaStatus")
}
