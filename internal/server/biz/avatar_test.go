package biz

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeAvatarPayload(t *testing.T) {
	t.Parallel()

	_, _, _, err := DecodeAvatarPayload("")
	require.ErrorIs(t, err, ErrAvatarNotFound)

	_, _, url, err := DecodeAvatarPayload("https://cdn.example.com/a.png")
	require.NoError(t, err)
	require.Equal(t, "https://cdn.example.com/a.png", url)

	payload := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	mime, data, external, err := DecodeAvatarPayload("data:image/png;base64," + payload)
	require.NoError(t, err)
	require.Equal(t, "image/png", mime)
	require.Equal(t, []byte("png-bytes"), data)
	require.Empty(t, external)

	require.Equal(t, "/admin/users/42/avatar", UserAvatarPath(42))
	require.Equal(t, "/admin/avatars/campus/同学-deadbeef", CampusAvatarPath("同学-deadbeef"))
}
