package biz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"entgo.io/ent/dialect/sql"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelprobe"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/usagelog"
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
	QuotaService   *QuotaService
}

func NewCampusCatalogService(params CampusCatalogServiceParams) *CampusCatalogService {
	return &CampusCatalogService{
		client:                             params.Ent,
		quotaService:                       params.QuotaService,
		walletService:                      NewTokenWalletService(params.Ent),
		listEnabledModels:                  params.ModelService.ListEnabledModels,
		resolveChannelModelFacade:          params.ModelService.ResolveChannelModelFacade,
		updateChannelModelMetadataOverride: params.ChannelService.UpdateChannelModelMetadataOverride,
	}
}

type CampusCatalogService struct {
	client                             *ent.Client
	quotaService                       *QuotaService
	walletService                      *TokenWalletService
	listEnabledModels                  func(context.Context) ([]ModelFacade, error)
	resolveChannelModelFacade          func(*Channel, ChannelModelEntry) ModelFacade
	updateChannelModelMetadataOverride func(context.Context, int, int, string, *objects.ModelMetadataPatch) (*ent.Channel, error)
}

// CampusResources is a deliberately narrow public projection. Never replace
// these DTOs with Ent entities: API keys and channels carry sensitive fields.
type CampusResources struct {
	Models        []string                `json:"models"`
	ModelDetails  []CampusModelDetail     `json:"modelDetails"`
	APIKeys       []CampusAPIKeyResources `json:"apiKeys"`
	Channels      []CampusChannelResource `json:"channels"`
	UsageOverview *CampusUsageOverview    `json:"usageOverview,omitempty"`
}

type CampusUsageOverview struct {
	AccountingMethod string                  `json:"accountingMethod"`
	Daily            CampusQuotaPeriod       `json:"daily"`
	Weekly           CampusQuotaPeriod       `json:"weekly"`
	Wallet           CampusWalletOverview    `json:"wallet"`
	Tokens           CampusTokenOverview     `json:"tokens"`
	Donations        []CampusDonationBenefit `json:"donations"`
}

type CampusQuotaPeriod struct {
	Limit     int64     `json:"limit"`
	Used      int64     `json:"used"`
	Remaining int64     `json:"remaining"`
	ResetAt   time.Time `json:"resetAt"`
}

type CampusWalletOverview struct {
	Balance        int64 `json:"balance"`
	LifetimeEarned int64 `json:"lifetimeEarned"`
	LifetimeSpent  int64 `json:"lifetimeSpent"`
}

type CampusTokenOverview struct {
	Input     int64 `json:"input"`
	CacheRead int64 `json:"cacheRead"`
	Output    int64 `json:"output"`
	Effective int64 `json:"effective"`
}

type CampusDonationBenefit struct {
	ChannelID            string     `json:"channelId"`
	Name                 string     `json:"name"`
	ExpiresAt            *time.Time `json:"expiresAt,omitempty"`
	EffectiveTokens      int64      `json:"effectiveTokens"`
	RewardEligibleTokens int64      `json:"rewardEligibleTokens"`
	CreditTokens         int64      `json:"creditTokens"`
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
	ID              string               `json:"id"`
	Name            string               `json:"name"`
	Provider        string               `json:"provider"`
	Source          string               `json:"source"`
	Description     string               `json:"description,omitempty"`
	Contributor     string               `json:"contributor"`
	Status          string               `json:"status"`
	ExpiresAt       *time.Time           `json:"expiresAt,omitempty"`
	Models          []string             `json:"models"`
	ModelCount      int                  `json:"modelCount"`
	EffectiveTokens int64                `json:"effectiveTokens"`
	CanProbe        bool                 `json:"canProbe"`
	Health          *CampusChannelHealth `json:"health,omitempty"`
}

type CampusChannelHealth struct {
	State               string     `json:"state"`
	RecentSuccessRate   float64    `json:"recentSuccessRate"`
	RecentRequestCount  int        `json:"recentRequestCount"`
	LastCheckedAt       *time.Time `json:"lastCheckedAt,omitempty"`
	LastSuccessAt       *time.Time `json:"lastSuccessAt,omitempty"`
	LastFailureCategory string     `json:"lastFailureCategory,omitempty"`
}

