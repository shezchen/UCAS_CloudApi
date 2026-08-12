package biz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samber/lo"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/watcher"
	"github.com/looplj/axonhub/internal/pkg/xapikey"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/pkg/xcache/live"
	"github.com/looplj/axonhub/internal/scopes"
)

const (
	//nolint:gosec // Checked.
	NoAuthAPIKeyValue = "AXONHUB_API_KEY_NO_AUTH"

	//nolint:gosec // Checked.
	NoAuthAPIKeyName = "No Auth System Key"
)

func personalAPIKeyScopes() []string {
	return []string{
		string(scopes.ScopeReadChannels),
		string(scopes.ScopeWriteRequests),
	}
}

func authorizeUserAPIKeyManagement(ctx context.Context, apiKey *ent.APIKey) error {
	currentUser, hasUser := contexts.GetUser(ctx)
	if !hasUser || currentUser == nil {
		// API-key principals (including the OpenAPI service-account path) have no
		// user in context. Leave those callers to the APIKey Ent scope policy.
		return nil
	}

	if currentUser.IsOwner || (apiKey.Type == apikey.TypePersonal && apiKey.UserID == currentUser.ID) {
		return nil
	}

	return fmt.Errorf("non-owner users can only manage their own personal API keys")
}

type APIKeyServiceParams struct {
	fx.In

	CacheConfig    xcache.Config
	Ent            *ent.Client
	ProjectService *ProjectService
	KeyPrefix      string `name:"api_key_prefix"`
}

type APIKeyService struct {
	*AbstractService

	ProjectService *ProjectService
	APIKeyCache    *live.IndexedCache[string, *ent.APIKey]
	apiKeyNotifier watcher.Notifier[live.CacheEvent[string]]
	keyPrefix      string
}

func NewAPIKeyService(params APIKeyServiceParams) *APIKeyService {
	svc := &APIKeyService{
		AbstractService: &AbstractService{
			db: params.Ent,
		},
		ProjectService: params.ProjectService,
		keyPrefix:      params.KeyPrefix,
	}

	cacheMode := params.CacheConfig.Mode
	if cacheMode == "" {
		cacheMode = xcache.ModeMemory
	}

	watcherMode := cacheMode
	if watcherMode == xcache.ModeTwoLevel {
		watcherMode = watcher.ModeRedis
	}

	// The cache name, watcher channel, and cache-key format carry a schema
	// version segment (v2: hash-based identity) so instances running the old
	// plaintext-key scheme never exchange incompatible invalidation events
	// with upgraded instances.
	notifier, err := watcher.NewWatcherFromConfig[live.CacheEvent[string]](watcher.Config{
		Mode:  watcherMode,
		Redis: params.CacheConfig.Redis,
	}, watcher.WatcherFromConfigOptions{
		RedisChannel: "axonhub:cache:api_keys:v2",
		Buffer:       32,
	})
	if err != nil {
		panic(fmt.Errorf("api key watcher init failed: %w", err))
	}

	ttl := params.CacheConfig.Memory.Expiration
	if ttl == 0 {
		ttl = 5 * time.Minute
	}

	svc.apiKeyNotifier = notifier
	svc.APIKeyCache = live.NewIndexedCache(live.IndexedOptions[string, *ent.APIKey]{
		Name:            "axonhub:api_keys:v2",
		TTL:             ttl,
		RefreshInterval: 30 * time.Second,
		DebounceDelay:   500 * time.Millisecond,
		KeyFunc:         func(v *ent.APIKey) string { return apiKeyCacheKeyForHash(v.KeyHash) },
		DeletedFunc:     func(v *ent.APIKey) bool { return v.DeletedAt != 0 },
		Watcher:         notifier,
		LoadOneFunc:     svc.onLoadOneKey,
		LoadSinceFunc:   svc.onLoadAPIKeysSince,
	})

	if err := svc.APIKeyCache.Load(context.Background()); err != nil {
		panic(fmt.Errorf("api key cache initial load failed: %w", err))
	}

	return svc
}

func (s *APIKeyService) Stop() {
	s.APIKeyCache.Stop()
}

