package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/providerquotastatus"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/log"
)

// ProviderQuotaViewResponse is the intentionally narrow, read-only projection
// exposed to authenticated users. Channel configuration and credentials never
// enter this response type.
type ProviderQuotaViewResponse struct {
	Channels []ProviderQuotaViewChannel `json:"channels"`
}

type ProviderQuotaViewChannel struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Type          string                  `json:"type"`
	ProviderQuota ProviderQuotaViewStatus `json:"providerQuotaStatus"`
}

type ProviderQuotaViewStatus struct {
	Status       string         `json:"status"`
	NextResetAt  *time.Time     `json:"nextResetAt"`
	Ready        bool           `json:"ready"`
	QuotaData    map[string]any `json:"quotaData"`
	ProviderType string         `json:"providerType"`
}

type providerQuotaViewQueryResult struct {
	allowed  bool
	statuses []*ent.ProviderQuotaStatus
}

// GetProviderQuotaView returns quota telemetry for enabled, unexpired channels
// to any JWT-authenticated user. It deliberately bypasses channel ownership only
// inside the database read, then copies data into a projection that contains no
// channel settings, URLs, credentials, ownership, or mutation capability.
func (h *SystemHandlers) GetProviderQuotaView(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	ctx := c.Request.Context()
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		JSONError(c, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}
	projectID, ok := contexts.GetProjectID(ctx)
	if !ok || projectID <= 0 {
		JSONError(c, http.StatusBadRequest, errors.New("project is required"))
		return
	}

	now := time.Now()
	result, err := authz.RunWithSystemBypass(ctx, "authenticated-provider-quota-view", func(bypassCtx context.Context) (providerQuotaViewQueryResult, error) {
		client := ent.FromContext(bypassCtx)
		activeProject, err := client.Project.Query().
			Where(project.IDEQ(projectID), project.DeletedAtEQ(0), project.StatusEQ(project.StatusActive)).
			Exist(bypassCtx)
		if err != nil || !activeProject {
			return providerQuotaViewQueryResult{}, err
		}

		if !currentUser.IsOwner {
			member, err := client.UserProject.Query().
				Where(userproject.UserIDEQ(currentUser.ID), userproject.ProjectIDEQ(projectID)).
				Exist(bypassCtx)
			if err != nil || !member {
				return providerQuotaViewQueryResult{}, err
			}
		}

		statuses, err := client.ProviderQuotaStatus.Query().
			Where(
				providerquotastatus.DeletedAtEQ(0),
				providerquotastatus.HasChannelWith(
					channel.DeletedAtEQ(0),
					channel.StatusEQ(channel.StatusEnabled),
					channel.Or(channel.ExpiresAtIsNil(), channel.ExpiresAtGT(now)),
				),
			).
			WithChannel(func(query *ent.ChannelQuery) {
				query.Select(channel.FieldID, channel.FieldName, channel.FieldType, channel.FieldStatus)
			}).
			Order(ent.Asc(providerquotastatus.FieldChannelID)).
			All(bypassCtx)
		return providerQuotaViewQueryResult{allowed: true, statuses: statuses}, err
	})
	if err != nil {
		log.Error(ctx, "failed to load authenticated provider quota view", log.Cause(err))
		JSONError(c, http.StatusInternalServerError, errors.New("failed to load provider quotas"))
		return
	}
	if !result.allowed {
		JSONError(c, http.StatusForbidden, errors.New("project access denied"))
		return
	}

	response := ProviderQuotaViewResponse{Channels: make([]ProviderQuotaViewChannel, 0, len(result.statuses))}
	for _, quotaStatus := range result.statuses {
		ch := quotaStatus.Edges.Channel
		if ch == nil || quotaHasNoCredentials(quotaStatus.QuotaData) {
			continue
		}

		response.Channels = append(response.Channels, ProviderQuotaViewChannel{
			ID:   "gid://axonhub/" + ent.TypeChannel + "/" + strconv.Itoa(ch.ID),
			Name: ch.Name,
			Type: ch.Type.String(),
			ProviderQuota: ProviderQuotaViewStatus{
				Status:       quotaStatus.Status.String(),
				NextResetAt:  quotaStatus.NextResetAt,
				Ready:        quotaStatus.Ready,
				QuotaData:    sanitizeProviderQuotaData(quotaStatus.ProviderType.String(), quotaStatus.QuotaData),
				ProviderType: quotaStatus.ProviderType.String(),
			},
		})
	}

	c.JSON(http.StatusOK, response)
}

