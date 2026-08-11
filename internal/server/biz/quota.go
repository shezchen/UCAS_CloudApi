package biz

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/shopspring/decimal"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xtime"
)

type QuotaWindow struct {
	Start        *time.Time
	End          *time.Time
	EndInclusive bool
}

type QuotaUsage struct {
	RequestCount int64
	TotalTokens  int64
	TotalCost    decimal.Decimal
}

type QuotaCheckResult struct {
	Allowed bool
	Message string
	Scope   string
	Window  QuotaWindow
}

type QuotaResult struct {
	Window QuotaWindow
	Usage  QuotaUsage
}

// AccountTokenQuotaPeriodUsage is one fixed Beijing-calendar allowance. Used
// tokens are settled base tokens: cache-read input and wallet-paid tokens have
// already been excluded.
type AccountTokenQuotaPeriodUsage struct {
	LimitTokens     int64
	UsedTokens      int64
	RemainingTokens int64
	Window          QuotaWindow
	ResetAt         time.Time
}

// AccountTokenQuotaOverview contains the user-facing quota and wallet state.
// Daily and weekly usage reset independently; wallet balances are permanent.
type AccountTokenQuotaOverview struct {
	Daily  AccountTokenQuotaPeriodUsage
	Weekly AccountTokenQuotaPeriodUsage
	Wallet TokenWalletSummary
}

type QuotaService struct {
	ent    *ent.Client
	system *SystemService
}

func NewQuotaService(entClient *ent.Client, systemService *SystemService) *QuotaService {
	return &QuotaService{ent: entClient, system: systemService}
}

func (s *QuotaService) CheckAPIKeyQuota(ctx context.Context, apiKeyID int, quota *objects.APIKeyQuota) (QuotaCheckResult, error) {
	if quota == nil {
		return QuotaCheckResult{Allowed: true}, nil
	}

	loc := s.system.TimeLocation(ctx)

	window, err := quotaWindow(xtime.UTCNow(), quota.Period, loc)
	if err != nil {
		return QuotaCheckResult{}, err
	}

	if quota.Requests != nil {
		reqCount, err := authz.RunWithSystemBypass(ctx, "quota-request-count", func(bypassCtx context.Context) (int64, error) {
			return s.requestCount(bypassCtx, apiKeyID, window)
		})
		if err != nil {
			return QuotaCheckResult{}, err
		}

		if reqCount >= *quota.Requests {
			return QuotaCheckResult{
				Allowed: false,
				Message: fmt.Sprintf("requests quota exceeded: %d/%d", reqCount, *quota.Requests),
				Window:  window,
			}, nil
		}
	}

	if quota.TotalTokens == nil && quota.Cost == nil {
		return QuotaCheckResult{
			Allowed: true,
			Window:  window,
		}, nil
	}

	usageAgg, err := authz.RunWithSystemBypass(ctx, "quota-usage-agg", func(bypassCtx context.Context) (usageAggResult, error) {
		return s.usageAgg(bypassCtx, apiKeyID, window, quota.TotalTokens != nil, quota.Cost != nil)
	})
	if err != nil {
		return QuotaCheckResult{}, err
	}

	if quota.TotalTokens != nil && usageAgg.TotalTokens >= *quota.TotalTokens {
		return QuotaCheckResult{
			Allowed: false,
			Message: fmt.Sprintf("total_tokens quota exceeded: %d/%d", usageAgg.TotalTokens, *quota.TotalTokens),
			Window:  window,
		}, nil
	}

	if quota.Cost != nil && usageAgg.TotalCost.GreaterThanOrEqual(*quota.Cost) {
		return QuotaCheckResult{
			Allowed: false,
			Message: fmt.Sprintf("cost quota exceeded: %s/%s", usageAgg.TotalCost.String(), quota.Cost.String()),
			Window:  window,
		}, nil
	}

	return QuotaCheckResult{
		Allowed: true,
		Window:  window,
	}, nil
}