func (s *APIKeyService) loadAPIKeyByKey(ctx context.Context, cacheKey string) (*ent.APIKey, error) {
	originalKey, ok := ctx.Value(apiKeyCtxKey{}).(string)
	if !ok || originalKey == "" {
		return nil, live.ErrKeyNotFound
	}

	client := s.entFromContext(ctx)
	keyHash := xapikey.Hash(originalKey)

	item, err := client.APIKey.Query().Where(apikey.KeyHashEQ(keyHash), apikey.DeletedAtEQ(0)).First(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, err
	}

	if item == nil {
		// Migration fallback: rows written by pre-hash instances still hold the
		// plaintext key and no hash. Match on the legacy column and repair the
		// row in place so subsequent lookups use the hash path.
		item, err = s.loadAndRepairLegacyAPIKey(ctx, originalKey, keyHash)
		if err != nil {
			return nil, err
		}
	}

	// Verify the loaded row actually corresponds to the requested cache entry
	// before it is stored under that key (guards against any key derivation
	// mismatch handing out a foreign API key).
	if item.KeyHash != keyHash || apiKeyCacheKeyForHash(item.KeyHash) != cacheKey {
		return nil, live.ErrKeyNotFound
	}

	return item, nil
}

// loadAndRepairLegacyAPIKey matches a raw key against the legacy plaintext
// column (only for rows that have no key_hash yet) and backfills the hashed
// form. This keeps authentication working for rows created by not-yet-upgraded
// instances during a rolling deploy; the startup data migration handles the
// bulk of existing rows.
func (s *APIKeyService) loadAndRepairLegacyAPIKey(ctx context.Context, rawKey, keyHash string) (*ent.APIKey, error) {
	// The key column holds redacted display values for every migrated row,
	// and those values are readable by anyone with read_api_keys. Matching
	// one here would let the display value authenticate — and the repair
	// below would then make it a permanent credential.
	if xapikey.IsRedacted(rawKey) {
		return nil, live.ErrKeyNotFound
	}

	client := s.entFromContext(ctx)

	item, err := client.APIKey.Query().
		Where(
			apikey.KeyEQ(rawKey),
			apikey.DeletedAtEQ(0),
			apikey.Or(apikey.KeyHashIsNil(), apikey.KeyHashEQ("")),
		).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, live.ErrKeyNotFound
		}

		return nil, err
	}

	repaired, err := client.APIKey.UpdateOneID(item.ID).
		SetKeyHash(keyHash).
		SetKeyPrefix(xapikey.Prefix(rawKey)).
		SetKey(xapikey.Redact(rawKey)).
		Save(ctx)
	if err != nil {
		// The row is still authenticatable in memory; keep serving and let a
		// later lookup (or the startup migration) retry the repair.
		log.Warn(ctx, "failed to backfill legacy api key hash", log.Int("api_key_id", item.ID), log.Cause(err))

		item.KeyHash = keyHash

		return item, nil
	}

	return repaired, nil
}

func (s *APIKeyService) loadAPIKeysSince(ctx context.Context, since time.Time) ([]*ent.APIKey, time.Time, error) {
	ctx = schematype.SkipSoftDelete(ctx)
	client := s.entFromContext(ctx)

	q := client.APIKey.Query()
	if !since.IsZero() {
		// GTE instead of GT: rows updated within the same instant as the last
		// sync watermark must not be skipped. Re-reading boundary rows is safe
		// because cache application is an idempotent upsert.
		q = q.Where(apikey.UpdatedAtGTE(since))
	}

	items, err := q.All(ctx)
	if err != nil {
		return nil, since, err
	}

	maxUpdated := since
	if len(items) > 0 {
		maxUpdated = lo.MaxBy(items, func(a, b *ent.APIKey) bool {
			return a.UpdatedAt.After(b.UpdatedAt)
		}).UpdatedAt
	}

	return items, maxUpdated, nil
}

// GenerateAPIKey generates a new API key with the given prefix.
func GenerateAPIKey(prefix string) (string, error) {
	if strings.TrimSpace(prefix) == "" {
		return "", fmt.Errorf("api key prefix must not be empty")
	}

	// Generate 32 bytes of random data
	bytes := make([]byte, 32)

	_, err := rand.Read(bytes)
	if err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Convert to hex and add prefix
	return prefix + "-" + hex.EncodeToString(bytes), nil
}

