package biz

import (
	_ "embed"
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
)

//go:embed model_catalog_data.json
var builtInModelCatalogJSON []byte

var builtInModelCatalog = mustLoadBuiltInModelCatalog()
var builtInModelCatalogIndexes = buildBuiltInModelCatalogIndexes(builtInModelCatalog)

type modelCatalogIndexes struct {
	folded map[string]*objects.ModelMetadataPatch
	suffix map[string]*objects.ModelMetadataPatch
}

var developerIcons = map[string]string{
	"alibaba":   "Qwen",
	"anthropic": "Claude",
	"bytedance": "Doubao",
	"deepseek":  "DeepSeek",
	"google":    "Gemini",
	"longcat":   "LongCat",
	"meta":      "Meta",
	"minimax":   "Minimax",
	"mistral":   "Mistral",
	"moonshot":  "Moonshot",
	"nvidia":    "Nvidia",
	"openai":    "OpenAI",
	"stepfun":   "Stepfun",
	"xai":       "XAI",
	"xiaomi":    "XiaomiMiMo",
	"zai":       "ZAI",
	"kwaipilot": "KwaiKAT",
}

type ModelMetadataSource string

const (
	ModelMetadataSourceConfigured ModelMetadataSource = "owner"
	ModelMetadataSourceOverride   ModelMetadataSource = "override"
	ModelMetadataSourceCatalog    ModelMetadataSource = "catalog"
	ModelMetadataSourceDefault    ModelMetadataSource = "default"
	ModelMetadataSourceMixed      ModelMetadataSource = "mixed"
)

func mustLoadBuiltInModelCatalog() map[string]*objects.ModelMetadataPatch {
	var catalog map[string]*objects.ModelMetadataPatch
	if err := json.Unmarshal(builtInModelCatalogJSON, &catalog); err != nil {
		panic("load built-in model catalog: " + err.Error())
	}
	return catalog
}

func buildBuiltInModelCatalogIndexes(catalog map[string]*objects.ModelMetadataPatch) modelCatalogIndexes {
	indexes := modelCatalogIndexes{
		folded: make(map[string]*objects.ModelMetadataPatch, len(catalog)),
		suffix: make(map[string]*objects.ModelMetadataPatch, len(catalog)),
	}
	ambiguousSuffixes := make(map[string]struct{})
	modelIDs := make([]string, 0, len(catalog))
	for modelID := range catalog {
		modelIDs = append(modelIDs, modelID)
	}
	sort.Strings(modelIDs)

	for _, modelID := range modelIDs {
		metadata := catalog[modelID]
		foldedID := strings.ToLower(modelID)
		// Case-only duplicates are exceptionally rare model aliases. Keep a
		// deterministic first match so callers still get case-insensitive exact
		// behavior; case-sensitive exact lookup always wins before this index.
		if _, exists := indexes.folded[foldedID]; !exists {
			indexes.folded[foldedID] = metadata
		}

		suffix := foldedID
		if slash := strings.LastIndexByte(suffix, '/'); slash >= 0 {
			suffix = suffix[slash+1:]
		}
		if suffix == "" {
			continue
		}
		if _, ambiguous := ambiguousSuffixes[suffix]; ambiguous {
			continue
		}
		if _, exists := indexes.suffix[suffix]; exists {
			delete(indexes.suffix, suffix)
			ambiguousSuffixes[suffix] = struct{}{}
			continue
		}
		indexes.suffix[suffix] = metadata
	}

	return indexes
}

func lookupBuiltInModelMetadata(modelID string) *objects.ModelMetadataPatch {
	return lookupModelMetadataFromCatalog(builtInModelCatalog, builtInModelCatalogIndexes, modelID)
}

func lookupModelMetadataFromCatalog(
	catalog map[string]*objects.ModelMetadataPatch,
	indexes modelCatalogIndexes,
	modelID string,
) *objects.ModelMetadataPatch {
	modelID = strings.TrimSpace(modelID)
	if metadata := catalog[modelID]; metadata != nil {
		return cloneModelMetadataPatch(metadata)
	}

	foldedID := strings.ToLower(modelID)
	if metadata := indexes.folded[foldedID]; metadata != nil {
		return cloneModelMetadataPatch(metadata)
	}
	suffix := foldedID
	if slash := strings.LastIndexByte(suffix, '/'); slash >= 0 {
		suffix = suffix[slash+1:]
	}
	if metadata := indexes.suffix[suffix]; metadata != nil {
		return cloneModelMetadataPatch(metadata)
	}

	return nil
}