// AccountDailyTokenLimit returns the globally configured daily total-token cap
// for every account. It deliberately does not read users.daily_token_limit:
// that legacy field remains for historical compatibility only and must not
// make the live global setting behave differently across accounts.
func (s *QuotaService) AccountDailyTokenLimit(ctx context.Context) (int64, error) {
	limit, err := s.system.UserDailyTokenLimit(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get account daily token limit: %w", err)
	}

	return limit, nil
}

// AccountWeeklyTokenLimit returns the globally configured weekly
// effective-token cap for every account.
func (s *QuotaService) AccountWeeklyTokenLimit(ctx context.Context) (int64, error) {
	limit, err := s.system.UserWeeklyTokenLimit(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get account weekly token limit: %w", err)
	}

	return limit, nil
}

var beijingQuotaLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err == nil {
		return loc
	}

	return time.FixedZone("Asia/Shanghai", 8*60*60)
}()

func accountQuotaWindows(now time.Time) (daily QuotaWindow, weekly QuotaWindow) {
	nowLocal := now.In(beijingQuotaLocation)
	todayStartLocal := time.Date(
		nowLocal.Year(),
		nowLocal.Month(),
		nowLocal.Day(),
		0, 0, 0, 0,
		beijingQuotaLocation,
	)
	weekday := int(nowLocal.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	weekStartLocal := todayStartLocal.AddDate(0, 0, -(weekday - 1))

	dailyStart := todayStartLocal.UTC()
	dailyEnd := todayStartLocal.AddDate(0, 0, 1).UTC()
	weeklyStart := weekStartLocal.UTC()
	weeklyEnd := weekStartLocal.AddDate(0, 0, 7).UTC()

	return QuotaWindow{Start: &dailyStart, End: &dailyEnd},
		QuotaWindow{Start: &weeklyStart, End: &weeklyEnd}
}

// CurrentWeeklyQuotaWindowStart returns the UTC start of the weekly account
// quota window that contains now. GC uses it as a floor for usage-log
// deletion: usage logs inside the active window are still the source of truth
// for account quota accounting, so deleting them would silently reset
// consumed quota.
func CurrentWeeklyQuotaWindowStart(now time.Time) time.Time {
	_, weekly := accountQuotaWindows(now)
	return *weekly.Start
}

func authorizeAccountQuotaOverview(ctx context.Context, userID int) error {
	principal, ok := authz.GetPrincipal(ctx)
	if !ok {
		return fmt.Errorf("account quota overview requires an authenticated principal")
	}

	switch principal.Type {
	case authz.PrincipalTypeSystem, authz.PrincipalTypeTest:
		return nil
	case authz.PrincipalTypeUser:
		currentUser, ok := contexts.GetUser(ctx)
		if !ok || currentUser == nil {
			return fmt.Errorf("account quota overview user principal is missing")
		}
		if currentUser.IsOwner || currentUser.ID == userID {
			return nil
		}
	case authz.PrincipalTypeAPIKey:
		currentKey, ok := contexts.GetAPIKey(ctx)
		if ok && currentKey != nil && currentKey.UserID == userID {
			return nil
		}
	}

	return fmt.Errorf("account quota overview access denied for user %d", userID)
}

func (s *QuotaService) accountAPIKeyIDs(ctx context.Context, userID int) ([]int, error) {
	if userID <= 0 {
		return nil, nil
	}

	apiKeyIDs, err := authz.RunWithSystemBypass(ctx, "account-quota-api-keys", func(bypassCtx context.Context) ([]int, error) {
		return s.ent.APIKey.Query().
			Where(apikey.UserIDEQ(userID)).
			IDs(schematype.SkipSoftDelete(bypassCtx))
	})
	if err != nil {
		return nil, fmt.Errorf("list account API keys for quota: %w", err)
	}

	return apiKeyIDs, nil
}

func (s *QuotaService) accountTokenQuotaUsage(
	ctx context.Context,
	userID int,
	now time.Time,
) (daily AccountTokenQuotaPeriodUsage, weekly AccountTokenQuotaPeriodUsage, err error) {
	dailyLimit, err := s.AccountDailyTokenLimit(ctx)
	if err != nil {
		return daily, weekly, err
	}
	weeklyLimit, err := s.AccountWeeklyTokenLimit(ctx)
	if err != nil {
		return daily, weekly, err
	}

	dailyWindow, weeklyWindow := accountQuotaWindows(now)
	apiKeyIDs, err := s.accountAPIKeyIDs(ctx, userID)
	if err != nil {
		return daily, weekly, err
	}

	dailyUsed, err := authz.RunWithSystemBypass(ctx, "account-daily-quota-usage", func(bypassCtx context.Context) (int64, error) {
		return s.settledBaseTokenUsageForAPIKeys(bypassCtx, apiKeyIDs, dailyWindow)
	})
	if err != nil {
		return daily, weekly, err
	}
	weeklyUsed, err := authz.RunWithSystemBypass(ctx, "account-weekly-quota-usage", func(bypassCtx context.Context) (int64, error) {
		return s.settledBaseTokenUsageForAPIKeys(bypassCtx, apiKeyIDs, weeklyWindow)
	})
	if err != nil {
		return daily, weekly, err
	}

	daily = AccountTokenQuotaPeriodUsage{
		LimitTokens:     dailyLimit,
		UsedTokens:      dailyUsed,
		RemainingTokens: max(dailyLimit-dailyUsed, int64(0)),
		Window:          dailyWindow,
		ResetAt:         *dailyWindow.End,
	}
	weekly = AccountTokenQuotaPeriodUsage{
		LimitTokens:     weeklyLimit,
		UsedTokens:      weeklyUsed,
		RemainingTokens: max(weeklyLimit-weeklyUsed, int64(0)),
		Window:          weeklyWindow,
		ResetAt:         *weeklyWindow.End,
	}

	return daily, weekly, nil
}

// AccountQuotaOverview returns the authenticated user's daily/weekly settled
// usage and permanent wallet counters for the UI. Owner may inspect any user.
func (s *QuotaService) AccountQuotaOverview(ctx context.Context, userID int) (AccountTokenQuotaOverview, error) {
	if userID <= 0 {
		return AccountTokenQuotaOverview{}, fmt.Errorf("account quota overview requires a positive user ID")
	}
	if err := authorizeAccountQuotaOverview(ctx, userID); err != nil {
		return AccountTokenQuotaOverview{}, err
	}

	daily, weekly, err := s.accountTokenQuotaUsage(ctx, userID, xtime.UTCNow())
	if err != nil {
		return AccountTokenQuotaOverview{}, err
	}
	wallet, err := NewTokenWalletService(s.ent).Summary(ctx, userID)
	if err != nil {
		return AccountTokenQuotaOverview{}, err
	}

	return AccountTokenQuotaOverview{
		Daily:  daily,
		Weekly: weekly,
		Wallet: wallet,
	}, nil
}

// CheckUserDailyTokenQuota retains its historical name while enforcing both
// account-wide daily and weekly effective-token limits. Soft-deleted keys remain
// part of the aggregate so rotating or deleting a key cannot reset usage.
// Permanent donation wallet credit is consumed before either calendar limit.
func (s *QuotaService) CheckUserDailyTokenQuota(ctx context.Context, userID int) (QuotaCheckResult, error) {
	if userID <= 0 {
		return QuotaCheckResult{Allowed: true}, nil
	}
	walletBalance, err := NewTokenWalletService(s.ent).Balance(ctx, userID)
	if err != nil {
		return QuotaCheckResult{}, err
	}
	if walletBalance > 0 {
		return QuotaCheckResult{Allowed: true}, nil
	}

	daily, weekly, err := s.accountTokenQuotaUsage(ctx, userID, xtime.UTCNow())
	if err != nil {
		return QuotaCheckResult{}, fmt.Errorf("read account quota usage: %w", err)
	}

	if daily.UsedTokens >= daily.LimitTokens {
		return QuotaCheckResult{
			Allowed: false,
			Message: fmt.Sprintf(
				"user daily effective_tokens quota exceeded: %d/%d",
				daily.UsedTokens,
				daily.LimitTokens,
			),
			Scope:  "user_daily",
			Window: daily.Window,
		}, nil
	}

	if weekly.UsedTokens >= weekly.LimitTokens {
		return QuotaCheckResult{
			Allowed: false,
			Message: fmt.Sprintf(
				"user weekly effective_tokens quota exceeded: %d/%d",
				weekly.UsedTokens,
				weekly.LimitTokens,
			),
			Scope:  "user_weekly",
			Window: weekly.Window,
		}, nil
	}

	return QuotaCheckResult{Allowed: true, Window: daily.Window}, nil
}

// ProfileQuotaUsage is the per-profile quota usage of an API key, shared by the
// admin and OpenAPI GraphQL resolvers so the "iterate profiles → GetQuota" logic
// lives in one place.
type ProfileQuotaUsage struct {
	ProfileName string
	Quota       *objects.APIKeyQuota
	Window      QuotaWindow
	Usage       QuotaUsage
}

// ProfileQuotaUsages returns the realtime quota usage for every profile on the
// given API key that has a quota configured. The caller is responsible for
// loading the key (and thereby applying authorization); this method only reads
// usage aggregates for the key's id.
func (s *QuotaService) ProfileQuotaUsages(ctx context.Context, apiKey *ent.APIKey) ([]ProfileQuotaUsage, error) {
	if apiKey.Profiles == nil || len(apiKey.Profiles.Profiles) == 0 {
		return nil, nil
	}

	out := make([]ProfileQuotaUsage, 0, len(apiKey.Profiles.Profiles))

	for _, p := range apiKey.Profiles.Profiles {
		if p.Quota == nil {
			continue
		}

		res, err := s.GetQuota(ctx, apiKey.ID, p.Quota)
		if err != nil {
			return nil, err
		}

		out = append(out, ProfileQuotaUsage{
			ProfileName: p.Name,
			Quota:       p.Quota,
			Window:      res.Window,
			Usage:       res.Usage,
		})
	}

	return out, nil
}

func (s *QuotaService) GetQuota(ctx context.Context, apiKeyID int, quota *objects.APIKeyQuota) (QuotaResult, error) {
	if quota == nil {
		return QuotaResult{}, nil
	}

	loc := s.system.TimeLocation(ctx)

	window, err := quotaWindow(xtime.UTCNow(), quota.Period, loc)
	if err != nil {
		return QuotaResult{}, err
	}

	reqCount, err := authz.RunWithSystemBypass(ctx, "quota-request-count", func(bypassCtx context.Context) (int64, error) {
		return s.requestCount(bypassCtx, apiKeyID, window)
	})
	if err != nil {
		return QuotaResult{}, err
	}

	usageAgg, err := authz.RunWithSystemBypass(ctx, "quota-usage-agg", func(bypassCtx context.Context) (usageAggResult, error) {
		return s.usageAgg(bypassCtx, apiKeyID, window, true, true)
	})
	if err != nil {
		return QuotaResult{}, err
	}

	return QuotaResult{
		Window: window,
		Usage: QuotaUsage{
			RequestCount: reqCount,
			TotalTokens:  usageAgg.TotalTokens,
			TotalCost:    usageAgg.TotalCost,
		},
	}, nil
}

func quotaWindow(now time.Time, period objects.APIKeyQuotaPeriod, loc *time.Location) (QuotaWindow, error) {
	if loc == nil {
		loc = time.UTC
	}

	switch period.Type {
	case objects.APIKeyQuotaPeriodTypeAllTime:
		end := now
		return QuotaWindow{End: &end, EndInclusive: true}, nil
	case objects.APIKeyQuotaPeriodTypePastDuration:
		if period.PastDuration == nil {
			return QuotaWindow{}, fmt.Errorf("pastDuration is required")
		}

		if period.PastDuration.Value <= 0 {
			return QuotaWindow{}, fmt.Errorf("pastDuration.value must be positive")
		}

		var d time.Duration

		switch period.PastDuration.Unit {
		case objects.APIKeyQuotaPastDurationUnitMinute:
			d = time.Duration(period.PastDuration.Value) * time.Minute
		case objects.APIKeyQuotaPastDurationUnitHour:
			d = time.Duration(period.PastDuration.Value) * time.Hour
		case objects.APIKeyQuotaPastDurationUnitDay:
			d = time.Duration(period.PastDuration.Value) * 24 * time.Hour
		default:
			return QuotaWindow{}, fmt.Errorf("unknown pastDuration.unit: %s", period.PastDuration.Unit)
		}

		start := now.Add(-d)
		end := now

		return QuotaWindow{Start: &start, End: &end, EndInclusive: true}, nil
	case objects.APIKeyQuotaPeriodTypeCalendarDuration:
		if period.CalendarDuration == nil {
			return QuotaWindow{}, fmt.Errorf("calendarDuration is required")
		}

		switch period.CalendarDuration.Unit {
		case objects.APIKeyQuotaCalendarDurationUnitDay:
			nowLocal := now.In(loc)
			startLocal := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, loc)
			endLocal := startLocal.AddDate(0, 0, 1)
			start := startLocal.UTC()
			end := endLocal.UTC()

			return QuotaWindow{Start: &start, End: &end}, nil
		case objects.APIKeyQuotaCalendarDurationUnitMonth:
			nowLocal := now.In(loc)
			startLocal := time.Date(nowLocal.Year(), nowLocal.Month(), 1, 0, 0, 0, 0, loc)
			endLocal := startLocal.AddDate(0, 1, 0)
			start := startLocal.UTC()
			end := endLocal.UTC()

			return QuotaWindow{Start: &start, End: &end}, nil
		default:
			return QuotaWindow{}, fmt.Errorf("unknown calendarDuration.unit: %s", period.CalendarDuration.Unit)
		}
	default:
		return QuotaWindow{}, fmt.Errorf("unknown period.type: %s", period.Type)
	}
}