type CampusAPIActivity struct {
	WindowStartedAt time.Time                `json:"windowStartedAt"`
	APIKeys         []CampusAPIActivityKey   `json:"apiKeys"`
	Events          []CampusAPIActivityEvent `json:"events"`
}

type CampusAPIActivityKey struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Suffix           string `json:"suffix"`
	InputTokens      int64  `json:"inputTokens"`
	CachedReadTokens int64  `json:"cachedReadTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	EffectiveTokens  int64  `json:"effectiveTokens"`
	SuccessCount     int    `json:"successCount"`
	ErrorCount       int    `json:"errorCount"`
	LastStatus       string `json:"lastStatus"`
}

type CampusAPIActivityEvent struct {
	RequestID     string    `json:"requestId"`
	APIKeyID      string    `json:"apiKeyId"`
	APIKeyName    string    `json:"apiKeyName"`
	APIKeySuffix  string    `json:"apiKeySuffix"`
	Model         string    `json:"model"`
	Status        string    `json:"status"`
	StatusCode    *int      `json:"statusCode,omitempty"`
	ErrorCategory string    `json:"errorCategory,omitempty"`
	ErrorMessage  string    `json:"errorMessage,omitempty"`
	LatencyMs     *int64    `json:"latencyMs,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
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
		resources.UsageOverview, err = svc.campusUsageOverview(bypassCtx, currentUser.ID)
		if err != nil {
			return nil, err
		}

		return resources, nil
	})
}

func (svc *CampusCatalogService) campusUsageOverview(ctx context.Context, userID int) (*CampusUsageOverview, error) {
	if svc.quotaService == nil || svc.walletService == nil {
		return nil, nil
	}

	quota, err := svc.quotaService.AccountQuotaOverview(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load campus account quota overview: %w", err)
	}

	result := &CampusUsageOverview{
		AccountingMethod: "effective=input-cache_read+output;cache_write_counted;unknown_cache_counted_as_input",
		Daily: CampusQuotaPeriod{
			Limit:     quota.Daily.LimitTokens,
			Used:      quota.Daily.UsedTokens,
			Remaining: quota.Daily.RemainingTokens,
			ResetAt:   quota.Daily.ResetAt,
		},
		Weekly: CampusQuotaPeriod{
			Limit:     quota.Weekly.LimitTokens,
			Used:      quota.Weekly.UsedTokens,
			Remaining: quota.Weekly.RemainingTokens,
			ResetAt:   quota.Weekly.ResetAt,
		},
		Wallet: CampusWalletOverview{
			Balance:        quota.Wallet.BalanceTokens,
			LifetimeEarned: quota.Wallet.LifetimeCreditedTokens,
			LifetimeSpent:  quota.Wallet.LifetimeDebitedTokens,
		},
		Donations: []CampusDonationBenefit{},
	}

	apiKeyIDs, err := svc.client.APIKey.Query().
		Where(apikey.UserIDEQ(userID)).
		IDs(schematype.SkipSoftDelete(ctx))
	if err != nil {
		return nil, fmt.Errorf("list campus account API keys for usage overview: %w", err)
	}
	if len(apiKeyIDs) > 0 {
		type tokenRow struct {
			Input     int64 `json:"input_tokens"`
			CacheRead int64 `json:"cache_read_tokens"`
			Output    int64 `json:"output_tokens"`
			Effective int64 `json:"effective_tokens"`
		}
		var rows []tokenRow
		query := svc.client.UsageLog.Query().
			Where(
				usagelog.APIKeyIDIn(apiKeyIDs...),
				usagelog.SourceNEQ(usagelog.SourceTest),
			)
		if quota.Daily.Window.Start != nil {
			query = query.Where(usagelog.CreatedAtGTE(*quota.Daily.Window.Start))
		}
		if quota.Daily.Window.End != nil {
			query = query.Where(usagelog.CreatedAtLT(*quota.Daily.Window.End))
		}
		if err := query.Modify(func(selector *sql.Selector) {
			selector.Select(
				sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", selector.C(usagelog.FieldPromptTokens)), "input_tokens"),
				sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", selector.C(usagelog.FieldPromptCachedTokens)), "cache_read_tokens"),
				sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", selector.C(usagelog.FieldCompletionTokens)), "output_tokens"),
				sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", effectiveTokensSQL(selector)), "effective_tokens"),
			)
		}).Scan(ctx, &rows); err != nil {
			return nil, fmt.Errorf("aggregate campus account daily tokens: %w", err)
		}
		if len(rows) > 0 {
			result.Tokens = CampusTokenOverview{
				Input:     max(rows[0].Input, int64(0)),
				CacheRead: max(rows[0].CacheRead, int64(0)),
				Output:    max(rows[0].Output, int64(0)),
				Effective: max(rows[0].Effective, int64(0)),
			}
		}
	}

	donations, err := svc.campusDonationBenefits(ctx, userID)
	if err != nil {
		return nil, err
	}
	result.Donations = donations

	return result, nil
}

