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
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

var (
	ErrCampusCatalogProjectRequired = errors.New("project is required")
	ErrCampusCatalogUnauthorized    = errors.New("authentication required")
	ErrCampusCatalogForbidden       = errors.New("current user is not a member of this project")
	ErrCampusCatalogInvalidInput    = errors.New("invalid campus model capability request")
	ErrCampusChannelNotFound        = errors.New("donated channel not found")
	ErrCampusOwnerOverrideForbidden = errors.New("project owners cannot edit contributor model overrides")
)

const (
	campusChannelDescriptionMaxRunes = 280
	campusModelIDMaxRunes            = 512
	campusCapabilityLimitMax         = 10_000_000
)

type CampusCatalogServiceParams struct {
	fx.In

	Ent            *ent.Client
	ModelService   *ModelService
	ChannelService *ChannelService
}

func NewCampusCatalogService(params CampusCatalogServiceParams) *CampusCatalogService {
	return &CampusCatalogService{
		client:                             params.Ent,
		listEnabledModels:                  params.ModelService.ListEnabledModels,
		resolveChannelModelFacade:          params.ModelService.ResolveChannelModelFacade,
		updateChannelModelMetadataOverride: params.ChannelService.UpdateChannelModelMetadataOverride,
	}
}

type CampusCatalogService struct {
	client                             *ent.Client
	listEnabledModels                  func(context.Context) ([]ModelFacade, error)
	resolveChannelModelFacade          func(*Channel, ChannelModelEntry) ModelFacade
	updateChannelModelMetadataOverride func(context.Context, int, int, string, *objects.ModelMetadataPatch) (*ent.Channel, error)
}

// CampusResources is a deliberately narrow public projection. Never replace
// these DTOs with Ent entities: API keys and channels carry sensitive fields.
type CampusResources struct {
	Models       []string                `json:"models"`
	ModelDetails []CampusModelDetail     `json:"modelDetails"`
	APIKeys      []CampusAPIKeyResources `json:"apiKeys"`
	Channels     []CampusChannelResource `json:"channels"`
}

type CampusAPIKeyResources struct {
	Name         string              `json:"name"`
	Models       []string            `json:"models"`
	ModelDetails []CampusModelDetail `json:"modelDetails"`
}

type CampusModelDetail struct {
	ID              string `json:"id"`
	Source          string `json:"source"`
	Vision          bool   `json:"vision"`
	ToolCall        bool   `json:"toolCall"`
	Reasoning       bool   `json:"reasoning"`
	ContextLength   int    `json:"contextLength"`
	MaxOutputTokens *int   `json:"maxOutputTokens,omitempty"`
	VariesByAPIKey  bool   `json:"variesByAPIKey,omitempty"`
	Overridden      bool   `json:"overridden,omitempty"`
}

type CampusChannelModelCapabilities struct {
	Channels []CampusOwnedChannelCapabilities `json:"channels"`
}

type CampusOwnedChannelCapabilities struct {
	ID     string              `json:"id"`
	Name   string              `json:"name"`
	Models []CampusModelDetail `json:"models"`
}

type CampusModelCapabilityOverride struct {
	Vision          bool
	ToolCall        bool
	Reasoning       bool
	ContextLength   int
	MaxOutputTokens *int
}