func (s *QuotaService) requestCount(ctx context.Context, apiKeyID int, window QuotaWindow) (int64, error) {
	q := s.ent.UsageLog.Query().Where(usagelog.APIKeyIDEQ(apiKeyID))

	if window.Start != nil {
		q = q.Where(usagelog.CreatedAtGTE(*window.Start))
	}

	if window.End != nil {
		if window.EndInclusive {
			q = q.Where(usagelog.CreatedAtLTE(*window.End))
		} else {
			q = q.Where(usagelog.CreatedAtLT(*window.End))
		}
	}

	n, err := q.Count(ctx)
	if err != nil {
		return 0, err
	}

	return int64(n), nil
}

type usageAggResult struct {
	TotalTokens int64
	TotalCost   decimal.Decimal
}

func effectiveTokensSQL(s *sql.Selector) string {
	prompt := s.C(usagelog.FieldPromptTokens)
	completion := s.C(usagelog.FieldCompletionTokens)
	cached := s.C(usagelog.FieldPromptCachedTokens)
	total := s.C(usagelog.FieldTotalTokens)
	storedEffective := s.C(usagelog.FieldEffectiveTokens)
	cacheReadKnown := s.C(usagelog.FieldCacheReadTokensKnown)

	nonCachedPrompt := fmt.Sprintf(
		"CASE WHEN COALESCE(%[1]s, 0) <= 0 OR COALESCE(%[2]s, 0) >= COALESCE(%[1]s, 0) THEN 0 WHEN COALESCE(%[1]s, 0) - CASE WHEN COALESCE(%[2]s, 0) > 0 THEN COALESCE(%[2]s, 0) ELSE 0 END > %[3]d THEN %[3]d ELSE COALESCE(%[1]s, 0) - CASE WHEN COALESCE(%[2]s, 0) > 0 THEN COALESCE(%[2]s, 0) ELSE 0 END END",
		prompt,
		cached,
		MaxEffectiveTokensPerRequest,
	)
	completionBounded := fmt.Sprintf(
		"CASE WHEN COALESCE(%[1]s, 0) <= 0 THEN 0 WHEN COALESCE(%[1]s, 0) > %[2]d THEN %[2]d ELSE COALESCE(%[1]s, 0) END",
		completion,
		MaxEffectiveTokensPerRequest,
	)
	derived := fmt.Sprintf(
		"CASE WHEN (%[1]s) + (%[2]s) > %[3]d THEN %[3]d ELSE (%[1]s) + (%[2]s) END",
		nonCachedPrompt,
		completionBounded,
		MaxEffectiveTokensPerRequest,
	)
	totalOnly := fmt.Sprintf(
		"CASE WHEN COALESCE(%[1]s, 0) <= 0 OR COALESCE(%[2]s, 0) >= COALESCE(%[1]s, 0) THEN 0 WHEN COALESCE(%[1]s, 0) - CASE WHEN COALESCE(%[2]s, 0) > 0 THEN COALESCE(%[2]s, 0) ELSE 0 END > %[3]d THEN %[3]d ELSE COALESCE(%[1]s, 0) - CASE WHEN COALESCE(%[2]s, 0) > 0 THEN COALESCE(%[2]s, 0) ELSE 0 END END",
		total,
		cached,
		MaxEffectiveTokensPerRequest,
	)
	legacy := fmt.Sprintf(
		"CASE WHEN (%[1]s) >= (%[2]s) THEN (%[1]s) ELSE (%[2]s) END",
		derived,
		totalOnly,
	)
	storedBounded := fmt.Sprintf(
		"CASE WHEN COALESCE(%[1]s, 0) <= 0 THEN 0 WHEN COALESCE(%[1]s, 0) > %[2]d THEN %[2]d ELSE COALESCE(%[1]s, 0) END",
		storedEffective,
		MaxEffectiveTokensPerRequest,
	)

	// effective_tokens is populated for every new row. The fallback keeps
	// already-recorded rows quota-bearing immediately after the additive schema
	// migration, without retroactively creating wallet credit.
	return fmt.Sprintf(
		"CASE WHEN COALESCE(%[1]s, 0) <> 0 OR COALESCE(%[2]s, FALSE) THEN (%[3]s) ELSE (%[4]s) END",
		storedEffective,
		cacheReadKnown,
		storedBounded,
		legacy,
	)
}