func (svc *CampusCatalogService) campusDonationBenefits(ctx context.Context, userID int) ([]CampusDonationBenefit, error) {
	ownedChannels, err := svc.client.Channel.Query().
		Where(channel.UserIDEQ(userID)).
		Select(channel.FieldID, channel.FieldName, channel.FieldExpiresAt).
		Order(ent.Asc(channel.FieldName), ent.Asc(channel.FieldID)).
		All(schematype.SkipSoftDelete(ctx))
	if err != nil {
		return nil, fmt.Errorf("list own donated channels for benefits: %w", err)
	}
	if len(ownedChannels) == 0 {
		return []CampusDonationBenefit{}, nil
	}

	startedAt, err := svc.walletService.EnsureStartedAt(ctx, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("load donation wallet cutover: %w", err)
	}

	channelIDs := make([]int, 0, len(ownedChannels))
	for _, ch := range ownedChannels {
		channelIDs = append(channelIDs, ch.ID)
	}

	type aggregateRow struct {
		ChannelID            int   `json:"channel_id"`
		EffectiveTokens      int64 `json:"effective_tokens"`
		RewardEligibleTokens int64 `json:"reward_eligible_tokens"`
		CreditTokens         int64 `json:"credit_tokens"`
	}
	var rows []aggregateRow
	if err := svc.client.UsageLog.Query().
		Where(
			usagelog.ChannelIDIn(channelIDs...),
			usagelog.SourceNEQ(usagelog.SourceTest),
			usagelog.CreatedAtGTE(startedAt),
		).
		Modify(func(selector *sql.Selector) {
			effective := effectiveTokensSQL(selector)
			donorCredit := selector.C(usagelog.FieldDonorCreditTokens)
			selector.
				Select(
					selector.C(usagelog.FieldChannelID),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", effective), "effective_tokens"),
					sql.As(
						fmt.Sprintf(
							"COALESCE(SUM(CASE WHEN COALESCE(%s, 0) > 0 THEN %s ELSE 0 END), 0)",
							donorCredit,
							effective,
						),
						"reward_eligible_tokens",
					),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", donorCredit), "credit_tokens"),
				).
				GroupBy(selector.C(usagelog.FieldChannelID))
		}).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("aggregate own donated channel benefits: %w", err)
	}

	byChannelID := make(map[int]aggregateRow, len(rows))
	for _, row := range rows {
		byChannelID[row.ChannelID] = row
	}

	result := make([]CampusDonationBenefit, 0, len(ownedChannels))
	for _, ch := range ownedChannels {
		row := byChannelID[ch.ID]
		result = append(result, CampusDonationBenefit{
			ChannelID:            strconv.Itoa(ch.ID),
			Name:                 ch.Name,
			ExpiresAt:            ch.ExpiresAt,
			EffectiveTokens:      max(row.EffectiveTokens, int64(0)),
			RewardEligibleTokens: max(row.RewardEligibleTokens, int64(0)),
			CreditTokens:         max(row.CreditTokens, int64(0)),
		})
	}

	return result, nil
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
			channel.FieldSettings,
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

	channelIDs := make([]int, 0, len(channels))
	for _, ch := range channels {
		channelIDs = append(channelIDs, ch.ID)
	}
	healthByChannel, err := svc.channelHealthMap(ctx, channelIDs, now)
	if err != nil {
		return nil, err
	}
	effectiveTokensByChannel, err := svc.channelEffectiveTokens(ctx, projectID, channelIDs)
	if err != nil {
		return nil, err
	}

	result := make([]CampusChannelResource, 0, len(channels))
	for _, ch := range channels {
		modelSet := make(map[string]struct{})
		for requestModel := range (&Channel{Channel: ch}).GetModelEntries() {
			if strings.TrimSpace(requestModel) != "" {
				modelSet[requestModel] = struct{}{}
			}
		}
		models := sortedStringSet(modelSet)
		resource := CampusChannelResource{
			ID:              strconv.Itoa(ch.ID),
			Name:            ch.Name,
			Provider:        ch.Type.String(),
			Source:          "project",
			Contributor:     "项目维护者",
			Status:          ch.Status.String(),
			ExpiresAt:       ch.ExpiresAt,
			Models:          models,
			ModelCount:      len(models),
			EffectiveTokens: effectiveTokensByChannel[ch.ID],
			CanProbe:        ch.Status == channel.StatusEnabled && firstCampusProbeModel(ch) != "",
			Health:          healthByChannel[ch.ID],
		}
		if ch.Status != channel.StatusEnabled {
			resource.Health = &CampusChannelHealth{
				State:               "unhealthy",
				LastFailureCategory: "administratively_disabled",
			}
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

func (svc *CampusCatalogService) channelEffectiveTokens(
	ctx context.Context,
	projectID int,
	channelIDs []int,
) (map[int]int64, error) {
	result := make(map[int]int64, len(channelIDs))
	if len(channelIDs) == 0 {
		return result, nil
	}

	type aggregateRow struct {
		ChannelID       int   `json:"channel_id"`
		EffectiveTokens int64 `json:"effective_tokens"`
	}
	var rows []aggregateRow
	if err := svc.client.UsageLog.Query().
		Where(
			usagelog.ProjectIDEQ(projectID),
			usagelog.ChannelIDIn(channelIDs...),
			usagelog.SourceNEQ(usagelog.SourceTest),
		).
		Modify(func(selector *sql.Selector) {
			selector.
				Select(
					selector.C(usagelog.FieldChannelID),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", effectiveTokensSQL(selector)), "effective_tokens"),
				).
				GroupBy(selector.C(usagelog.FieldChannelID))
		}).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("aggregate public campus channel tokens: %w", err)
	}

	for _, row := range rows {
		result[row.ChannelID] = max(row.EffectiveTokens, int64(0))
	}

	return result, nil
}

func firstCampusProbeModel(ch *ent.Channel) string {
	candidates := campusProbeModelCandidates(ch, nil)
	if len(candidates) == 0 {
		return ""
	}

	return candidates[0]
}

// campusProbeModelCandidates deliberately does not trust any single source of
// model truth. A configured default is preferred only while it is still in the
// synchronized catalog; recent successful production models are the next
// fallback; remaining synchronized models are ordered by version rather than
// by their incidental array position.
func campusProbeModelCandidates(ch *ent.Channel, recentSuccessfulModels []string) []string {
	if ch == nil {
		return nil
	}

	supportedModels := append([]string(nil), ch.SupportedModels...)
	sort.SliceStable(supportedModels, func(i, j int) bool {
		return campusModelVersionNewer(supportedModels[i], supportedModels[j])
	})
	supportedSet := make(map[string]struct{}, len(supportedModels))
	for _, modelID := range supportedModels {
		if modelID = strings.TrimSpace(modelID); modelID != "" {
			supportedSet[modelID] = struct{}{}
		}
	}

	candidates := make([]string, 0, 1+len(recentSuccessfulModels)+len(supportedModels))
	seen := make(map[string]struct{}, cap(candidates))
	appendUnique := func(modelID string) {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			return
		}
		if _, ok := seen[modelID]; ok {
			return
		}
		seen[modelID] = struct{}{}
		candidates = append(candidates, modelID)
	}
	appendIfSynchronized := func(modelID string) {
		modelID = strings.TrimSpace(modelID)
		if _, ok := supportedSet[modelID]; !ok {
			return
		}
		appendUnique(modelID)
	}

	appendIfSynchronized(ch.DefaultTestModel)
	for _, modelID := range recentSuccessfulModels {
		appendIfSynchronized(modelID)
	}
	for _, modelID := range supportedModels {
		appendUnique(modelID)
	}

	return candidates
}