func cloneModelMetadataPatch(source *objects.ModelMetadataPatch) *objects.ModelMetadataPatch {
	if source == nil {
		return nil
	}

	clone := *source
	clone.Name = clonePointer(source.Name)
	clone.Description = clonePointer(source.Description)
	clone.Developer = clonePointer(source.Developer)
	clone.Type = clonePointer(source.Type)
	clone.Icon = clonePointer(source.Icon)
	clone.Group = clonePointer(source.Group)
	clone.ToolCall = clonePointer(source.ToolCall)
	clone.Temperature = clonePointer(source.Temperature)
	clone.Vision = clonePointer(source.Vision)
	clone.Knowledge = clonePointer(source.Knowledge)
	clone.ReleaseDate = clonePointer(source.ReleaseDate)
	clone.LastUpdated = clonePointer(source.LastUpdated)

	if source.Reasoning != nil {
		clone.Reasoning = &objects.ModelCardReasoningPatch{
			Supported: clonePointer(source.Reasoning.Supported),
			Default:   clonePointer(source.Reasoning.Default),
		}
	}
	if source.Modalities != nil {
		clone.Modalities = &objects.ModelCardModalitiesPatch{
			Input:  cloneStringSlicePointer(source.Modalities.Input),
			Output: cloneStringSlicePointer(source.Modalities.Output),
		}
	}
	if source.Cost != nil {
		clone.Cost = &objects.ModelCardCostPatch{
			Input:      clonePointer(source.Cost.Input),
			Output:     clonePointer(source.Cost.Output),
			CacheRead:  clonePointer(source.Cost.CacheRead),
			CacheWrite: clonePointer(source.Cost.CacheWrite),
		}
	}
	if source.Limit != nil {
		clone.Limit = &objects.ModelCardLimitPatch{
			Context: clonePointer(source.Limit.Context),
			Output:  clonePointer(source.Limit.Output),
		}
	}

	return &clone
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneStringSlicePointer(value *[]string) *[]string {
	if value == nil {
		return nil
	}
	clone := make([]string, len(*value))
	copy(clone, *value)
	return &clone
}

// mergeModelMetadataPatch overlays only fields explicitly present in overlay.
func mergeModelMetadataPatch(base, overlay *objects.ModelMetadataPatch) *objects.ModelMetadataPatch {
	result := cloneModelMetadataPatch(base)
	if result == nil {
		result = &objects.ModelMetadataPatch{}
	}
	if overlay == nil {
		return result
	}

	if overlay.Name != nil {
		result.Name = clonePointer(overlay.Name)
	}
	if overlay.Description != nil {
		result.Description = clonePointer(overlay.Description)
	}
	if overlay.Developer != nil {
		result.Developer = clonePointer(overlay.Developer)
	}
	if overlay.Type != nil {
		result.Type = clonePointer(overlay.Type)
	}
	if overlay.Icon != nil {
		result.Icon = clonePointer(overlay.Icon)
	}
	if overlay.Group != nil {
		result.Group = clonePointer(overlay.Group)
	}
	if overlay.ToolCall != nil {
		result.ToolCall = clonePointer(overlay.ToolCall)
	}
	if overlay.Temperature != nil {
		result.Temperature = clonePointer(overlay.Temperature)
	}
	if overlay.Vision != nil {
		result.Vision = clonePointer(overlay.Vision)
	}
	if overlay.Knowledge != nil {
		result.Knowledge = clonePointer(overlay.Knowledge)
	}
	if overlay.ReleaseDate != nil {
		result.ReleaseDate = clonePointer(overlay.ReleaseDate)
	}
	if overlay.LastUpdated != nil {
		result.LastUpdated = clonePointer(overlay.LastUpdated)
	}

	if overlay.Reasoning != nil {
		if result.Reasoning == nil {
			result.Reasoning = &objects.ModelCardReasoningPatch{}
		}
		if overlay.Reasoning.Supported != nil {
			result.Reasoning.Supported = clonePointer(overlay.Reasoning.Supported)
		}
		if overlay.Reasoning.Default != nil {
			result.Reasoning.Default = clonePointer(overlay.Reasoning.Default)
		}
	}
	if overlay.Modalities != nil {
		if result.Modalities == nil {
			result.Modalities = &objects.ModelCardModalitiesPatch{}
		}
		if overlay.Modalities.Input != nil {
			result.Modalities.Input = cloneStringSlicePointer(overlay.Modalities.Input)
		}
		if overlay.Modalities.Output != nil {
			result.Modalities.Output = cloneStringSlicePointer(overlay.Modalities.Output)
		}
	}
	if overlay.Cost != nil {
		if result.Cost == nil {
			result.Cost = &objects.ModelCardCostPatch{}
		}
		if overlay.Cost.Input != nil {
			result.Cost.Input = clonePointer(overlay.Cost.Input)
		}
		if overlay.Cost.Output != nil {
			result.Cost.Output = clonePointer(overlay.Cost.Output)
		}
		if overlay.Cost.CacheRead != nil {
			result.Cost.CacheRead = clonePointer(overlay.Cost.CacheRead)
		}
		if overlay.Cost.CacheWrite != nil {
			result.Cost.CacheWrite = clonePointer(overlay.Cost.CacheWrite)
		}
	}
	if overlay.Limit != nil {
		if result.Limit == nil {
			result.Limit = &objects.ModelCardLimitPatch{}
		}
		if overlay.Limit.Context != nil {
			result.Limit.Context = clonePointer(overlay.Limit.Context)
		}
		if overlay.Limit.Output != nil {
			result.Limit.Output = clonePointer(overlay.Limit.Output)
		}
	}

	return result
}

func defaultChannelModelMetadata(ch *Channel, modelID string) *objects.ModelMetadataPatch {
	developer := "unknown"
	if ch != nil && ch.Channel != nil {
		developer = ch.Type.String()
	}
	name := modelID
	modelType := "chat"
	group := developer
	vision := true
	toolCall := true
	reasoning := true
	contextLength := 1_000_000
	input := []string{"text", "image"}
	output := []string{"text"}

	metadata := &objects.ModelMetadataPatch{
		Name:      &name,
		Developer: &developer,
		Type:      &modelType,
		Group:     &group,
		Reasoning: &objects.ModelCardReasoningPatch{
			Supported: &reasoning,
			Default:   clonePointer(&reasoning),
		},
		ToolCall: &toolCall,
		Modalities: &objects.ModelCardModalitiesPatch{
			Input:  &input,
			Output: &output,
		},
		Vision: &vision,
		Limit:  &objects.ModelCardLimitPatch{Context: &contextLength},
	}
	return metadata
}

func normalizeResolvedModelMetadata(metadata *objects.ModelMetadataPatch) *objects.ModelMetadataPatch {
	if metadata == nil {
		return nil
	}
	if metadata.Icon == nil && metadata.Developer != nil {
		if icon, ok := developerIcons[*metadata.Developer]; ok {
			metadata.Icon = &icon
		}
	}

	if metadata.Reasoning != nil && metadata.Reasoning.Supported != nil && !*metadata.Reasoning.Supported {
		value := false
		metadata.Reasoning.Default = &value
	}

	if metadata.Vision != nil && metadata.Modalities != nil && metadata.Modalities.Input != nil {
		input := slices.Clone(*metadata.Modalities.Input)
		if *metadata.Vision {
			if !slices.Contains(input, "image") {
				input = append(input, "image")
			}
		} else {
			input = slices.DeleteFunc(input, func(value string) bool { return value == "image" })
		}
		metadata.Modalities.Input = &input
	}

	return metadata
}

func (svc *ModelService) resolveChannelModelMetadata(ch *Channel, entry ChannelModelEntry) (*objects.ModelMetadataPatch, ModelMetadataSource) {
	requestModel := strings.TrimSpace(entry.RequestModel)
	actualModel := strings.TrimSpace(entry.ActualModel)
	if requestModel == "" {
		requestModel = actualModel
	}
	if actualModel == "" {
		actualModel = requestModel
	}

	metadata := defaultChannelModelMetadata(ch, requestModel)
	source := ModelMetadataSourceDefault

	if catalogMetadata := lookupBuiltInModelMetadata(actualModel); catalogMetadata != nil {
		metadata = mergeModelMetadataPatch(metadata, catalogMetadata)
		source = ModelMetadataSourceCatalog
	}
	if requestModel != actualModel {
		if catalogMetadata := lookupBuiltInModelMetadata(requestModel); catalogMetadata != nil {
			metadata = mergeModelMetadataPatch(metadata, catalogMetadata)
			source = ModelMetadataSourceCatalog
		}
	}

	var overrides map[string]*objects.ModelMetadataPatch
	if ch != nil && ch.Channel != nil && ch.Settings != nil {
		overrides = ch.Settings.ModelMetadataOverrides
	}
	if override := overrides[actualModel]; override != nil {
		metadata = mergeModelMetadataPatch(metadata, override)
		source = ModelMetadataSourceOverride
	}
	if requestModel != actualModel {
		if override := overrides[requestModel]; override != nil {
			metadata = mergeModelMetadataPatch(metadata, override)
			source = ModelMetadataSourceOverride
		}
	}

	// Channel metadata must never be interpreted as real billing. Actual usage
	// pricing is maintained separately by ChannelModelPrice.
	metadata.Cost = nil
	return normalizeResolvedModelMetadata(metadata), source
}

// ResolveChannelModelFacade resolves effective metadata for one channel model.
// Priority is request override, actual-model override, request catalog,
// actual-model catalog, then the permissive unknown-model default.
func (svc *ModelService) ResolveChannelModelFacade(ch *Channel, entry ChannelModelEntry) ModelFacade {
	metadata, source := svc.resolveChannelModelMetadata(ch, entry)
	modelID := strings.TrimSpace(entry.RequestModel)
	if modelID == "" {
		modelID = strings.TrimSpace(entry.ActualModel)
	}
	result := ModelFacade{
		ID:             modelID,
		DisplayName:    modelID,
		Metadata:       metadata,
		MetadataSource: source,
	}
	if ch != nil && ch.Channel != nil {
		result.CreatedAt = ch.CreatedAt
		result.Created = ch.CreatedAt.Unix()
		result.OwnedBy = ch.Type.String()
	}

	return result
}

func configuredModelMetadata(m *ent.Model) *objects.ModelMetadataPatch {
	if m == nil {
		return nil
	}

	name := m.Name
	developer := m.Developer
	modelType := m.Type.String()
	icon := m.Icon
	group := m.Group
	metadata := &objects.ModelMetadataPatch{
		Name:      &name,
		Developer: &developer,
		Type:      &modelType,
		Icon:      &icon,
		Group:     &group,
	}
	if m.Remark != nil {
		metadata.Description = clonePointer(m.Remark)
	}
	if m.ModelCard == nil {
		return metadata
	}

	card := m.ModelCard
	reasoningSupported := card.Reasoning.Supported
	reasoningDefault := card.Reasoning.Default
	toolCall := card.ToolCall
	temperature := card.Temperature
	vision := card.Vision
	input := slices.Clone(card.Modalities.Input)
	output := slices.Clone(card.Modalities.Output)
	if input == nil {
		input = []string{}
	}
	if output == nil {
		output = []string{}
	}
	costInput := card.Cost.Input
	costOutput := card.Cost.Output
	costCacheRead := card.Cost.CacheRead
	costCacheWrite := card.Cost.CacheWrite
	contextLength := card.Limit.Context
	maxOutputTokens := card.Limit.Output
	knowledge := card.Knowledge
	releaseDate := card.ReleaseDate
	lastUpdated := card.LastUpdated

	metadata.Reasoning = &objects.ModelCardReasoningPatch{
		Supported: &reasoningSupported,
		Default:   &reasoningDefault,
	}
	metadata.ToolCall = &toolCall
	metadata.Temperature = &temperature
	metadata.Modalities = &objects.ModelCardModalitiesPatch{Input: &input, Output: &output}
	metadata.Vision = &vision
	metadata.Cost = &objects.ModelCardCostPatch{
		Input:      &costInput,
		Output:     &costOutput,
		CacheRead:  &costCacheRead,
		CacheWrite: &costCacheWrite,
	}
	metadata.Limit = &objects.ModelCardLimitPatch{Context: &contextLength, Output: &maxOutputTokens}
	metadata.Knowledge = &knowledge
	metadata.ReleaseDate = &releaseDate
	metadata.LastUpdated = &lastUpdated

	return metadata
}

func conservativeModelMetadata(left, right *objects.ModelMetadataPatch) *objects.ModelMetadataPatch {
	if left == nil || right == nil {
		return nil
	}

	result := &objects.ModelMetadataPatch{
		Name:        conservativeComparablePointer(left.Name, right.Name),
		Description: conservativeComparablePointer(left.Description, right.Description),
		Developer:   conservativeComparablePointer(left.Developer, right.Developer),
		Type:        conservativeComparablePointer(left.Type, right.Type),
		Icon:        conservativeComparablePointer(left.Icon, right.Icon),
		Group:       conservativeComparablePointer(left.Group, right.Group),
		ToolCall:    conservativeBoolPointer(left.ToolCall, right.ToolCall),
		Temperature: conservativeBoolPointer(left.Temperature, right.Temperature),
		Vision:      conservativeBoolPointer(left.Vision, right.Vision),
		Knowledge:   conservativeComparablePointer(left.Knowledge, right.Knowledge),
		ReleaseDate: conservativeComparablePointer(left.ReleaseDate, right.ReleaseDate),
		LastUpdated: conservativeComparablePointer(left.LastUpdated, right.LastUpdated),
	}

	if left.Reasoning != nil && right.Reasoning != nil {
		result.Reasoning = &objects.ModelCardReasoningPatch{
			Supported: conservativeBoolPointer(left.Reasoning.Supported, right.Reasoning.Supported),
			Default:   conservativeBoolPointer(left.Reasoning.Default, right.Reasoning.Default),
		}
		if result.Reasoning.Supported == nil && result.Reasoning.Default == nil {
			result.Reasoning = nil
		}
	}
	if left.Modalities != nil && right.Modalities != nil {
		result.Modalities = &objects.ModelCardModalitiesPatch{
			Input:  intersectStringSlicePointers(left.Modalities.Input, right.Modalities.Input),
			Output: intersectStringSlicePointers(left.Modalities.Output, right.Modalities.Output),
		}
		if result.Modalities.Input == nil && result.Modalities.Output == nil {
			result.Modalities = nil
		}
	}
	if left.Limit != nil && right.Limit != nil {
		result.Limit = &objects.ModelCardLimitPatch{
			Context: conservativePositiveIntPointer(left.Limit.Context, right.Limit.Context),
			Output:  conservativePositiveIntPointer(left.Limit.Output, right.Limit.Output),
		}
		if result.Limit.Context == nil && result.Limit.Output == nil {
			result.Limit = nil
		}
	}

	// Channel-level cards intentionally never aggregate or expose cost.
	result.Cost = nil
	return normalizeResolvedModelMetadata(result)
}

func conservativeComparablePointer[T comparable](left, right *T) *T {
	if left == nil || right == nil || *left != *right {
		return nil
	}
	return clonePointer(left)
}

func conservativeBoolPointer(left, right *bool) *bool {
	if left == nil || right == nil {
		return nil
	}
	value := *left && *right
	return &value
}

func conservativePositiveIntPointer(left, right *int) *int {
	if left == nil || right == nil || *left <= 0 || *right <= 0 {
		return nil
	}
	value := min(*left, *right)
	return &value
}

func intersectStringSlicePointers(left, right *[]string) *[]string {
	if left == nil || right == nil {
		return nil
	}
	rightSet := make(map[string]struct{}, len(*right))
	for _, value := range *right {
		rightSet[value] = struct{}{}
	}
	result := make([]string, 0, min(len(*left), len(*right)))
	seen := make(map[string]struct{}, len(*left))
	for _, value := range *left {
		if _, ok := rightSet[value]; !ok {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return &result
}

func mergeChannelModelFacades(left, right ModelFacade) ModelFacade {
	result := left
	result.Metadata = conservativeModelMetadata(left.Metadata, right.Metadata)
	if left.OwnedBy != right.OwnedBy {
		result.OwnedBy = "multiple"
	}
	if result.CreatedAt.IsZero() || (!right.CreatedAt.IsZero() && right.CreatedAt.Before(result.CreatedAt)) {
		result.CreatedAt = right.CreatedAt
		result.Created = right.Created
	}
	if left.MetadataSource != right.MetadataSource {
		result.MetadataSource = ModelMetadataSourceMixed
	}
	result.DisplayName = result.ID
	return result
}
