package objects

type ModelCardReasoning struct {
	Supported bool `json:"supported"`
	Default   bool `json:"default"`
}

type ModelCardModalities struct {
	// "text","image","video"
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type ModelCardCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

type ModelCardLimit struct {
	Context int `json:"context"`
	Output  int `json:"output"`
}

type ModelCard struct {
	Reasoning   ModelCardReasoning  `json:"reasoning"`
	ToolCall    bool                `json:"toolCall"`
	Temperature bool                `json:"temperature"`
	Modalities  ModelCardModalities `json:"modalities"`
	Vision      bool                `json:"vision"`
	Cost        ModelCardCost       `json:"cost"`
	Limit       ModelCardLimit      `json:"limit"`
	Knowledge   string              `json:"knowledge"`
	ReleaseDate string              `json:"releaseDate"`
	LastUpdated string              `json:"lastUpdated"`
}

// ModelMetadataPatch represents partial model metadata. Pointer scalar fields
// deliberately distinguish an explicit false/zero value from an unknown value.
// It is used for per-channel metadata overrides and for the built-in provider
// catalog; it is not a routing configuration.
type ModelMetadataPatch struct {
	Name        *string                   `json:"name,omitempty"`
	Description *string                   `json:"description,omitempty"`
	Developer   *string                   `json:"developer,omitempty"`
	Type        *string                   `json:"type,omitempty"`
	Icon        *string                   `json:"icon,omitempty"`
	Group       *string                   `json:"group,omitempty"`
	Reasoning   *ModelCardReasoningPatch  `json:"reasoning,omitempty"`
	ToolCall    *bool                     `json:"toolCall,omitempty"`
	Temperature *bool                     `json:"temperature,omitempty"`
	Modalities  *ModelCardModalitiesPatch `json:"modalities,omitempty"`
	Vision      *bool                     `json:"vision,omitempty"`
	Cost        *ModelCardCostPatch       `json:"cost,omitempty"`
	Limit       *ModelCardLimitPatch      `json:"limit,omitempty"`
	Knowledge   *string                   `json:"knowledge,omitempty"`
	ReleaseDate *string                   `json:"releaseDate,omitempty"`
	LastUpdated *string                   `json:"lastUpdated,omitempty"`
}

type ModelCardReasoningPatch struct {
	Supported *bool `json:"supported,omitempty"`
	Default   *bool `json:"default,omitempty"`
}

type ModelCardModalitiesPatch struct {
	// A nil pointer means unknown; a non-nil empty slice means explicitly none.
	Input  *[]string `json:"input,omitempty"`
	Output *[]string `json:"output,omitempty"`
}

type ModelCardCostPatch struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cacheRead,omitempty"`
	CacheWrite *float64 `json:"cacheWrite,omitempty"`
}

type ModelCardLimitPatch struct {
	Context *int `json:"context,omitempty"`
	Output  *int `json:"output,omitempty"`
}

type ModelSettings struct {
	DisableDeveloperSettingsInheritance bool                `json:"disableDeveloperSettingsInheritance"`
	Associations                        []*ModelAssociation `json:"associations"`
}

const (
	ModelAssociationConditionFieldPromptTokens        = "prompt_tokens"
	ModelAssociationConditionFieldStream              = "stream"
	ModelAssociationConditionFieldRequestFormat       = "request_format"
	ModelAssociationConditionFieldDailyTime           = "daily_time"
	ModelAssociationConditionFieldHasImage            = "has_image"
	ModelAssociationConditionFieldHasVideo            = "has_video"
	ModelAssociationConditionFieldHasDocument         = "has_document"
	ModelAssociationConditionFieldHasAudio            = "has_audio"
	ModelAssociationConditionFieldRequestHeader       = "request_header"
	ModelAssociationConditionFieldRequestHeaderPrefix = "request_header."
)

type ModelAssociation struct {
	// channel_model: the specified model id in the specified channel
	// channel_regex: the specified pattern in the specified channel
	// regex: the pattern for all channels
	// model: the specified model id
	// channel_tags_model: the specified model id in channels with specified tags (OR logic)
	// channel_tags_regex: the specified pattern in channels with specified tags (OR logic)
	Type             string                       `json:"type"`
	Priority         int                          `json:"priority"` // Lower value = higher priority, default 0
	Disabled         bool                         `json:"disabled"`
	When             *ModelAssociationWhen        `json:"when,omitempty"`
	ChannelModel     *ChannelModelAssociation     `json:"channelModel"`
	ChannelRegex     *ChannelRegexAssociation     `json:"channelRegex"`
	Regex            *RegexAssociation            `json:"regex"`
	ModelID          *ModelIDAssociation          `json:"modelId"`
	ChannelTagsModel *ChannelTagsModelAssociation `json:"channelTagsModel"`
	ChannelTagsRegex *ChannelTagsRegexAssociation `json:"channelTagsRegex"`
}

type ModelAssociationWhen struct {
	Enabled   bool       `json:"enabled"`
	Condition *Condition `json:"condition,omitempty"`
}

type ExcludeAssociation struct {
	ChannelNamePattern string   `json:"channelNamePattern"`
	ChannelIds         []int    `json:"channelIds"`
	ChannelTags        []string `json:"channelTags"`
}

type ChannelModelAssociation struct {
	ChannelID int    `json:"channelId"`
	ModelID   string `json:"modelId"`
}

type ChannelRegexAssociation struct {
	ChannelID int    `json:"channelId"`
	Pattern   string `json:"pattern"`
}

type RegexAssociation struct {
	Pattern string                `json:"pattern"`
	Exclude []*ExcludeAssociation `json:"exclude"`
}

type ModelIDAssociation struct {
	ModelID string                `json:"modelId"`
	Exclude []*ExcludeAssociation `json:"exclude"`
}

type ChannelTagsModelAssociation struct {
	ChannelTags []string `json:"channelTags"`
	ModelID     string   `json:"modelId"`
}

type ChannelTagsRegexAssociation struct {
	ChannelTags []string `json:"channelTags"`
	Pattern     string   `json:"pattern"`
}