func campusModelVersionNewer(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	leftNumbers := campusModelVersionNumbers(left)
	rightNumbers := campusModelVersionNumbers(right)

	common := min(len(leftNumbers), len(rightNumbers))
	for i := 0; i < common; i++ {
		if leftNumbers[i] != rightNumbers[i] {
			return leftNumbers[i] > rightNumbers[i]
		}
	}
	if len(leftNumbers) != len(rightNumbers) {
		return len(leftNumbers) > len(rightNumbers)
	}

	return strings.ToLower(left) > strings.ToLower(right)
}

func campusModelVersionNumbers(modelID string) []int {
	numbers := make([]int, 0, 4)
	for i := 0; i < len(modelID); {
		if modelID[i] < '0' || modelID[i] > '9' {
			i++
			continue
		}
		start := i
		for i < len(modelID) && modelID[i] >= '0' && modelID[i] <= '9' {
			i++
		}
		value, err := strconv.Atoi(modelID[start:i])
		if err != nil {
			value = int(^uint(0) >> 1)
		}
		numbers = append(numbers, value)
	}

	return numbers
}

func (svc *CampusCatalogService) channelHealthMap(
	ctx context.Context,
	channelIDs []int,
	now time.Time,
) (map[int]*CampusChannelHealth, error) {
	result := make(map[int]*CampusChannelHealth, len(channelIDs))
	if len(channelIDs) == 0 {
		return result, nil
	}

	for _, channelID := range channelIDs {
		result[channelID] = &CampusChannelHealth{State: "unknown"}
	}

	cutoff := now.Add(-6 * time.Hour)
	probes, err := svc.client.ChannelProbe.Query().
		Where(
			channelprobe.ChannelIDIn(channelIDs...),
			channelprobe.TimestampGTE(cutoff.Unix()),
		).
		Order(ent.Asc(channelprobe.FieldTimestamp), ent.Asc(channelprobe.FieldID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query public channel health: %w", err)
	}

	type healthAccumulator struct {
		total         int
		success       int
		lastPoint     *ent.ChannelProbe
		previousPoint *ent.ChannelProbe
		lastSuccessAt *time.Time
	}
	accumulators := make(map[int]*healthAccumulator, len(channelIDs))
	for _, probe := range probes {
		accumulator := accumulators[probe.ChannelID]
		if accumulator == nil {
			accumulator = &healthAccumulator{}
			accumulators[probe.ChannelID] = accumulator
		}
		accumulator.total += max(probe.TotalRequestCount, 0)
		accumulator.success += max(probe.SuccessRequestCount, 0)
		accumulator.previousPoint = accumulator.lastPoint
		accumulator.lastPoint = probe
		if probe.SuccessRequestCount > 0 {
			successAt := time.Unix(probe.Timestamp, 0).UTC()
			accumulator.lastSuccessAt = &successAt
		}
	}

	for channelID, accumulator := range accumulators {
		if accumulator.lastPoint == nil {
			continue
		}
		checkedAt := time.Unix(accumulator.lastPoint.Timestamp, 0).UTC()
		health := &CampusChannelHealth{
			State:              "unknown",
			RecentRequestCount: accumulator.total,
			LastCheckedAt:      &checkedAt,
			LastSuccessAt:      accumulator.lastSuccessAt,
		}
		if accumulator.total > 0 {
			health.RecentSuccessRate = float64(accumulator.success) / float64(accumulator.total)
		}

		latestFailed := accumulator.lastPoint.TotalRequestCount > 0 &&
			accumulator.lastPoint.SuccessRequestCount == 0
		previousFailed := accumulator.previousPoint != nil &&
			accumulator.previousPoint.TotalRequestCount > 0 &&
			accumulator.previousPoint.SuccessRequestCount == 0
		latestSucceeded := accumulator.lastPoint.SuccessRequestCount > 0

		switch {
		case latestFailed:
			health.State = "unhealthy"
		case latestSucceeded && previousFailed:
			health.State = "recovering"
		case health.RecentSuccessRate >= 0.9:
			health.State = "healthy"
		case health.RecentSuccessRate > 0:
			health.State = "degraded"
		case accumulator.total > 0:
			health.State = "unhealthy"
		default:
			health.State = "unknown"
		}
		result[channelID] = health
	}

	failures, err := svc.client.RequestExecution.Query().
		Where(
			requestexecution.ChannelIDIn(channelIDs...),
			requestexecution.CreatedAtGTE(cutoff),
			requestexecution.StatusIn(requestexecution.StatusFailed, requestexecution.StatusCanceled),
		).
		Order(ent.Desc(requestexecution.FieldCreatedAt), ent.Desc(requestexecution.FieldID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query public channel failure categories: %w", err)
	}
	seenFailure := make(map[int]struct{}, len(channelIDs))
	for _, execution := range failures {
		if _, ok := seenFailure[execution.ChannelID]; ok {
			continue
		}
		seenFailure[execution.ChannelID] = struct{}{}
		result[execution.ChannelID].LastFailureCategory = campusFailureCategory(
			execution.ResponseStatusCode,
			execution.ErrorMessage,
		)
	}

	return result, nil
}

func campusFailureCategory(statusCode *int, message string) string {
	if statusCode != nil {
		switch {
		case *statusCode == 401 || *statusCode == 403:
			return "authentication"
		case *statusCode == 408:
			return "timeout"
		case *statusCode == 429:
			return "rate_limited"
		case *statusCode >= 500:
			return "upstream_unavailable"
		case *statusCode >= 400:
			return "request_rejected"
		}
	}

	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline"):
		return "timeout"
	case strings.Contains(lower, "stream ended without terminal"),
		strings.Contains(lower, "incomplete stream"):
		return "upstream_unavailable"
	case strings.Contains(lower, "connection"), strings.Contains(lower, "tls"), strings.Contains(lower, "network"):
		return "network"
	case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "invalid token"), strings.Contains(lower, "authentication"):
		return "authentication"
	case strings.Contains(lower, "empty response"), strings.Contains(lower, "no content"):
		return "empty_response"
	case strings.Contains(lower, "rate limit"):
		return "rate_limited"
	case strings.TrimSpace(lower) != "":
		return "provider_error"
	default:
		return "unknown"
	}
}

