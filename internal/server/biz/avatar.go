package biz

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrAvatarNotFound      = errors.New("avatar not found")
	ErrAvatarInvalidFormat = errors.New("invalid avatar format")
)

// UserAvatarPath is the authenticated admin path that serves a user's avatar
// without embedding the stored base64 payload in list responses.
func UserAvatarPath(userID int) string {
	return fmt.Sprintf("/admin/users/%d/avatar", userID)
}

// CampusAvatarPath is the privacy-preserving campus avatar path keyed by the
// already-public project-scoped alias.
func CampusAvatarPath(publicAlias string) string {
	return "/admin/avatars/campus/" + publicAlias
}

// DecodeAvatarPayload accepts either an https URL or a data-URL image payload.
// For remote URLs, mimeType and data are empty and externalURL is set.
func DecodeAvatarPayload(avatar string) (mimeType string, data []byte, externalURL string, err error) {
	avatar = strings.TrimSpace(avatar)
	if avatar == "" {
		return "", nil, "", ErrAvatarNotFound
	}

	if strings.HasPrefix(avatar, "http://") || strings.HasPrefix(avatar, "https://") {
		return "", nil, avatar, nil
	}

	if !strings.HasPrefix(avatar, "data:") {
		return "", nil, "", ErrAvatarInvalidFormat
	}

	parts := strings.SplitN(avatar, ",", 2)
	if len(parts) != 2 {
		return "", nil, "", ErrAvatarInvalidFormat
	}

	headerPart := parts[0]
	mimeStart := strings.Index(headerPart, ":")
	mimeEnd := strings.Index(headerPart, ";")
	if mimeStart == -1 || mimeEnd == -1 || mimeEnd <= mimeStart+1 {
		return "", nil, "", ErrAvatarInvalidFormat
	}

	mimeType = headerPart[mimeStart+1 : mimeEnd]
	imageData, decodeErr := base64.StdEncoding.DecodeString(parts[1])
	if decodeErr != nil {
		return "", nil, "", fmt.Errorf("%w: %v", ErrAvatarInvalidFormat, decodeErr)
	}
	if len(imageData) == 0 {
		return "", nil, "", ErrAvatarNotFound
	}

	return mimeType, imageData, "", nil
}