func quotaHasNoCredentials(data map[string]any) bool {
	raw, ok := data["error"].(string)
	if !ok {
		return false
	}

	message := strings.ToLower(raw)
	return strings.Contains(message, "no credentials") ||
		strings.Contains(message, "no api key") ||
		strings.Contains(message, "missing api key")
}

type providerQuotaFieldKind uint8

const (
	providerQuotaText providerQuotaFieldKind = iota + 1
	providerQuotaNumber
	providerQuotaBool
	providerQuotaNumericText
	providerQuotaObject
)

type providerQuotaFieldSpec struct {
	kind   providerQuotaFieldKind
	fields map[string]providerQuotaFieldSpec
}

var (
	providerQuotaTextField        = providerQuotaFieldSpec{kind: providerQuotaText}
	providerQuotaNumberField      = providerQuotaFieldSpec{kind: providerQuotaNumber}
	providerQuotaBoolField        = providerQuotaFieldSpec{kind: providerQuotaBool}
	providerQuotaNumericTextField = providerQuotaFieldSpec{kind: providerQuotaNumericText}

	providerQuotaClaudeWindow = providerQuotaObjectField(map[string]providerQuotaFieldSpec{
		"utilization": providerQuotaNumberField,
		"reset":       providerQuotaNumberField,
		"status":      providerQuotaTextField,
	})
	providerQuotaCodexWindow = providerQuotaObjectField(map[string]providerQuotaFieldSpec{
		"used_percent":         providerQuotaNumberField,
		"reset_at":             providerQuotaNumberField,
		"reset_after_seconds":  providerQuotaNumberField,
		"limit_window_seconds": providerQuotaNumberField,
	})
	providerQuotaCopilotSnapshot = providerQuotaObjectField(map[string]providerQuotaFieldSpec{
		"entitlement":       providerQuotaNumberField,
		"has_quota":         providerQuotaBoolField,
		"overage_count":     providerQuotaNumberField,
		"overage_permitted": providerQuotaBoolField,
		"percent_remaining": providerQuotaNumberField,
		"quota_remaining":   providerQuotaNumberField,
		"quota_reset_at":    providerQuotaNumberField,
		"remaining":         providerQuotaNumberField,
		"timestamp_utc":     providerQuotaTextField,
		"unlimited":         providerQuotaBoolField,
	})
	providerQuotaNanoGPTWindow = providerQuotaObjectField(map[string]providerQuotaFieldSpec{
		"used":        providerQuotaNumberField,
		"remaining":   providerQuotaNumberField,
		"percentUsed": providerQuotaNumberField,
		"resetAt":     providerQuotaNumberField,
	})
	providerQuotaOpenCodeWindow = providerQuotaObjectField(map[string]providerQuotaFieldSpec{
		"usage_percent":     providerQuotaNumberField,
		"reset_in_seconds":  providerQuotaNumberField,
		"reset_time":        providerQuotaTextField,
		"status":            providerQuotaTextField,
		"percent_remaining": providerQuotaNumberField,
	})
	providerQuotaClineWindow = providerQuotaObjectField(map[string]providerQuotaFieldSpec{
		"items_count":          providerQuotaNumberField,
		"used_cost_units":      providerQuotaNumberField,
		"limit_cost_units":     providerQuotaNumberField,
		"remaining_cost_units": providerQuotaNumberField,
		"credits_used":         providerQuotaNumberField,
		"usage_ratio":          providerQuotaNumberField,
		"usage_percent":        providerQuotaNumberField,
		"next_reset_at":        providerQuotaTextField,
	})

	providerQuotaFieldAllowlists = map[string]map[string]providerQuotaFieldSpec{
		"claudecode": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"windows": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"5h":      providerQuotaClaudeWindow,
				"7d":      providerQuotaClaudeWindow,
				"overage": providerQuotaClaudeWindow,
			}),
			"representative_claim": providerQuotaTextField,
		}),
		"codex": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"rate_limit": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"primary_window":   providerQuotaCodexWindow,
				"secondary_window": providerQuotaCodexWindow,
			}),
		}),
		"github_copilot": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"limited_user_quotas": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"chat":        providerQuotaNumberField,
				"completions": providerQuotaNumberField,
			}),
			"total_quotas": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"chat":        providerQuotaNumberField,
				"completions": providerQuotaNumberField,
			}),
			"quota_snapshots": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"chat":                 providerQuotaCopilotSnapshot,
				"completions":          providerQuotaCopilotSnapshot,
				"premium_interactions": providerQuotaCopilotSnapshot,
				"premium_models":       providerQuotaCopilotSnapshot,
			}),
		}),
		"nanogpt": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"state":        providerQuotaTextField,
			"active":       providerQuotaBoolField,
			"allowOverage": providerQuotaBoolField,
			"limits": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"weeklyInputTokens": providerQuotaNumberField,
				"dailyImages":       providerQuotaNumberField,
				"dailyInputTokens":  providerQuotaNumberField,
			}),
			"windows": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"weeklyInputTokens": providerQuotaNanoGPTWindow,
				"dailyImages":       providerQuotaNanoGPTWindow,
				"dailyInputTokens":  providerQuotaNanoGPTWindow,
			}),
			"period": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"currentPeriodEnd": providerQuotaTextField,
			}),
		}),
		"wafer": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"current_period_used_percent": providerQuotaNumberField,
			"remaining_included_requests": providerQuotaNumberField,
			"included_request_limit":      providerQuotaNumberField,
			"overage_request_count":       providerQuotaNumberField,
			"window_start":                providerQuotaTextField,
			"window_end":                  providerQuotaTextField,
			"plan_tier":                   providerQuotaTextField,
		}),
		"synthetic": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"weeklyTokenLimit": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"percentRemaining": providerQuotaNumberField,
				"remainingCredits": providerQuotaNumericTextField,
				"maxCredits":       providerQuotaNumericTextField,
				"nextRegenAt":      providerQuotaTextField,
			}),
			"rollingFiveHourLimit": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"limited":     providerQuotaBoolField,
				"remaining":   providerQuotaNumberField,
				"max":         providerQuotaNumberField,
				"nextTickAt":  providerQuotaTextField,
				"tickPercent": providerQuotaNumberField,
			}),
		}),
		"neuralwatt": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"balance": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"credits_remaining_usd": providerQuotaNumberField,
				"total_credits_usd":     providerQuotaNumberField,
			}),
			"subscription": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"kwh_included":   providerQuotaNumberField,
				"kwh_used":       providerQuotaNumberField,
				"kwh_remaining":  providerQuotaNumberField,
				"in_overage":     providerQuotaBoolField,
				"status":         providerQuotaTextField,
				"plan":           providerQuotaTextField,
				"kwh_reset_date": providerQuotaTextField,
			}),
		}),
		"apertis": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"is_subscriber": providerQuotaBoolField,
			"payg": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"account_credits":         providerQuotaNumberField,
				"token_used":              providerQuotaNumberField,
				"token_total":             providerQuotaNumericTextField,
				"token_remaining":         providerQuotaNumericTextField,
				"token_is_unlimited":      providerQuotaBoolField,
				"token_monthly_limit_usd": providerQuotaNumberField,
				"token_monthly_used_usd":  providerQuotaNumberField,
				"monthly_reset_day":       providerQuotaNumberField,
			}),
			"subscription": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"plan_type":             providerQuotaTextField,
				"status":                providerQuotaTextField,
				"cycle_quota_limit":     providerQuotaNumberField,
				"cycle_quota_used":      providerQuotaNumberField,
				"cycle_quota_remaining": providerQuotaNumberField,
				"cycle_start":           providerQuotaTextField,
				"cycle_end":             providerQuotaTextField,
				"payg_fallback_enabled": providerQuotaBoolField,
				"payg_spent_usd":        providerQuotaNumberField,
				"payg_limit_usd":        providerQuotaNumberField,
			}),
		}),
		"opencode_go": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"windows": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"rolling": providerQuotaOpenCodeWindow,
				"weekly":  providerQuotaOpenCodeWindow,
				"monthly": providerQuotaOpenCodeWindow,
			}),
		}),
		"cline": providerQuotaFieldsWithPlan(map[string]providerQuotaFieldSpec{
			"model_scope":  providerQuotaTextField,
			"status_basis": providerQuotaTextField,
			"pool":         providerQuotaTextField,
			"pool_note":    providerQuotaTextField,
			"cost_scale":   providerQuotaNumberField,
			"balance": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"raw_balance": providerQuotaNumberField,
				"unit_note":   providerQuotaTextField,
			}),
			"windows": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"last5h":  providerQuotaClineWindow,
				"last7d":  providerQuotaClineWindow,
				"last30d": providerQuotaClineWindow,
			}),
			"usage_fetch": providerQuotaObjectField(map[string]providerQuotaFieldSpec{
				"pages":      providerQuotaNumberField,
				"items_seen": providerQuotaNumberField,
				"truncated":  providerQuotaBoolField,
			}),
		}),
	}
)