// PrepareChannelProbe authorizes a project member against the privacy-safe
// catalog and prepares an ordered server-side fallback chain. The caller never
// supplies a model, URL, proxy or credential.
func (svc *CampusCatalogService) PrepareChannelProbe(
	ctx context.Context,
	channelID int,
) (objects.GUID, []string, error) {
	currentUser, projectID, err := campusCatalogIdentity(ctx)
	if err != nil {
		return objects.GUID{}, nil, err
	}
	if channelID <= 0 {
		return objects.GUID{}, nil, ErrCampusCatalogInvalidInput
	}

	type preparedProbe struct {
		guid   objects.GUID
		models []string
	}
	prepared, err := authz.RunWithSystemBypass(ctx, "campus-public-channel-probe", func(bypassCtx context.Context) (preparedProbe, error) {
		if _, err := svc.verifyCampusProjectAccess(bypassCtx, currentUser, projectID); err != nil {
			return preparedProbe{}, err
		}

		now := time.Now()
		ch, err := svc.client.Channel.Query().
			Where(
				channel.IDEQ(channelID),
				channel.StatusEQ(channel.StatusEnabled),
				channel.Or(channel.ExpiresAtIsNil(), channel.ExpiresAtGT(now)),
			).
			Only(bypassCtx)
		if err != nil {
			if ent.IsNotFound(err) {
				return preparedProbe{}, ErrCampusChannelNotFound
			}
			return preparedProbe{}, fmt.Errorf("load channel for public probe: %w", err)
		}

		recentExecutions, err := svc.client.RequestExecution.Query().
			Where(
				requestexecution.ChannelIDEQ(ch.ID),
				requestexecution.StatusEQ(requestexecution.StatusCompleted),
				requestexecution.ModelIDNEQ(""),
				requestexecution.HasRequestWith(request.SourceEQ(request.SourceAPI)),
			).
			Select(requestexecution.FieldModelID).
			Order(ent.Desc(requestexecution.FieldCreatedAt), ent.Desc(requestexecution.FieldID)).
			Limit(50).
			All(bypassCtx)
		if err != nil {
			return preparedProbe{}, fmt.Errorf("query recent successful channel models: %w", err)
		}
		recentModels := make([]string, 0, len(recentExecutions))
		for _, execution := range recentExecutions {
			recentModels = append(recentModels, execution.ModelID)
		}

		models := campusProbeModelCandidates(ch, recentModels)
		if len(models) == 0 {
			return preparedProbe{}, ErrCampusCatalogInvalidInput
		}

		return preparedProbe{
			guid:   objects.GUID{Type: ent.TypeChannel, ID: ch.ID},
			models: models,
		}, nil
	})
	if err != nil {
		return objects.GUID{}, nil, err
	}

	return prepared.guid, prepared.models, nil
}

