package gql

import (
	"context"

	"github.com/looplj/axonhub/internal/contexts"
)

func requireUserDailyQuotaSettingsOwner(ctx context.Context) error {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil || !currentUser.IsOwner {
		return ErrNotOwner
	}

	return nil
}
