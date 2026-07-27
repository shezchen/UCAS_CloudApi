package gql

import (
	"context"
	"fmt"
	"sort"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/pkg/xtime"
	"github.com/looplj/axonhub/internal/server/biz"
)

const campusLeaderboardLimit = 50

var campusLeaderboardLocation = time.FixedZone("Asia/Shanghai", 8*60*60)

type campusUsageAggregate struct {
	UserID            int    `json:"user_id"`
	Nickname          string `json:"nickname"`
	RecordedTokens    int64  `json:"recorded_tokens"`
	MeteredRequestCnt int    `json:"metered_request_count"`
}

type campusModelUsageAggregate struct {
	ModelID           string `json:"model_id"`
	EffectiveTokens   int64  `json:"effective_tokens"`
	InputTokens       int64  `json:"input_tokens"`
	CachedReadTokens  int64  `json:"cached_read_tokens"`
	OutputTokens      int64  `json:"output_tokens"`
	MeteredRequestCnt int    `json:"metered_request_count"`
}

type rankedCampusUsage struct {
	UserID            int
	DisplayName       string
	PublicAlias       string
	RecordedTokens    int64
	MeteredRequestCnt int
	LimitPercent      float64
}

func campusLimitPercent(recordedTokens, dailyTokenLimit int64) float64 {
	// A negative sentinel means that no quota period corresponds to the
	// selected leaderboard window (currently the monthly view).
	if dailyTokenLimit < 0 {
		return 0
	}
	if dailyTokenLimit == 0 {
		if recordedTokens > 0 {
			return 100
		}
		return 0
	}

	return float64(recordedTokens) / float64(dailyTokenLimit) * 100
}

func campusLeaderboardPeriod(periods xtime.CalendarPeriods, timeWindow *string) (xtime.Period, error) {
	window := "day"
	if timeWindow != nil && *timeWindow != "" {
		window = *timeWindow
	}

	switch window {
	case "day":
		return periods.Today, nil
	case "week":
		return periods.ThisWeek, nil
	case "month":
		return periods.ThisMonth, nil
	default:
		return xtime.Period{}, fmt.Errorf("invalid campus leaderboard time window %q; expected day, week, or month", window)
	}
}

func campusLeaderboardQuotaLimit(settings *biz.UserDailyQuotaSettings, timeWindow *string) int64 {
	window := "day"
	if timeWindow != nil && *timeWindow != "" {
		window = *timeWindow
	}

	switch window {
	case "day":
		return settings.DailyTokenLimit
	case "week":
		return settings.WeeklyTokenLimit
	default:
		// There is intentionally no monthly account quota.
		return -1
	}
}