// CreateLLMAPIKey creates a new API key for LLM calls using a service account API key.
func (s *APIKeyService) CreateLLMAPIKey(ctx context.Context, owner *ent.APIKey, name string) (*ent.APIKey, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrAPIKeyNameRequired
	}

	generatedKey, err := GenerateAPIKey(s.keyPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to generate api key: %w", err)
	}

	var apiKey *ent.APIKey

	err = s.RunInTransaction(ctx, func(ctx context.Context) error {
		client := s.entFromContext(ctx)

		created, err := client.APIKey.Create().
			SetName(name).
			SetKey(generatedKey).
			SetUserID(owner.UserID).
			SetProjectID(owner.ProjectID).
			SetType(apikey.TypeUser).
			SetScopes([]string{
				string(scopes.ScopeReadChannels),
				string(scopes.ScopeWriteRequests),
			}).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("failed to create api key: %w", err)
		}

		apiKey = created

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Only the hash is persisted; hand the raw key back exactly once so the
	// caller can show it to the user.
	apiKey.Key = generatedKey

	return apiKey, nil
}

// CreateAPIKey creates a new API key for a user.
func (s *APIKeyService) CreateAPIKey(ctx context.Context, input ent.CreateAPIKeyInput) (*ent.APIKey, error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return nil, fmt.Errorf("user not found in context")
	}

	if input.Type != nil && *input.Type == apikey.TypeNoauth {
		return nil, fmt.Errorf("noauth type API key is reserved")
	}

	apiKeyType := apikey.TypeUser
	if currentUser.IsOwner {
		if input.Type != nil {
			apiKeyType = *input.Type
		}
	} else {
		if input.Type != nil && *input.Type != apikey.TypePersonal {
			return nil, fmt.Errorf("non-owner users can only create personal API keys")
		}

		apiKeyType = apikey.TypePersonal
	}

	if apiKeyType == apikey.TypePersonal && input.Scopes != nil {
		return nil, fmt.Errorf("personal API key scopes are managed by the system")
	}

	// Generate API key with configured prefix
	generatedKey, err := GenerateAPIKey(s.keyPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to generate API key: %w", err)
	}

	var apiKey *ent.APIKey

	err = s.RunInTransaction(ctx, func(ctx context.Context) error {
		client := s.entFromContext(ctx)

		create := client.APIKey.Create().
			SetName(input.Name).
			SetKey(generatedKey).
			SetUserID(currentUser.ID).
			SetProjectID(input.ProjectID).
			SetType(apiKeyType)

		switch apiKeyType {
		case apikey.TypePersonal:
			create.SetScopes(personalAPIKeyScopes())
		case apikey.TypeServiceAccount:
			if input.Scopes != nil {
				create.SetScopes(input.Scopes)
			} else {
				create.SetScopes([]string{})
			}
		}

		created, err := create.Save(ctx)
		if err != nil {
			return fmt.Errorf("failed to create API key: %w", err)
		}

		apiKey = created

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Only the hash is persisted; hand the raw key back exactly once so the
	// caller can show it to the user.
	apiKey.Key = generatedKey

	return apiKey, nil
}

// UpdateAPIKey updates an existing API key.
func (s *APIKeyService) UpdateAPIKey(ctx context.Context, id int, input ent.UpdateAPIKeyInput) (*ent.APIKey, error) {
	var result *ent.APIKey

	err := s.RunInTransaction(ctx, func(ctx context.Context) error {
		client := s.entFromContext(ctx)

		apiKey, err := client.APIKey.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("failed to get API key: %w", err)
		}

		if apiKey.Type == apikey.TypeUser || apiKey.Type == apikey.TypePersonal {
			if len(input.Scopes) > 0 || len(input.AppendScopes) > 0 || input.ClearScopes {
				return fmt.Errorf("%s type API key cannot update scopes", apiKey.Type)
			}
		}

		if apiKey.Type == apikey.TypeNoauth {
			return fmt.Errorf("noauth type API key cannot be updated")
		}

		if err := authorizeUserAPIKeyManagement(ctx, apiKey); err != nil {
			return err
		}

		update := client.APIKey.UpdateOneID(id).SetNillableName(input.Name)

		if apiKey.Type == apikey.TypeServiceAccount {
			if len(input.Scopes) > 0 {
				update.SetScopes(input.Scopes)
			}

			if len(input.AppendScopes) > 0 {
				update.AppendScopes(input.AppendScopes)
			}

			if input.ClearScopes {
				update.ClearScopes()
			}
		}

		updated, err := update.Save(ctx)
		if err != nil {
			return fmt.Errorf("failed to update API key: %w", err)
		}

		result = updated

		return nil
	})
	if err != nil {
		return nil, err
	}

	s.invalidateAPIKeyCaches(ctx, result.KeyHash)

	return result, nil
}