type UpdateCampusChannelModelCapabilitiesInput struct {
	ChannelID string
	ModelID   string
	Override  *CampusModelCapabilityOverride
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
		activeProject, err := svc.verifyCampusProjectAccess(bypassCtx, currentUser, projectID)
		if err != nil {
			return nil, err
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
			Models:       []string{},
			ModelDetails: []CampusModelDetail{},
			APIKeys:      []CampusAPIKeyResources{},
			Channels:     []CampusChannelResource{},
		}
		modelSet := make(map[string]struct{})
		type aggregate struct {
			detail       CampusModelDetail
			presentInKey int
		}
		aggregates := make(map[string]aggregate)
		callableKeyCount := 0

		for _, key := range keys {
			if !slices.Contains(key.Scopes, string(scopes.ScopeWriteRequests)) {
				continue
			}
			callableKeyCount++

			keyCtx := contexts.WithAPIKey(bypassCtx, key)
			keyCtx = contexts.WithProjectID(keyCtx, activeProject.ID)
			models, err := svc.listEnabledModels(keyCtx)
			if err != nil {
				return nil, fmt.Errorf("list effective models for API key %q: %w", key.Name, err)
			}

			keyModelSet := make(map[string]struct{}, len(models))
			keyDetails := make(map[string]CampusModelDetail, len(models))
			for _, facade := range models {
				modelID := strings.TrimSpace(facade.ID)
				if modelID == "" {
					continue
				}
				keyModelSet[modelID] = struct{}{}
				modelSet[modelID] = struct{}{}

				detail := campusModelDetailFromFacade(facade)
				detail.ID = modelID
				if existing, ok := keyDetails[modelID]; ok {
					keyDetails[modelID] = mergeCampusModelDetails(existing, detail)
				} else {
					keyDetails[modelID] = detail
				}
			}

			sortedDetails := sortedCampusModelDetails(keyDetails)
			resources.APIKeys = append(resources.APIKeys, CampusAPIKeyResources{
				Name:         key.Name,
				Models:       sortedStringSet(keyModelSet),
				ModelDetails: sortedDetails,
			})

			for _, detail := range sortedDetails {
				current, ok := aggregates[detail.ID]
				if !ok {
					aggregates[detail.ID] = aggregate{detail: detail, presentInKey: 1}
					continue
				}

				if !sameCampusModelDetail(current.detail, detail) {
					current.detail.VariesByAPIKey = true
				}
				current.detail = mergeCampusModelDetails(current.detail, detail)
				current.presentInKey++
				aggregates[detail.ID] = current
			}
		}

		resources.Models = sortedStringSet(modelSet)
		for modelID, current := range aggregates {
			current.detail.ID = modelID
			if current.presentInKey != callableKeyCount {
				current.detail.VariesByAPIKey = true
			}
			aggregates[modelID] = current
		}
		resources.ModelDetails = make([]CampusModelDetail, 0, len(aggregates))
		for _, current := range aggregates {
			resources.ModelDetails = append(resources.ModelDetails, current.detail)
		}
		sort.Slice(resources.ModelDetails, func(i, j int) bool {
			return resources.ModelDetails[i].ID < resources.ModelDetails[j].ID
		})
		resources.Channels, err = svc.listPublicChannels(bypassCtx, projectID, time.Now())
		if err != nil {
			return nil, err
		}

		return resources, nil
	})
}

func (svc *CampusCatalogService) verifyCampusProjectAccess(ctx context.Context, currentUser *ent.User, projectID int) (*ent.Project, error) {
	activeProject, err := svc.client.Project.Query().
		Where(project.IDEQ(projectID), project.StatusEQ(project.StatusActive)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrCampusCatalogForbidden
		}
		return nil, fmt.Errorf("verify campus catalog project: %w", err)
	}

	if currentUser.IsOwner {
		return activeProject, nil
	}

	isMember, err := svc.client.UserProject.Query().
		Where(userproject.UserIDEQ(currentUser.ID), userproject.ProjectIDEQ(projectID)).
		Exist(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify campus catalog membership: %w", err)
	}
	if !isMember {
		return nil, ErrCampusCatalogForbidden
	}

	return activeProject, nil
}

func campusCatalogIdentity(ctx context.Context) (*ent.User, int, error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return nil, 0, ErrCampusCatalogUnauthorized
	}

	projectID, ok := contexts.GetProjectID(ctx)
	if !ok {
		return nil, 0, ErrCampusCatalogProjectRequired
	}

	return currentUser, projectID, nil
}