func rankCampusUsage(
	projectID, currentUserID int,
	currentNickname string,
	dailyTokenLimit int64,
	aggregates []campusUsageAggregate,
) []*CampusUsageLeaderboardEntry {
	foundCurrentUser := false
	ranked := make([]rankedCampusUsage, 0, len(aggregates)+1)
	for _, aggregate := range aggregates {
		if aggregate.UserID == currentUserID {
			foundCurrentUser = true
		}
		publicAlias := biz.CampusPublicAlias(projectID, aggregate.UserID)
		ranked = append(ranked, rankedCampusUsage{
			UserID:            aggregate.UserID,
			DisplayName:       biz.CampusDisplayName(aggregate.Nickname, publicAlias),
			PublicAlias:       publicAlias,
			RecordedTokens:    aggregate.RecordedTokens,
			MeteredRequestCnt: aggregate.MeteredRequestCnt,
			LimitPercent:      campusLimitPercent(aggregate.RecordedTokens, dailyTokenLimit),
		})
	}

	if !foundCurrentUser {
		publicAlias := biz.CampusPublicAlias(projectID, currentUserID)
		ranked = append(ranked, rankedCampusUsage{
			UserID:      currentUserID,
			DisplayName: biz.CampusDisplayName(currentNickname, publicAlias),
			PublicAlias: publicAlias,
		})
	}

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].RecordedTokens != ranked[j].RecordedTokens {
			return ranked[i].RecordedTokens > ranked[j].RecordedTokens
		}
		if ranked[i].MeteredRequestCnt != ranked[j].MeteredRequestCnt {
			return ranked[i].MeteredRequestCnt > ranked[j].MeteredRequestCnt
		}
		return ranked[i].PublicAlias < ranked[j].PublicAlias
	})

	entries := make([]*CampusUsageLeaderboardEntry, 0, min(campusLeaderboardLimit+1, len(ranked)))
	for i, item := range ranked {
		isCurrentUser := item.UserID == currentUserID
		if i >= campusLeaderboardLimit && !isCurrentUser {
			continue
		}
		entries = append(entries, &CampusUsageLeaderboardEntry{
			Rank:                i + 1,
			DisplayName:         item.DisplayName,
			PublicAlias:         item.PublicAlias,
			IsMe:                isCurrentUser,
			RecordedTokens:      float64(item.RecordedTokens),
			MeteredRequestCount: item.MeteredRequestCnt,
			LimitPercent:        item.LimitPercent,
		})
	}

	return entries
}

func rankCampusModelUsage(aggregates []campusModelUsageAggregate) []*CampusModelUsageLeaderboardEntry {
	sort.Slice(aggregates, func(i, j int) bool {
		if aggregates[i].EffectiveTokens != aggregates[j].EffectiveTokens {
			return aggregates[i].EffectiveTokens > aggregates[j].EffectiveTokens
		}
		if aggregates[i].MeteredRequestCnt != aggregates[j].MeteredRequestCnt {
			return aggregates[i].MeteredRequestCnt > aggregates[j].MeteredRequestCnt
		}
		return aggregates[i].ModelID < aggregates[j].ModelID
	})

	entryCount := min(campusLeaderboardLimit, len(aggregates))
	entries := make([]*CampusModelUsageLeaderboardEntry, 0, entryCount)
	for i := 0; i < entryCount; i++ {
		item := aggregates[i]
		entries = append(entries, &CampusModelUsageLeaderboardEntry{
			Rank:                i + 1,
			ModelID:             item.ModelID,
			EffectiveTokens:     float64(item.EffectiveTokens),
			InputTokens:         float64(item.InputTokens),
			CachedReadTokens:    float64(item.CachedReadTokens),
			OutputTokens:        float64(item.OutputTokens),
			MeteredRequestCount: item.MeteredRequestCnt,
		})
	}

	return entries
}

func (r *queryResolver) campusLeaderboardIdentity(
	ctx context.Context,
) (currentUserID, projectID int, currentNickname string, err error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return 0, 0, "", fmt.Errorf("user not found in context")
	}

	projectID, ok = contexts.GetProjectID(ctx)
	if !ok {
		return 0, 0, "", fmt.Errorf("project ID not found in context")
	}

	if !currentUser.IsOwner {
		isMember, membershipErr := r.client.UserProject.Query().
			Where(
				userproject.UserIDEQ(currentUser.ID),
				userproject.ProjectIDEQ(projectID),
			).
			Exist(ctx)
		if membershipErr != nil {
			return 0, 0, "", fmt.Errorf("failed to verify project membership: %w", membershipErr)
		}
		if !isMember {
			return 0, 0, "", fmt.Errorf("permission denied: current user is not a project member")
		}
	}

	return currentUser.ID, projectID, currentUser.Nickname, nil
}

