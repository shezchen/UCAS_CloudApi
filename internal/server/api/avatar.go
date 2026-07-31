package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
)

type AvatarHandlersParams struct {
	fx.In

	Ent *ent.Client
}

type AvatarHandlers struct {
	client *ent.Client
}

func NewAvatarHandlers(params AvatarHandlersParams) *AvatarHandlers {
	return &AvatarHandlers{client: params.Ent}
}

// GetUserAvatar serves a user's avatar by numeric user ID.
func (h *AvatarHandlers) GetUserAvatar(c *gin.Context) {
	ctx := c.Request.Context()

	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		JSONError(c, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}

	projectID, ok := contexts.GetProjectID(ctx)
	if !ok || projectID <= 0 {
		JSONError(c, http.StatusBadRequest, errors.New("project ID not found in context"))
		return
	}

	userID, err := strconv.Atoi(c.Param("user_id"))
	if err != nil || userID <= 0 {
		JSONError(c, http.StatusBadRequest, errors.New("invalid user id"))
		return
	}

	isMember, err := h.client.UserProject.Query().
		Where(
			userproject.ProjectIDEQ(projectID),
			userproject.UserIDEQ(userID),
		).
		Exist(ctx)
	if err != nil {
		log.Error(ctx, "failed to verify avatar subject membership", log.Cause(err))
		JSONError(c, http.StatusInternalServerError, errors.New("failed to load avatar"))
		return
	}
	if !isMember && !currentUser.IsOwner {
		// Non-owners only see avatars of users in the current project.
		JSONError(c, http.StatusNotFound, biz.ErrAvatarNotFound)
		return
	}

	u, err := h.client.User.Query().
		Where(user.IDEQ(userID)).
		Select(user.FieldID, user.FieldAvatar).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			JSONError(c, http.StatusNotFound, biz.ErrAvatarNotFound)
			return
		}
		log.Error(ctx, "failed to load user avatar", log.Cause(err))
		JSONError(c, http.StatusInternalServerError, errors.New("failed to load avatar"))
		return
	}

	h.writeAvatar(c, u.Avatar)
}

// GetCampusAvatar serves a campus avatar by the privacy-preserving public alias.
func (h *AvatarHandlers) GetCampusAvatar(c *gin.Context) {
	ctx := c.Request.Context()

	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		JSONError(c, http.StatusUnauthorized, errors.New("authentication required"))
		return
	}

	projectID, ok := contexts.GetProjectID(ctx)
	if !ok || projectID <= 0 {
		JSONError(c, http.StatusBadRequest, errors.New("project ID not found in context"))
		return
	}

	publicAlias := strings.TrimSpace(c.Param("publicAlias"))
	if publicAlias == "" {
		JSONError(c, http.StatusBadRequest, errors.New("invalid public alias"))
		return
	}

	if !currentUser.IsOwner {
		isMember, membershipErr := h.client.UserProject.Query().
			Where(
				userproject.UserIDEQ(currentUser.ID),
				userproject.ProjectIDEQ(projectID),
			).
			Exist(ctx)
		if membershipErr != nil {
			log.Error(ctx, "failed to verify campus avatar membership", log.Cause(membershipErr))
			JSONError(c, http.StatusInternalServerError, errors.New("failed to load avatar"))
			return
		}
		if !isMember {
			JSONError(c, http.StatusForbidden, errors.New("permission denied"))
			return
		}
	}

	memberships, err := h.client.UserProject.Query().
		Where(userproject.ProjectIDEQ(projectID)).
		WithUser(func(query *ent.UserQuery) {
			query.Select(user.FieldID, user.FieldAvatar)
		}).
		All(ctx)
	if err != nil {
		log.Error(ctx, "failed to list campus members for avatar lookup", log.Cause(err))
		JSONError(c, http.StatusInternalServerError, errors.New("failed to load avatar"))
		return
	}

	for _, membership := range memberships {
		if biz.CampusPublicAlias(projectID, membership.UserID) != publicAlias {
			continue
		}
		if membership.Edges.User == nil {
			JSONError(c, http.StatusNotFound, biz.ErrAvatarNotFound)
			return
		}
		h.writeAvatar(c, membership.Edges.User.Avatar)
		return
	}

	JSONError(c, http.StatusNotFound, biz.ErrAvatarNotFound)
}

func (h *AvatarHandlers) writeAvatar(c *gin.Context, avatar string) {
	mimeType, data, externalURL, err := biz.DecodeAvatarPayload(avatar)
	if err != nil {
		if errors.Is(err, biz.ErrAvatarNotFound) {
			JSONError(c, http.StatusNotFound, biz.ErrAvatarNotFound)
			return
		}
		JSONError(c, http.StatusBadRequest, fmt.Errorf("invalid avatar: %w", err))
		return
	}

	c.Header("Cache-Control", "private, max-age=3600")
	if externalURL != "" {
		c.Redirect(http.StatusFound, externalURL)
		return
	}

	c.Header("Content-Length", strconv.Itoa(len(data)))
	c.Data(http.StatusOK, mimeType, data)
}