// GetChannelModelCapabilities returns only the current contributor's active
// donated channels. Project owners use the existing advanced global model page
// and intentionally receive an empty contributor projection here.
func (svc *CampusCatalogService) GetChannelModelCapabilities(ctx context.Context) (*CampusChannelModelCapabilities, error) {
	currentUser, projectID, err := campusCatalogIdentity(ctx)
	if err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "campus-channel-model-capabilities", func(bypassCtx context.Context) (*CampusChannelModelCapabilities, error) {
		if _, err := svc.verifyCampusProjectAccess(bypassCtx, currentUser, projectID); err != nil {
			return nil, err
		}

		result := &CampusChannelModelCapabilities{Channels: []CampusOwnedChannelCapabilities{}}
		if currentUser.IsOwner {
			return result, nil
		}

		now := time.Now()
		ownedChannels, err := svc.client.Channel.Query().
			Where(
				channel.UserIDEQ(currentUser.ID),
				channel.StatusIn(channel.StatusEnabled, channel.StatusDisabled),
				channel.Or(channel.ExpiresAtIsNil(), channel.ExpiresAtGT(now)),
			).
			Select(
				channel.FieldID,
				channel.FieldCreatedAt,
				channel.FieldName,
				channel.FieldType,
				channel.FieldStatus,
				channel.FieldUserID,
				channel.FieldExpiresAt,
				channel.FieldSupportedModels,
				channel.FieldSettings,
			).
			Order(ent.Asc(channel.FieldName), ent.Asc(channel.FieldID)).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("query own donated channels for model capabilities: %w", err)
		}

		for _, row := range ownedChannels {
			owned := CampusOwnedChannelCapabilities{
				ID:     fmt.Sprintf("gid://axonhub/%s/%d", ent.TypeChannel, row.ID),
				Name:   row.Name,
				Models: []CampusModelDetail{},
			}
			if svc.resolveChannelModelFacade == nil {
				return nil, errors.New("channel model metadata resolver is unavailable")
			}

			resolvedChannel := &Channel{Channel: row}
			entries := resolvedChannel.GetModelEntries()
			modelIDs := make([]string, 0, len(entries))
			for modelID := range entries {
				if strings.TrimSpace(modelID) != "" {
					modelIDs = append(modelIDs, modelID)
				}
			}
			sort.Strings(modelIDs)

			for _, modelID := range modelIDs {
				entry := entries[modelID]
				facade := svc.resolveChannelModelFacade(resolvedChannel, entry)
				detail := campusModelDetailFromFacade(facade)
				detail.ID = modelID
				detail.Overridden = row.Settings != nil && row.Settings.ModelMetadataOverrides != nil && row.Settings.ModelMetadataOverrides[modelID] != nil
				owned.Models = append(owned.Models, detail)
			}

			result.Channels = append(result.Channels, owned)
		}

		return result, nil
	})
}