func settledBaseTokensSQL(s *sql.Selector) string {
	effective := effectiveTokensSQL(s)
	walletConsumed := s.C(usagelog.FieldWalletConsumedTokens)

	return fmt.Sprintf(
		"CASE WHEN COALESCE(%[1]s, 0) >= (%[2]s) THEN 0 ELSE (%[2]s) - CASE WHEN COALESCE(%[1]s, 0) > 0 THEN COALESCE(%[1]s, 0) ELSE 0 END END",
		walletConsumed,
		effective,
	)
}

func (s *QuotaService) settledBaseTokenUsageForAPIKeys(
	ctx context.Context,
	apiKeyIDs []int,
	window QuotaWindow,
) (int64, error) {
	if len(apiKeyIDs) == 0 {
		return 0, nil
	}

	type row struct {
		TotalTokens int64 `json:"total_tokens"`
	}
	var rows []row

	q := s.ent.UsageLog.Query().Where(
		usagelog.APIKeyIDIn(apiKeyIDs...),
		usagelog.SourceNEQ(usagelog.SourceTest),
	)
	if window.Start != nil {
		q = q.Where(usagelog.CreatedAtGTE(*window.Start))
	}
	if window.End != nil {
		q = q.Where(usagelog.CreatedAtLT(*window.End))
	}

	err := q.Modify(func(selector *sql.Selector) {
		selector.Select(
			sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", settledBaseTokensSQL(selector)), "total_tokens"),
		)
	}).Scan(ctx, &rows)
	if err != nil {
		return 0, fmt.Errorf("aggregate settled base token usage: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	return rows[0].TotalTokens, nil
}

func (s *QuotaService) usageAgg(ctx context.Context, apiKeyID int, window QuotaWindow, needTokens bool, needCost bool) (usageAggResult, error) {
	return s.usageAggForAPIKeys(ctx, []int{apiKeyID}, window, needTokens, needCost)
}

func (s *QuotaService) usageAggForAPIKeys(
	ctx context.Context,
	apiKeyIDs []int,
	window QuotaWindow,
	needTokens bool,
	needCost bool,
) (usageAggResult, error) {
	if !needTokens && !needCost {
		return usageAggResult{}, nil
	}
	if len(apiKeyIDs) == 0 {
		return usageAggResult{TotalCost: decimal.Zero}, nil
	}

	queryAgg := func(q *ent.UsageLogQuery) (usageAggResult, error) {
		if needTokens {
			q = q.Where(usagelog.SourceNEQ(usagelog.SourceTest))
		}

		if window.Start != nil {
			q = q.Where(usagelog.CreatedAtGTE(*window.Start))
		}

		if window.End != nil {
			if window.EndInclusive {
				q = q.Where(usagelog.CreatedAtLTE(*window.End))
			} else {
				q = q.Where(usagelog.CreatedAtLT(*window.End))
			}
		}

		switch {
		case needTokens && needCost:
			type row struct {
				TotalTokens int64   `json:"total_tokens"`
				TotalCost   float64 `json:"total_cost"`
			}

			var rows []row

			err := q.Modify(func(s *sql.Selector) {
				s.Select(
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", effectiveTokensSQL(s)), "total_tokens"),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", s.C(usagelog.FieldTotalCost)), "total_cost"),
				)
			}).Scan(ctx, &rows)
			if err != nil {
				return usageAggResult{}, err
			}

			if len(rows) == 0 {
				return usageAggResult{TotalCost: decimal.Zero}, nil
			}

			return usageAggResult{
				TotalTokens: rows[0].TotalTokens,
				TotalCost:   decimal.NewFromFloat(rows[0].TotalCost),
			}, nil
		case needTokens:
			type row struct {
				TotalTokens int64 `json:"total_tokens"`
			}

			var rows []row

			err := q.Modify(func(s *sql.Selector) {
				s.Select(
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", effectiveTokensSQL(s)), "total_tokens"),
				)
			}).Scan(ctx, &rows)
			if err != nil {
				return usageAggResult{}, err
			}

			if len(rows) == 0 {
				return usageAggResult{TotalCost: decimal.Zero}, nil
			}

			return usageAggResult{TotalTokens: rows[0].TotalTokens, TotalCost: decimal.Zero}, nil
		default:
			type row struct {
				TotalCost float64 `json:"total_cost"`
			}

			var rows []row

			err := q.Modify(func(s *sql.Selector) {
				s.Select(
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", s.C(usagelog.FieldTotalCost)), "total_cost"),
				)
			}).Scan(ctx, &rows)
			if err != nil {
				return usageAggResult{}, err
			}

			if len(rows) == 0 {
				return usageAggResult{TotalCost: decimal.Zero}, nil
			}

			return usageAggResult{TotalCost: decimal.NewFromFloat(rows[0].TotalCost)}, nil
		}
	}

	agg1, err := queryAgg(s.ent.UsageLog.Query().Where(usagelog.APIKeyIDIn(apiKeyIDs...)))
	if err != nil {
		return usageAggResult{}, err
	}

	//  Compatible with old usage log without api_key_id.
	//  DO NOT NEED FOR NOW.
	// agg2, err := queryAgg(s.ent.UsageLog.Query().Where(
	// 	usagelog.APIKeyIDIsNil(),
	// 	usagelog.HasRequestWith(request.APIKeyIDEQ(apiKeyID)),
	// ))
	// if err != nil {
	// 	return usageAggResult{}, err
	// }

	return usageAggResult{
		TotalTokens: agg1.TotalTokens, // + agg2.TotalTokens,
		TotalCost:   agg1.TotalCost,   // .Add(agg2.TotalCost),
	}, nil
}
