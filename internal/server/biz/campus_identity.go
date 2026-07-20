package biz

import (
	"crypto/sha256"
	"fmt"
)

// CampusPublicAlias returns a project-scoped stable alias without exposing a
// user's database identifier.
func CampusPublicAlias(projectID, userID int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("axonhub-campus:%d:%d", projectID, userID)))
	return fmt.Sprintf("同学-%x", digest[:4])
}

// CampusDisplayName prefers a valid voluntary nickname and otherwise falls
// back to the privacy-preserving public alias.
func CampusDisplayName(nickname, publicAlias string) string {
	normalized, err := NormalizeCampusNickname(nickname)
	if err != nil || normalized == "" {
		return publicAlias
	}

	return normalized
}