// UpdateAPIKeyStatus updates the status of an API key.
func (s *APIKeyService) UpdateAPIKeyStatus(ctx context.Context, id int, status apikey.Status) (*ent.APIKey, error) {
	client := s.entFromContext(ctx)

	existing, err := client.APIKey.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get API key: %w", err)
	}

	if existing.Type == apikey.TypeNoauth {
		return nil, fmt.Errorf("noauth type API key status cannot be updated")
	}

	if err := authorizeUserAPIKeyManagement(ctx, existing); err != nil {
		return nil, err
	}

	apiKey, err := client.APIKey.UpdateOneID(id).
		SetStatus(status).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update API key status: %w", err)
	}

	// Invalidate cache
	s.invalidateAPIKeyCaches(ctx, apiKey.KeyHash)

	return apiKey, nil
}

// UpdateAPIKeyProfiles updates the profiles of an API key.
func (s *APIKeyService) UpdateAPIKeyProfiles(ctx context.Context, id int, profiles objects.APIKeyProfiles) (*ent.APIKey, error) {
	client := s.entFromContext(ctx)

	existing, err := client.APIKey.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get API key: %w", err)
	}

	if existing.Type == apikey.TypeNoauth {
		return nil, fmt.Errorf("noauth type API key profiles cannot be updated")
	}

	if err := authorizeUserAPIKeyManagement(ctx, existing); err != nil {
		return nil, err
	}

	// Validate that profile names are unique (case-insensitive)
	if err := validateProfileNames(profiles.Profiles); err != nil {
		return nil, err
	}

	// Validate that active profile exists in the profiles list
	if err := validateActiveProfile(profiles.ActiveProfile, profiles.Profiles); err != nil {
		return nil, err
	}

	if err := validateProfileFilters(profiles.Profiles); err != nil {
		return nil, err
	}
	if existing.Type == apikey.TypePersonal {
		for _, profile := range profiles.Profiles {
			if len(profile.ChannelIDs) > 0 || len(profile.ChannelTags) > 0 || profile.ChannelTagsMatchMode != "" {
				return nil, fmt.Errorf("personal API key profiles cannot select shared channels or channel tags")
			}
			if profile.LoadBalanceStrategy != nil {
				strategy := strings.TrimSpace(*profile.LoadBalanceStrategy)
				if strategy != "" && strategy != "system_default" {
					return nil, fmt.Errorf("personal API key profiles must use the system load balancing strategy")
				}
			}
		}
	}

	// Validate quota configuration (if present)
	if err := validateProfileQuota(profiles.Profiles); err != nil {
		return nil, err
	}

	apiKey, err := client.APIKey.UpdateOneID(id).
		SetProfiles(&profiles).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update API key profiles: %w", err)
	}

	// Invalidate cache
	s.invalidateAPIKeyCaches(ctx, apiKey.KeyHash)

	return apiKey, nil
}

// validateProfileNames checks that all profile names are unique (case-insensitive).
func validateProfileNames(profiles []objects.APIKeyProfile) error {
	seen := make(map[string]bool)

	for _, profile := range profiles {
		nameLower := strings.ToLower(strings.TrimSpace(profile.Name))
		if nameLower == "" {
			return fmt.Errorf("profile name cannot be empty")
		}

		if seen[nameLower] {
			return fmt.Errorf("duplicate profile name: %s", profile.Name)
		}

		seen[nameLower] = true
	}

	return nil
}

// validateActiveProfile checks that the active profile exists in the profiles list.
func validateActiveProfile(activeProfile string, profiles []objects.APIKeyProfile) error {
	for _, profile := range profiles {
		if profile.Name == activeProfile {
			return nil
		}
	}

	return fmt.Errorf("active profile '%s' does not exist in the profiles list", activeProfile)
}

func validateProfileFilters(profiles []objects.APIKeyProfile) error {
	for _, profile := range profiles {
		if !profile.ChannelTagsMatchMode.IsValid() {
			return fmt.Errorf("profile '%s' channelTagsMatchMode is invalid", profile.Name)
		}
	}

	return nil
}

