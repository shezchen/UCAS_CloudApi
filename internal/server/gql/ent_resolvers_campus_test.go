package gql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
)

func TestChannelResolver_UserID(t *testing.T) {
	resolver := &channelResolver{}

	t.Run("donated channel", func(t *testing.T) {
		userID := 42
		got, err := resolver.UserID(context.Background(), &ent.Channel{UserID: &userID})
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, ent.TypeUser, got.Type)
		require.Equal(t, userID, got.ID)
	})

	t.Run("owner global channel", func(t *testing.T) {
		got, err := resolver.UserID(context.Background(), &ent.Channel{})
		require.NoError(t, err)
		require.Nil(t, got)
	})
}
