package biz

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
)

const (
	// DefaultUserDailyTokenLimit is the global daily effective-token allowance
	// used until an Owner configures another value.
	DefaultUserDailyTokenLimit int64 = 16_000_000

	// DefaultUserWeeklyTokenLimit is the global weekly effective-token
	// allowance used until an Owner configures another value.
	DefaultUserWeeklyTokenLimit int64 = 64_000_000

	// SystemKeyUserDailyQuotaSettings stores the Owner-configured global user
	// daily quota. It is read for every quota check.
	SystemKeyUserDailyQuotaSettings = "user_daily_quota_settings"
)

// UserDailyQuotaSettings contains the global account-wide daily and weekly
// effective-token allowances. It applies to every existing and future user
// account. The historical type and system-key names are retained for stored
// configuration and GraphQL compatibility.
type UserDailyQuotaSettings struct {
	DailyTokenLimit  int64 `json:"daily_token_limit"`
	WeeklyTokenLimit int64 `json:"weekly_token_limit"`
}

func defaultUserDailyQuotaSettings() UserDailyQuotaSettings {
	return UserDailyQuotaSettings{
		DailyTokenLimit:  DefaultUserDailyTokenLimit,
		WeeklyTokenLimit: DefaultUserWeeklyTokenLimit,
	}
}

// UserDailyQuotaSettings returns the global account-wide daily quota. The
// narrowly scoped system bypass lets the request quota path read only this
// setting without exposing broader system settings.
func (s *SystemService) UserDailyQuotaSettings(ctx context.Context) (*UserDailyQuotaSettings, error) {
	value, err := authz.RunWithSystemBypass(ctx, "user-daily-quota-settings", func(bypassCtx context.Context) (string, error) {
		return s.getSystemValue(bypassCtx, SystemKeyUserDailyQuotaSettings)
	})
	if err != nil {
		if ent.IsNotFound(err) {
			settings := defaultUserDailyQuotaSettings()
			return &settings, nil
		}

		return nil, fmt.Errorf("failed to get user daily quota settings: %w", err)
	}

	var stored struct {
		DailyTokenLimit  *int64 `json:"daily_token_limit"`
		WeeklyTokenLimit *int64 `json:"weekly_token_limit"`
	}
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user daily quota settings: %w", err)
	}

	settings := defaultUserDailyQuotaSettings()
	if stored.DailyTokenLimit != nil {
		settings.DailyTokenLimit = *stored.DailyTokenLimit
	}
	if stored.WeeklyTokenLimit != nil {
		settings.WeeklyTokenLimit = *stored.WeeklyTokenLimit
	}

	if settings.DailyTokenLimit < 0 {
		return nil, fmt.Errorf("invalid user daily quota settings: daily token limit cannot be negative")
	}
	if settings.WeeklyTokenLimit < 0 {
		return nil, fmt.Errorf("invalid user daily quota settings: weekly token limit cannot be negative")
	}

	return &settings, nil
}

// UserDailyTokenLimit returns the global limit enforced for every account.
func (s *SystemService) UserDailyTokenLimit(ctx context.Context) (int64, error) {
	settings, err := s.UserDailyQuotaSettings(ctx)
	if err != nil {
		return 0, err
	}

	return settings.DailyTokenLimit, nil
}

// UserWeeklyTokenLimit returns the global weekly limit enforced for every
// account.
func (s *SystemService) UserWeeklyTokenLimit(ctx context.Context) (int64, error) {
	settings, err := s.UserDailyQuotaSettings(ctx)
	if err != nil {
		return 0, err
	}

	return settings.WeeklyTokenLimit, nil
}

// SetUserDailyQuotaSettings updates the global limits for all accounts. Cache
// invalidation in setSystemValue makes new values visible on the next check.
func (s *SystemService) SetUserDailyQuotaSettings(ctx context.Context, settings UserDailyQuotaSettings) error {
	if settings.DailyTokenLimit < 0 {
		return fmt.Errorf("daily token limit cannot be negative")
	}
	if settings.WeeklyTokenLimit < 0 {
		return fmt.Errorf("weekly token limit cannot be negative")
	}

	value, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("failed to marshal user daily quota settings: %w", err)
	}

	return s.setSystemValue(ctx, SystemKeyUserDailyQuotaSettings, string(value))
}
