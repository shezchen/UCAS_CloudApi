package biz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/scopes"
)

var (
	ErrCampusCatalogProjectRequired = errors.New("project is required")
	ErrCampusCatalogUnauthorized    = errors.New("authentication required")
	ErrCampusCatalogForbidden       = errors.New("current user is not a member of this project")
)

const campusChannelDescriptionMaxRunes = 280

type CampusCatalogServiceParams struct {
	fx.In

	Ent          *ent.Client
	ModelService *ModelService
}

func NewCampusCatalogService(params CampusCatalogServiceParams) *CampusCatalogService {
	return &CampusCatalogService{
		client:            params.Ent,
		listEnabledModels: params.ModelService.ListEnabledModels,
	}
}

type CampusCatalogService struct {
	client            *ent.Client
	listEnabledModels func(context.Context) ([]ModelFacade, error)
}

// CampusResources is a deliberately narrow public projection. Never replace
// these DTOs with Ent entities: API keys and channels carry sensitive fields.
type CampusResources struct {
	Models   []string                `json:"models"`
	APIKeys  []CampusAPIKeyResources `json:"apiKeys"`
	Channels []CampusChannelResource `json:"channels"`
}

type CampusAPIKeyResources struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

type CampusChannelResource struct {
	Name        string     `json:"name"`
	Provider    string     `json:"provider"`
	Source      string     `json:"source"`
	Description string     `json:"description,omitempty"`
	Contributor string     `json:"contributor"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	ModelCount  int        `json:"modelCount"`
}

// GetResources returns the effective model names for the current user's own
// callable API keys and a privacy-safe directory of shared channels.
func (svc *CampusCatalogService) GetResources(ctx context.Context) (*CampusResources, error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return nil, ErrCampusCatalogUnauthorized
	}

	projectID, ok := contexts.GetProjectID(ctx)
	if !ok {
		return nil, ErrCampusCatalogProjectRequired
	}

	return authz.RunWithSystemBypass(ctx, "campus-resource-catalog", func(bypassCtx context.Context) (*CampusResources, error) {
		activeProject, err := svc.client.Project.Query().
			Where(project.IDEQ(projectID), project.StatusEQ(project.StatusActive)).
			Only(bypassCtx)
		if err != nil {
			if ent.IsNotFound(err) {
				return nil, ErrCampusCatalogForbidden
			}
			return nil, fmt.Errorf("verify campus catalog project: %w", err)
		}

		if !currentUser.IsOwner {
			isMember, err := svc.client.UserProject.Query().
				Where(userproject.UserIDEQ(currentUser.ID), userproject.ProjectIDEQ(projectID)).
				Exist(bypassCtx)
			if err != nil {
				return nil, fmt.Errorf("verify campus catalog membership: %w", err)
			}
			if !isMember {
				return nil, ErrCampusCatalogForbidden
			}
		}

		keys, err := svc.client.APIKey.Query().
			Where(
				apikey.UserIDEQ(currentUser.ID),
				apikey.ProjectIDEQ(projectID),
				apikey.StatusEQ(apikey.StatusEnabled),
				apikey.TypeNEQ(apikey.TypeNoauth),
			).
			Select(
				apikey.FieldID,
				apikey.FieldUserID,
				apikey.FieldProjectID,
				apikey.FieldName,
				apikey.FieldType,
				apikey.FieldStatus,
				apikey.FieldScopes,
				apikey.FieldProfiles,
			).
			WithProject(func(query *ent.ProjectQuery) {
				query.Select(project.FieldID, project.FieldProfiles)
			}).
			Order(ent.Asc(apikey.FieldName), ent.Asc(apikey.FieldID)).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("query own campus API keys: %w", err)
		}

		resources := &CampusResources{
			Models:   []string{},
			APIKeys:  []CampusAPIKeyResources{},
			Channels: []CampusChannelResource{},
		}
		modelSet := make(map[string]struct{})

		for _, key := range keys {
			if !slices.Contains(key.Scopes, string(scopes.ScopeWriteRequests)) {
				continue
			}

			keyCtx := contexts.WithAPIKey(bypassCtx, key)
			keyCtx = contexts.WithProjectID(keyCtx, activeProject.ID)
			models, err := svc.listEnabledModels(keyCtx)
			if err != nil {
				return nil, fmt.Errorf("list effective models for API key %q: %w", key.Name, err)
			}

			keyModelSet := make(map[string]struct{}, len(models))
			for _, model := range models {
				modelID := strings.TrimSpace(model.ID)
				if modelID == "" {
					continue
				}
				keyModelSet[modelID] = struct{}{}
				modelSet[modelID] = struct{}{}
			}

			resources.APIKeys = append(resources.APIKeys, CampusAPIKeyResources{
				Name:   key.Name,
				Models: sortedStringSet(keyModelSet),
			})
		}

		resources.Models = sortedStringSet(modelSet)
		resources.Channels, err = svc.listPublicChannels(bypassCtx, projectID, time.Now())
		if err != nil {
			return nil, err
		}

		return resources, nil
	})
}

func (svc *CampusCatalogService) listPublicChannels(ctx context.Context, projectID int, now time.Time) ([]CampusChannelResource, error) {
	channels, err := svc.client.Channel.Query().
		Where(
			channel.StatusIn(channel.StatusEnabled, channel.StatusDisabled),
			channel.Or(channel.ExpiresAtIsNil(), channel.ExpiresAtGT(now)),
		).
		Select(
			channel.FieldID,
			channel.FieldName,
			channel.FieldType,
			channel.FieldStatus,
			channel.FieldUserID,
			channel.FieldExpiresAt,
			channel.FieldSupportedModels,
			channel.FieldRemark,
		).
		WithUser(func(query *ent.UserQuery) {
			query.Select(user.FieldID, user.FieldNickname)
		}).
		Order(ent.Asc(channel.FieldName), ent.Asc(channel.FieldID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query public campus channels: %w", err)
	}

	result := make([]CampusChannelResource, 0, len(channels))
	for _, ch := range channels {
		resource := CampusChannelResource{
			Name:        ch.Name,
			Provider:    ch.Type.String(),
			Source:      "project",
			Contributor: "项目维护者",
			Status:      ch.Status.String(),
			ExpiresAt:   ch.ExpiresAt,
			ModelCount:  uniqueNonEmptyCount(ch.SupportedModels),
		}

		if ch.UserID != nil {
			resource.Source = "donated"
			resource.Description = sanitizeCampusChannelDescription(ch.Remark)
			alias := CampusPublicAlias(projectID, *ch.UserID)
			if ch.Edges.User == nil {
				resource.Contributor = alias
			} else {
				resource.Contributor = CampusDisplayName(ch.Edges.User.Nickname, alias)
			}
		}

		result = append(result, resource)
	}

	return result, nil
}

func sortedStringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func uniqueNonEmptyCount(values []string) int {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = struct{}{}
		}
	}
	return len(set)
}

func sanitizeCampusChannelDescription(remark *string) string {
	if remark == nil {
		return ""
	}

	clean := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(*remark))
	clean = strings.Join(strings.Fields(clean), " ")
	if utf8.RuneCountInString(clean) <= campusChannelDescriptionMaxRunes {
		return clean
	}

	runes := []rune(clean)
	return string(runes[:campusChannelDescriptionMaxRunes])
}