func validateProfileQuota(profiles []objects.APIKeyProfile) error {
	for _, profile := range profiles {
		if profile.Quota == nil {
			continue
		}

		q := profile.Quota
		if q.Requests == nil && q.TotalTokens == nil && q.Cost == nil {
			return fmt.Errorf("profile '%s' quota must set at least one limit", profile.Name)
		}

		if q.Requests != nil && *q.Requests <= 0 {
			return fmt.Errorf("profile '%s' quota.requests must be positive", profile.Name)
		}

		if q.TotalTokens != nil && *q.TotalTokens <= 0 {
			return fmt.Errorf("profile '%s' quota.totalTokens must be positive", profile.Name)
		}

		if q.Cost != nil && q.Cost.IsNegative() {
			return fmt.Errorf("profile '%s' quota.cost must be non-negative", profile.Name)
		}

		switch q.Period.Type {
		case objects.APIKeyQuotaPeriodTypeAllTime:
		case objects.APIKeyQuotaPeriodTypePastDuration:
			if q.Period.PastDuration == nil {
				return fmt.Errorf("profile '%s' quota.period.pastDuration is required", profile.Name)
			}

			if q.Period.PastDuration.Value <= 0 {
				return fmt.Errorf("profile '%s' quota.period.pastDuration.value must be positive", profile.Name)
			}

			switch q.Period.PastDuration.Unit {
			case objects.APIKeyQuotaPastDurationUnitMinute, objects.APIKeyQuotaPastDurationUnitHour, objects.APIKeyQuotaPastDurationUnitDay:
			default:
				return fmt.Errorf("profile '%s' quota.period.pastDuration.unit is invalid", profile.Name)
			}
		case objects.APIKeyQuotaPeriodTypeCalendarDuration:
			if q.Period.CalendarDuration == nil {
				return fmt.Errorf("profile '%s' quota.period.calendarDuration is required", profile.Name)
			}

			switch q.Period.CalendarDuration.Unit {
			case objects.APIKeyQuotaCalendarDurationUnitDay, objects.APIKeyQuotaCalendarDurationUnitMonth:
			default:
				return fmt.Errorf("profile '%s' quota.period.calendarDuration.unit is invalid", profile.Name)
			}
		default:
			return fmt.Errorf("profile '%s' quota.period.type is invalid", profile.Name)
		}
	}

	return nil
}

type apiKeyCtxKey struct{}

// apiKeyCacheKeyForHash builds the cache key from the stored key hash. The v2
// segment versions the cache identity scheme (see cache-compat rules).
func apiKeyCacheKeyForHash(keyHash string) string {
	return "api_key:v2:" + keyHash
}

// buildAPIKeyCacheKey builds the cache key from a raw (plaintext) API key.
func buildAPIKeyCacheKey(rawKey string) string {
	return apiKeyCacheKeyForHash(xapikey.Hash(rawKey))
}

func apiKeyCacheKeysForHashes(hashes []string) []string {
	cacheKeys := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		cacheKeys = append(cacheKeys, apiKeyCacheKeyForHash(hash))
	}

	return cacheKeys
}

func (s *APIKeyService) GetAPIKey(ctx context.Context, key string) (*ent.APIKey, error) {
	// Add API key to context for cache.
	ctx = context.WithValue(ctx, apiKeyCtxKey{}, key)
	keyHash := xapikey.Hash(key)
	cacheKey := apiKeyCacheKeyForHash(keyHash)

	cached, err := s.APIKeyCache.Get(ctx, cacheKey)

	if err != nil {
		if errors.Is(err, live.ErrKeyNotFound) {
			return nil, fmt.Errorf("%w: failed to get api key: %w", ErrInvalidAPIKey, err)
		}

		return nil, fmt.Errorf("failed to get api key: %w", err)
	}

	// Re-verify the cache hit against the requested key so a stale or
	// colliding cache entry can never authenticate a different key.
	if cached == nil || (*cached).KeyHash != keyHash {
		return nil, fmt.Errorf("%w: cached api key does not match requested key", ErrInvalidAPIKey)
	}

	apiKey := *cached

	// DO NOT CACHE PROJECT
	project, err := s.ProjectService.GetProjectByID(ctx, apiKey.ProjectID)
	if err != nil {
		// Check if it's a "not found" error
		if errors.Is(err, ErrProjectNotFound) {
			return nil, fmt.Errorf("%w: project not found", ErrInvalidAPIKey)
		}
		// Return original error for other cases (database errors, internal errors, etc.)
		return nil, fmt.Errorf("failed to get api key project: %w", err)
	}

	apiKey.Edges.Project = project

	return &apiKey, nil
}