// UpdateChannelModelCapabilities changes only a contributor-owned metadata
// override. It never edits global Model entities, routing associations, channel
// credentials, or any other channel setting.
func (svc *CampusCatalogService) UpdateChannelModelCapabilities(ctx context.Context, input UpdateCampusChannelModelCapabilitiesInput) error {
	currentUser, projectID, err := campusCatalogIdentity(ctx)
	if err != nil {
		return err
	}
	if currentUser.IsOwner {
		return ErrCampusOwnerOverrideForbidden
	}

	channelGUID, err := objects.ParseGUID(strings.TrimSpace(input.ChannelID))
	if err != nil || channelGUID.Type != ent.TypeChannel || channelGUID.ID <= 0 {
		return ErrCampusCatalogInvalidInput
	}
	modelID := strings.TrimSpace(input.ModelID)
	if modelID == "" || utf8.RuneCountInString(modelID) > campusModelIDMaxRunes {
		return ErrCampusCatalogInvalidInput
	}
	if err := validateCampusModelCapabilityOverride(input.Override); err != nil {
		return err
	}

	err = authz.RunWithSystemBypassVoid(ctx, "campus-update-channel-model-capabilities", func(bypassCtx context.Context) error {
		if _, err := svc.verifyCampusProjectAccess(bypassCtx, currentUser, projectID); err != nil {
			return err
		}

		now := time.Now()
		row, err := svc.client.Channel.Query().
			Where(
				channel.IDEQ(channelGUID.ID),
				channel.UserIDEQ(currentUser.ID),
				channel.StatusIn(channel.StatusEnabled, channel.StatusDisabled),
				channel.Or(channel.ExpiresAtIsNil(), channel.ExpiresAtGT(now)),
			).
			Select(
				channel.FieldID,
				channel.FieldCreatedAt,
				channel.FieldName,
				channel.FieldType,
				channel.FieldStatus,
				channel.FieldUserID,
				channel.FieldExpiresAt,
				channel.FieldSupportedModels,
				channel.FieldSettings,
			).
			Only(bypassCtx)
		if err != nil {
			if ent.IsNotFound(err) {
				return ErrCampusChannelNotFound
			}
			return fmt.Errorf("query donated channel for model capability update: %w", err)
		}

		resolvedChannel := &Channel{Channel: row}
		if _, ok := resolvedChannel.GetModelEntries()[modelID]; !ok {
			return ErrCampusChannelNotFound
		}

		if svc.updateChannelModelMetadataOverride == nil {
			return errors.New("channel model metadata writer is unavailable")
		}
		var metadata *objects.ModelMetadataPatch
		if input.Override != nil {
			metadata = campusModelMetadataPatch(input.Override)
		}
		if _, err := svc.updateChannelModelMetadataOverride(bypassCtx, row.ID, currentUser.ID, modelID, metadata); err != nil {
			if errors.Is(err, ErrChannelModelMetadataTargetUnavailable) {
				return ErrCampusChannelNotFound
			}
			return fmt.Errorf("save donated channel model capability override: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func validateCampusModelCapabilityOverride(override *CampusModelCapabilityOverride) error {
	if override == nil {
		return nil
	}
	if override.ContextLength <= 0 || override.ContextLength > campusCapabilityLimitMax {
		return ErrCampusCatalogInvalidInput
	}
	if override.MaxOutputTokens != nil {
		if *override.MaxOutputTokens <= 0 || *override.MaxOutputTokens > campusCapabilityLimitMax || *override.MaxOutputTokens > override.ContextLength {
			return ErrCampusCatalogInvalidInput
		}
	}

	return nil
}

func campusModelMetadataPatch(override *CampusModelCapabilityOverride) *objects.ModelMetadataPatch {
	vision := override.Vision
	toolCall := override.ToolCall
	reasoning := override.Reasoning
	contextLength := override.ContextLength
	patch := &objects.ModelMetadataPatch{
		Vision:   &vision,
		ToolCall: &toolCall,
		Reasoning: &objects.ModelCardReasoningPatch{
			Supported: &reasoning,
			Default:   &reasoning,
		},
		Limit: &objects.ModelCardLimitPatch{Context: &contextLength},
	}
	if override.MaxOutputTokens != nil {
		maxOutputTokens := *override.MaxOutputTokens
		patch.Limit.Output = &maxOutputTokens
	}

	return patch
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

func campusModelDetailFromFacade(facade ModelFacade) CampusModelDetail {
	detail := CampusModelDetail{
		ID:     strings.TrimSpace(facade.ID),
		Source: string(facade.MetadataSource),
	}
	if detail.Source == "" {
		detail.Source = "default"
	}
	metadata := facade.Metadata
	if metadata == nil {
		return detail
	}
	if metadata.Vision != nil {
		detail.Vision = *metadata.Vision
	}
	if metadata.ToolCall != nil {
		detail.ToolCall = *metadata.ToolCall
	}
	if metadata.Reasoning != nil && metadata.Reasoning.Supported != nil {
		detail.Reasoning = *metadata.Reasoning.Supported
	}
	if metadata.Limit != nil {
		if metadata.Limit.Context != nil && *metadata.Limit.Context > 0 {
			detail.ContextLength = *metadata.Limit.Context
		}
		if metadata.Limit.Output != nil && *metadata.Limit.Output > 0 {
			output := *metadata.Limit.Output
			detail.MaxOutputTokens = &output
		}
	}

	return detail
}

func sortedCampusModelDetails(values map[string]CampusModelDetail) []CampusModelDetail {
	result := make([]CampusModelDetail, 0, len(values))
	for _, detail := range values {
		result = append(result, detail)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })

	return result
}

func sameCampusModelDetail(left, right CampusModelDetail) bool {
	if left.ID != right.ID ||
		left.Source != right.Source ||
		left.Vision != right.Vision ||
		left.ToolCall != right.ToolCall ||
		left.Reasoning != right.Reasoning ||
		left.ContextLength != right.ContextLength {
		return false
	}
	if left.MaxOutputTokens == nil || right.MaxOutputTokens == nil {
		return left.MaxOutputTokens == nil && right.MaxOutputTokens == nil
	}

	return *left.MaxOutputTokens == *right.MaxOutputTokens
}

func mergeCampusModelDetails(left, right CampusModelDetail) CampusModelDetail {
	merged := left
	merged.Vision = left.Vision && right.Vision
	merged.ToolCall = left.ToolCall && right.ToolCall
	merged.Reasoning = left.Reasoning && right.Reasoning
	merged.ContextLength = conservativeCampusLimit(left.ContextLength, right.ContextLength)
	merged.MaxOutputTokens = conservativeCampusOptionalLimit(left.MaxOutputTokens, right.MaxOutputTokens)
	merged.VariesByAPIKey = left.VariesByAPIKey || right.VariesByAPIKey
	merged.Overridden = left.Overridden || right.Overridden
	if left.Source != right.Source {
		merged.Source = "mixed"
	}

	return merged
}

func conservativeCampusLimit(left, right int) int {
	if left <= 0 || right <= 0 {
		return 0
	}
	return min(left, right)
}

func conservativeCampusOptionalLimit(left, right *int) *int {
	if left == nil || right == nil || *left <= 0 || *right <= 0 {
		return nil
	}
	value := min(*left, *right)
	return &value
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