func providerQuotaObjectField(fields map[string]providerQuotaFieldSpec) providerQuotaFieldSpec {
	return providerQuotaFieldSpec{kind: providerQuotaObject, fields: fields}
}

func providerQuotaFieldsWithPlan(fields map[string]providerQuotaFieldSpec) map[string]providerQuotaFieldSpec {
	fields["plan_type"] = providerQuotaTextField
	return fields
}

// sanitizeProviderQuotaData is fail-closed: only fields consumed by the
// provider quota UI survive, and each nested object has its own allowlist.
func sanitizeProviderQuotaData(providerType string, data map[string]any) map[string]any {
	fields, ok := providerQuotaFieldAllowlists[providerType]
	if !ok {
		return map[string]any{}
	}

	return sanitizeProviderQuotaObject(data, fields)
}

func sanitizeProviderQuotaObject(data map[string]any, fields map[string]providerQuotaFieldSpec) map[string]any {
	sanitized := make(map[string]any, len(fields))
	for key, value := range data {
		spec, allowed := fields[key]
		if !allowed {
			continue
		}
		if clean, ok := sanitizeProviderQuotaValue(value, spec); ok {
			sanitized[key] = clean
		}
	}
	return sanitized
}

func sanitizeProviderQuotaValue(value any, spec providerQuotaFieldSpec) (any, bool) {
	if value == nil {
		return nil, true
	}

	switch spec.kind {
	case providerQuotaObject:
		typed, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		return sanitizeProviderQuotaObject(typed, spec.fields), true
	case providerQuotaText:
		typed, ok := value.(string)
		if !ok || !isSafeProviderQuotaText(typed) {
			return nil, false
		}
		return typed, true
	case providerQuotaNumber:
		return value, isProviderQuotaNumber(value)
	case providerQuotaBool:
		_, ok := value.(bool)
		return value, ok
	case providerQuotaNumericText:
		if isProviderQuotaNumber(value) {
			return value, true
		}
		typed, ok := value.(string)
		if !ok {
			return nil, false
		}
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return typed, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
	}

	return nil, false
}

func isProviderQuotaNumber(value any) bool {
	switch typed := value.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return !float32IsNonFinite(typed)
	case float64:
		return !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case json.Number:
		number, err := typed.Float64()
		return err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
	default:
		return false
	}
}

func float32IsNonFinite(value float32) bool {
	number := float64(value)
	return math.IsNaN(number) || math.IsInf(number, 0)
}

func isSafeProviderQuotaText(value string) bool {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) > 512 {
		return false
	}

	lower := strings.ToLower(trimmed)
	for _, marker := range []string{
		"http://", "https://",
		"authorization", "bearer ", "basic ",
		"x-api-key", "x_api_key", "apikey", "api_key",
		"client-secret", "client_secret", "password",
		"access-token", "access_token", "refresh-token", "refresh_token",
		"id-token", "id_token", "secret=", "token=", "token",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}

	if strings.Contains(trimmed, "@") ||
		strings.HasPrefix(lower, "sk-") ||
		strings.HasPrefix(lower, "ghp_") ||
		strings.HasPrefix(lower, "gho_") ||
		strings.HasPrefix(lower, "github_pat_") ||
		(strings.HasPrefix(lower, "eyj") && strings.Count(trimmed, ".") == 2) {
		return false
	}

	return true
}