// GetChannelHealth returns only the narrow health projection after verifying
// project membership and public-channel eligibility.
func (svc *CampusCatalogService) GetChannelHealth(ctx context.Context, channelID int) (*CampusChannelHealth, error) {
	currentUser, projectID, err := campusCatalogIdentity(ctx)
	if err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "campus-public-channel-health", func(bypassCtx context.Context) (*CampusChannelHealth, error) {
		if _, err := svc.verifyCampusProjectAccess(bypassCtx, currentUser, projectID); err != nil {
			return nil, err
		}
		exists, err := svc.client.Channel.Query().
			Where(
				channel.IDEQ(channelID),
				channel.Or(channel.ExpiresAtIsNil(), channel.ExpiresAtGT(time.Now())),
			).
			Exist(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("verify public channel health target: %w", err)
		}
		if !exists {
			return nil, ErrCampusChannelNotFound
		}

		healthByChannel, err := svc.channelHealthMap(bypassCtx, []int{channelID}, time.Now())
		if err != nil {
			return nil, err
		}
		return healthByChannel[channelID], nil
	})
}

// GetAPIActivity exposes a six-hour, current-user-only diagnostic projection.
// It deliberately omits request/response bodies, headers, IPs, URLs and full
// API key values.
func (svc *CampusCatalogService) GetAPIActivity(ctx context.Context, selectedAPIKeyID *int) (*CampusAPIActivity, error) {
	currentUser, projectID, err := campusCatalogIdentity(ctx)
	if err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "campus-own-api-activity", func(bypassCtx context.Context) (*CampusAPIActivity, error) {
		if _, err := svc.verifyCampusProjectAccess(bypassCtx, currentUser, projectID); err != nil {
			return nil, err
		}

		windowStartedAt := time.Now().Add(-campusExecutionErrorRetention)
		keyQuery := svc.client.APIKey.Query().
			Where(
				apikey.UserIDEQ(currentUser.ID),
				apikey.ProjectIDEQ(projectID),
			).
			Select(apikey.FieldID, apikey.FieldName, apikey.FieldKey).
			Order(ent.Asc(apikey.FieldName), ent.Asc(apikey.FieldID))
		if selectedAPIKeyID != nil {
			keyQuery = keyQuery.Where(apikey.IDEQ(*selectedAPIKeyID))
		}

		keys, err := keyQuery.All(schematype.SkipSoftDelete(bypassCtx))
		if err != nil {
			return nil, fmt.Errorf("query own API keys for campus activity: %w", err)
		}
		if selectedAPIKeyID != nil && len(keys) == 0 {
			return nil, ErrCampusCatalogForbidden
		}

		result := &CampusAPIActivity{
			WindowStartedAt: windowStartedAt,
			APIKeys:         make([]CampusAPIActivityKey, 0, len(keys)),
			Events:          []CampusAPIActivityEvent{},
		}
		if len(keys) == 0 {
			return result, nil
		}

		keyByID := make(map[int]*ent.APIKey, len(keys))
		summaryIndexByID := make(map[int]int, len(keys))
		keyIDs := make([]int, 0, len(keys))
		for _, key := range keys {
			keyByID[key.ID] = key
			keyIDs = append(keyIDs, key.ID)
			summary := CampusAPIActivityKey{
				ID:     strconv.Itoa(key.ID),
				Name:   key.Name,
				Suffix: campusAPIKeySuffix(key.Key),
			}
			result.APIKeys = append(result.APIKeys, summary)
			summaryIndexByID[key.ID] = len(result.APIKeys) - 1
		}

		requests, err := svc.client.Request.Query().
			Where(
				request.ProjectIDEQ(projectID),
				request.APIKeyIDIn(keyIDs...),
				request.SourceEQ(request.SourceAPI),
				request.CreatedAtGTE(windowStartedAt),
			).
			WithExecutions(func(query *ent.RequestExecutionQuery) {
				query.Order(ent.Desc(requestexecution.FieldCreatedAt), ent.Desc(requestexecution.FieldID))
			}).
			WithUsageLogs().
			Order(ent.Desc(request.FieldCreatedAt), ent.Desc(request.FieldID)).
			Limit(200).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("query own recent API activity: %w", err)
		}

		for _, req := range requests {
			key := keyByID[req.APIKeyID]
			summaryIndex, hasSummary := summaryIndexByID[req.APIKeyID]
			if key == nil || !hasSummary {
				continue
			}
			summary := &result.APIKeys[summaryIndex]

			status := string(req.Status)
			if summary.LastStatus == "" {
				summary.LastStatus = status
			}
			switch req.Status {
			case request.StatusCompleted:
				summary.SuccessCount++
			case request.StatusFailed, request.StatusCanceled:
				summary.ErrorCount++
			}

			for _, usage := range req.Edges.UsageLogs {
				summary.InputTokens += max(usage.PromptTokens, int64(0))
				summary.CachedReadTokens += max(usage.PromptCachedTokens, int64(0))
				summary.OutputTokens += max(usage.CompletionTokens, int64(0))
				summary.EffectiveTokens += campusStoredEffectiveTokens(usage)
			}

			if len(result.Events) >= 100 {
				continue
			}

			event := CampusAPIActivityEvent{
				RequestID:    strconv.Itoa(req.ID),
				APIKeyID:     strconv.Itoa(key.ID),
				APIKeyName:   key.Name,
				APIKeySuffix: campusAPIKeySuffix(key.Key),
				Model:        req.ModelID,
				Status:       status,
				LatencyMs:    req.MetricsLatencyMs,
				CreatedAt:    req.CreatedAt,
			}
			if len(req.Edges.Executions) > 0 {
				execution := req.Edges.Executions[0]
				event.Status = string(execution.Status)
				event.StatusCode = execution.ResponseStatusCode
				if execution.ModelID != "" {
					event.Model = execution.ModelID
				}
				if execution.MetricsLatencyMs != nil {
					event.LatencyMs = execution.MetricsLatencyMs
				}
				if execution.Status == requestexecution.StatusFailed ||
					execution.Status == requestexecution.StatusCanceled {
					event.ErrorCategory = campusFailureCategory(execution.ResponseStatusCode, execution.ErrorMessage)
					event.ErrorMessage = sanitizeRequestExecutionErrorMessage(execution.ErrorMessage)
				}
			}
			result.Events = append(result.Events, event)
		}

		return result, nil
	})
}

func campusAPIKeySuffix(key string) string {
	runes := []rune(strings.TrimSpace(key))
	if len(runes) <= 4 {
		return string(runes)
	}

	return string(runes[len(runes)-4:])
}

func campusStoredEffectiveTokens(usage *ent.UsageLog) int64 {
	if usage == nil {
		return 0
	}
	return StoredEffectiveTokens(
		usage.EffectiveTokens,
		usage.CacheReadTokensKnown,
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.TotalTokens,
		usage.PromptCachedTokens,
	)
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
