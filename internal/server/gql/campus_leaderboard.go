package gql

import (
	"context"
	"fmt"
	"sort"

	"entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/privacy"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/pkg/xtime"
	"github.com/looplj/axonhub/internal/server/biz"
)

const campusLeaderboardLimit = 50

type campusUsageAggregate struct {
	UserID            int    `json:"user_id"`
	Nickname          string `json:"nickname"`
	DailyTokenLimit   int64  `json:"daily_token_limit"`
	RecordedTokens    int64  `json:"recorded_tokens"`
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
	if dailyTokenLimit <= 0 {
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

func rankCampusUsage(projectID, currentUserID int, currentNickname string, aggregates []campusUsageAggregate) []*CampusUsageLeaderboardEntry {
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
			LimitPercent:      campusLimitPercent(aggregate.RecordedTokens, aggregate.DailyTokenLimit),
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

func (r *queryResolver) resolveCampusUsageLeaderboard(ctx context.Context, timeWindow *string) ([]*CampusUsageLeaderboardEntry, error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return nil, fmt.Errorf("user not found in context")
	}

	projectID, ok := contexts.GetProjectID(ctx)
	if !ok {
		return nil, fmt.Errorf("project ID not found in context")
	}

	if !currentUser.IsOwner {
		isMember, err := r.client.UserProject.Query().
			Where(
				userproject.UserIDEQ(currentUser.ID),
				userproject.ProjectIDEQ(projectID),
			).
			Exist(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to verify project membership: %w", err)
		}
		if !isMember {
			return nil, fmt.Errorf("permission denied: current user is not a project member")
		}
	}

	// General settings are Owner-managed, but members need the configured
	// timezone to share the same calendar-day boundary.
	restrictedReadCtx := privacy.DecisionContext(ctx, privacy.Allow)
	period, err := campusLeaderboardPeriod(xtime.GetCalendarPeriods(r.systemService.TimeLocation(restrictedReadCtx)), timeWindow)
	if err != nil {
		return nil, err
	}
	var aggregates []campusUsageAggregate

	// Membership is verified above. The privacy bypass is deliberately scoped to
	// this aggregate, whose GraphQL DTO contains no direct identifiers or secrets.
	err = r.client.UsageLog.Query().
		Where(
			usagelog.ProjectIDEQ(projectID),
			usagelog.APIKeyIDNotNil(),
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
				sql.As(userTable.C("daily_token_limit"), "daily_token_limit"),
				sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", s.C(usagelog.FieldTotalTokens)), "recorded_tokens"),
				sql.As(sql.Count(s.C(usagelog.FieldID)), "metered_request_count"),
			).
				GroupBy(userTable.C("id"), userTable.C("nickname"), userTable.C("daily_token_limit"))
		}).
		Scan(restrictedReadCtx, &aggregates)
	if err != nil {
		return nil, fmt.Errorf("failed to get campus usage leaderboard: %w", err)
	}

	return rankCampusUsage(projectID, currentUser.ID, currentUser.Nickname, aggregates), nil
}