// GetForRead loads an API key by id, key, or name for read-only access. Exactly
// one of id, key, or name must be non-nil.
//
// It deliberately goes through the context-bound ent client (entFromContext) so
// the APIKey privacy policy runs: an API key principal must hold read_api_keys
// and can only see keys inside its own project. Callers in another project — or
// missing the scope — therefore get a NotFound / privacy error, never a foreign
// key. This is the read-side counterpart to the implicit ent gating used by the
// update mutations.
//
// Name lookups made by an API-key principal are additionally scoped to that
// principal's user. Display names are intentionally non-unique; callers that
// created duplicate labels must use the key id or value for an unambiguous
// operation.
func (s *APIKeyService) GetForRead(ctx context.Context, id *int, key *string, name *string) (*ent.APIKey, error) {
	if lo.Count([]bool{id != nil, key != nil, name != nil}, true) != 1 {
		return nil, fmt.Errorf("exactly one of api key id, key, or name must be provided")
	}

	client := s.entFromContext(ctx)
	q := client.APIKey.Query()

	switch {
	case id != nil:
		q = q.Where(apikey.IDEQ(*id))
	case key != nil:
		// Only the hash of the raw key is stored.
		q = q.Where(apikey.KeyHashEQ(xapikey.Hash(*key)))
	case name != nil:
		q = q.Where(apikey.NameEQ(*name))

		// API-key privacy permits a service account to read non-personal keys across
		// its project. Limit name resolution to the principal's creator so a duplicate
		// label can never select another user's key. Contexts without an API-key
		// principal retain the existing privacy-governed project lookup behavior.
		if principal, ok := contexts.GetAPIKey(ctx); ok && principal != nil && principal.UserID != 0 {
			q = q.Where(apikey.UserIDEQ(principal.UserID))
		}
	}

	apiKey, err := q.Only(ctx)
	if err != nil {
		// Display names are deliberately non-unique. A duplicated label cannot
		// identify one key, so surface an actionable error instead of Ent's opaque
		// "not singular".
		if name != nil && ent.IsNotSingular(err) {
			return nil, fmt.Errorf("multiple API keys are named %q for this creator; use id or key to identify the key", *name)
		}

		return nil, err
	}

	return apiKey, nil
}

// invalidateAPIKeyCaches invalidates cache entries by the stored key hash
// (the raw key is generally no longer available after creation).
func (s *APIKeyService) invalidateAPIKeyCaches(ctx context.Context, keyHashes ...string) {
	hashes := lo.Filter(keyHashes, func(hash string, _ int) bool { return hash != "" })
	if len(hashes) == 0 {
		return
	}

	cacheKeys := apiKeyCacheKeysForHashes(hashes)
	if err := s.apiKeyNotifier.Notify(ctx, live.NewInvalidateKeysEvent(cacheKeys...)); err != nil {
		log.Warn(ctx, "api key cache watcher notify failed", log.Cause(err))
	}
}

func (s *APIKeyService) bulkUpdateAPIKeyStatus(ctx context.Context, ids []int, status apikey.Status, action string) error {
	if len(ids) == 0 {
		return nil
	}

	client := s.entFromContext(ctx)

	apiKeys, err := client.APIKey.Query().
		Where(apikey.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("failed to query API keys: %w", err)
	}

	if len(apiKeys) != len(ids) {
		return fmt.Errorf("expected to find %d API keys, but found %d", len(ids), len(apiKeys))
	}

	for _, apiKey := range apiKeys {
		if apiKey.Type == apikey.TypeNoauth {
			return fmt.Errorf("noauth type API key cannot be bulk %sd", action)
		}

		if err := authorizeUserAPIKeyManagement(ctx, apiKey); err != nil {
			return fmt.Errorf("cannot %s API key %q: %w", action, apiKey.Name, err)
		}
	}

	// Update all API keys status
	_, err = client.APIKey.Update().
		Where(apikey.IDIn(ids...)).
		SetStatus(status).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("failed to %s API keys: %w", action, err)
	}

	s.invalidateAPIKeyCaches(ctx, lo.Map(apiKeys, func(apiKey *ent.APIKey, _ int) string { return apiKey.KeyHash })...)
	return nil
}

// BulkDisableAPIKeys disables multiple API keys by their IDs.
func (s *APIKeyService) BulkDisableAPIKeys(ctx context.Context, ids []int) error {
	return s.bulkUpdateAPIKeyStatus(ctx, ids, apikey.StatusDisabled, "disable")
}

