package gql

import (
	"context"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
)

func canReadChannelSecrets(ctx context.Context, ch *ent.Channel) bool {
	if ch == nil || ch.ExpiresAt != nil && !ch.ExpiresAt.After(time.Now()) {
		return false
	}

	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return false
	}

	return currentUser.IsOwner || ch.UserID != nil && *ch.UserID == currentUser.ID
}

func filterChannelsByProjectProfile(channels []*ent.Channel, projectProfile *objects.ProjectProfile) []*ent.Channel {
	if projectProfile == nil {
		return channels
	}

	filtered := channels
	if len(projectProfile.ChannelIDs) > 0 {
		filtered = lo.Filter(filtered, func(ch *ent.Channel, _ int) bool {
			return lo.Contains(projectProfile.ChannelIDs, ch.ID)
		})
	}

	if len(projectProfile.ChannelTags) > 0 {
		filtered = lo.Filter(filtered, func(ch *ent.Channel, _ int) bool {
			return projectProfile.MatchChannelTags(ch.Tags)
		})
	}

	return filtered
}