func (r *queryResolver) resolveCampusUsageLeaderboard(ctx context.Context, timeWindow *string) ([]*CampusUsageLeaderboardEntry, error) {
	currentUserID, projectID, currentNickname, err := r.campusLeaderboardIdentity(ctx)
	if err != nil {
		return nil, err
	}

	period, err := campusLeaderboardPeriod(xtime.GetCalendarPeriods(campusLeaderboardLocation), timeWindow)
	if err != nil {
		return nil, err
	}
	settings, err := r.systemService.UserDailyQuotaSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get account token limits: %w", err)
	}
	quotaTokenLimit := campusLeaderboardQuotaLimit(settings, timeWindow)
	var aggregates []campusUsageAggregate

	// Membership is verified above. The audited system scope is limited to this
	// aggregate, whose GraphQL DTO exposes only public aliases and nicknames.
	err = authz.RunWithSystemBypassVoid(ctx, "campus-user-usage-leaderboard", func(aggregateCtx context.Context) error {
		return r.client.UsageLog.Query().
			Where(
				usagelog.ProjectIDEQ(projectID),
				usagelog.APIKeyIDNotNil(),
				usagelog.SourceEQ(usagelog.SourceAPI),
				usagelog.CreatedAtGTE(period.Start),
				usagelog.CreatedAtLT(period.End),
			).
			Modify(func(s *sql.Selector) {
				apiKeyTable := sql.Table(apikey.Table)
				userTable := sql.Table("users")

				s.Join(apiKeyTable).On(
					s.C(usagelog.FieldAPIKeyID),
					apiKeyTable.C(apikey.FieldID),
				)
				s.Join(userTable).On(
					apiKeyTable.C(apikey.FieldUserID),
					userTable.C("id"),
				)

				// Intentionally do not filter api_keys.deleted_at: deleting or rotating
				// a key must not erase its already-recorded contribution to today's use.
				s.Select(
					sql.As(userTable.C("id"), "user_id"),
					sql.As(userTable.C("nickname"), "nickname"),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", biz.EffectiveTokensSQL(s)), "recorded_tokens"),
					sql.As(sql.Count(s.C(usagelog.FieldID)), "metered_request_count"),
				).
					GroupBy(userTable.C("id"), userTable.C("nickname"))
			}).
			Scan(aggregateCtx, &aggregates)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get campus usage leaderboard: %w", err)
	}

	return rankCampusUsage(projectID, currentUserID, currentNickname, quotaTokenLimit, aggregates), nil
}

func (r *queryResolver) resolveCampusModelUsageLeaderboard(
	ctx context.Context,
	timeWindow *string,
) ([]*CampusModelUsageLeaderboardEntry, error) {
	_, projectID, _, err := r.campusLeaderboardIdentity(ctx)
	if err != nil {
		return nil, err
	}

	period, err := campusLeaderboardPeriod(xtime.GetCalendarPeriods(campusLeaderboardLocation), timeWindow)
	if err != nil {
		return nil, err
	}

	var aggregates []campusModelUsageAggregate
	err = authz.RunWithSystemBypassVoid(ctx, "campus-model-usage-leaderboard", func(aggregateCtx context.Context) error {
		return r.client.UsageLog.Query().
			Where(
				usagelog.ProjectIDEQ(projectID),
				usagelog.SourceEQ(usagelog.SourceAPI),
				usagelog.CreatedAtGTE(period.Start),
				usagelog.CreatedAtLT(period.End),
			).
			Modify(func(s *sql.Selector) {
				s.Select(
					s.C(usagelog.FieldModelID),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", biz.EffectiveTokensSQL(s)), "effective_tokens"),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", s.C(usagelog.FieldPromptTokens)), "input_tokens"),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", s.C(usagelog.FieldPromptCachedTokens)), "cached_read_tokens"),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", s.C(usagelog.FieldCompletionTokens)), "output_tokens"),
					sql.As(sql.Count(s.C(usagelog.FieldID)), "metered_request_count"),
				).
					GroupBy(s.C(usagelog.FieldModelID))
			}).
			Scan(aggregateCtx, &aggregates)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get campus model usage leaderboard: %w", err)
	}

	return rankCampusModelUsage(aggregates), nil
}