// BulkEnableAPIKeys enables multiple API keys by their IDs.
func (s *APIKeyService) BulkEnableAPIKeys(ctx context.Context, ids []int) error {
	return s.bulkUpdateAPIKeyStatus(ctx, ids, apikey.StatusEnabled, "enable")
}

// BulkArchiveAPIKeys archives multiple API keys by their IDs.
func (s *APIKeyService) BulkArchiveAPIKeys(ctx context.Context, ids []int) error {
	return s.bulkUpdateAPIKeyStatus(ctx, ids, apikey.StatusArchived, "archive")
}

// RotateAPIKey rotates an API key by generating a new key value while preserving all other properties.
// This is useful when a key is compromised or when an employee leaves, without losing usage statistics.
func (s *APIKeyService) RotateAPIKey(ctx context.Context, id int) (*ent.APIKey, error) {
	// Use the context-bound client so the APIKey privacy policy applies,
	// consistent with the other mutation paths.
	client := s.entFromContext(ctx)

	existing, err := client.APIKey.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get API key: %w", err)
	}

	// Cannot rotate noauth type API key
	if existing.Type == apikey.TypeNoauth {
		return nil, fmt.Errorf("noauth type API key cannot be rotated")
	}

	if err := authorizeUserAPIKeyManagement(ctx, existing); err != nil {
		return nil, err
	}

	// Generate a new API key
	newKey, err := GenerateAPIKey(s.keyPrefix)
	if err != nil {
		return nil, fmt.Errorf("failed to generate new API key: %w", err)
	}

	oldKeyHash := existing.KeyHash

	// Update the key field directly using Ent
	rotated, err := client.APIKey.UpdateOneID(id).
		SetKey(newKey).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to rotate API key: %w", err)
	}

	// Invalidate caches for both old and new keys
	s.invalidateAPIKeyCaches(ctx, oldKeyHash, rotated.KeyHash)

	// Only the hash is persisted; hand the raw key back exactly once so the
	// caller can show it to the user.
	rotated.Key = newKey

	return rotated, nil
}

func (s *APIKeyService) EnsureNoAuthAPIKey(ctx context.Context) (*ent.APIKey, error) {
	existing, err := s.GetAPIKey(ctx, NoAuthAPIKeyValue)
	if err == nil {
		return existing, nil
	}

	if !errors.Is(err, ErrInvalidAPIKey) {
		return nil, fmt.Errorf("failed to query noauth api key from cache: %w", err)
	}

	client := s.entFromContext(ctx)
	proj, err := client.Project.Query().
		Order(ent.Asc(project.FieldID)).
		First(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get default project: %w", err)
	}

	owner, err := client.User.Query().Where(user.IsOwnerEQ(true)).First(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get owner user for noauth api key: %w", err)
	}

	apiKey, err := client.APIKey.Create().
		SetName(NoAuthAPIKeyName).
		SetKey(NoAuthAPIKeyValue).
		SetUserID(owner.ID).
		SetProjectID(proj.ID).
		SetType(apikey.TypeNoauth).
		SetStatus(apikey.StatusEnabled).
		SetScopes([]string{string(scopes.ScopeWriteRequests), string(scopes.ScopeReadChannels)}).
		Save(ctx)
	if err != nil {
		// Concurrent callers can race past the cache miss and both attempt the
		// create; the unique key_hash index rejects the loser. Re-read the row
		// the winner created instead of failing the request. Drop the local
		// negative cache entry first so the re-read hits the database.
		if ent.IsConstraintError(err) {
			s.APIKeyCache.Invalidate(buildAPIKeyCacheKey(NoAuthAPIKeyValue))

			existing, getErr := s.GetAPIKey(ctx, NoAuthAPIKeyValue)
			if getErr == nil {
				return existing, nil
			}

			return nil, fmt.Errorf("failed to load noauth api key after concurrent create: %w", getErr)
		}

		return nil, fmt.Errorf("failed to create noauth api key: %w", err)
	}

	// DO NOT CACHE PROJECT
	project, err := s.ProjectService.GetProjectByID(ctx, apiKey.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("failed to get api key project: %w", err)
	}

	apiKey.Edges.Project = project

	s.invalidateAPIKeyCaches(ctx, apiKey.KeyHash)

	return apiKey, nil
}
