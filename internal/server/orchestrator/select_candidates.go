package orchestrator

import (
	"context"
	"fmt"
	"sort"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
)

// selectCandidates creates a middleware that selects available channel model candidates for the model.
// This is the second step in the inbound pipeline, moved from outbound transformer.
// If no valid candidates are found, it returns ErrInvalidModel to fail fast.
func selectCandidates(inbound *PersistentInboundTransformer) pipeline.Middleware {
	return pipeline.OnLlmRequest("select-candidates", func(ctx context.Context, llmRequest *llm.Request) (*llm.Request, error) {
		// Only select candidates once
		if len(inbound.state.ChannelModelsCandidates) > 0 {
			return llmRequest, nil
		}

		selector := inbound.state.CandidateSelector

		// Explicit trace/session IDs remain authoritative. Mark an already-bound
		// trace as sticky so its subsequent turns do not advance the fair
		// new-session counter. Only clients without a trace receive the
		// privacy-preserving context-prefix fallback.
		if trace, hasTrace := contexts.GetTrace(ctx); hasTrace && inbound.state.RequestService != nil {
			if preferredChannelID, err := inbound.state.RequestService.GetLastSuccessfulChannelID(ctx, trace.ID); err == nil && preferredChannelID > 0 {
				ctx = contextWithSessionAffinityChannel(ctx, preferredChannelID)
			}
		} else if inbound.state.SessionAffinity != nil {
			if inbound.state.SessionAffinityKey == "" {
				if key, ok := inbound.state.SessionAffinity.RequestKey(ctx, llmRequest, inbound.state.APIKey); ok {
					inbound.state.SessionAffinityKey = key
				}
			}
			if preferredChannelID, ok := inbound.state.SessionAffinity.Lookup(inbound.state.SessionAffinityKey); ok {
				ctx = contextWithSessionAffinityChannel(ctx, preferredChannelID)
			}
		}

		// Project-level profile filtering (upper boundary)
		if inbound.state.APIKey != nil {
			if project := inbound.state.APIKey.Edges.Project; project != nil {
				if projectProfile := project.GetActiveProfile(); projectProfile != nil {
					if len(projectProfile.ChannelIDs) > 0 {
						selector = WithSelectedChannelsSelector(selector, projectProfile.ChannelIDs)
					}

					if len(projectProfile.ChannelTags) > 0 {
						selector = WithChannelTagsFilterSelector(selector, projectProfile.ChannelTags, projectProfile.ChannelTagsMatchMode)
					}
				}
			}
		}

		// Key-level channel routing is an administrative feature. Campus
		// personal keys may still set their own quota/model profile, but cannot
		// pin traffic to one contributor or bypass the fair global rotation.
		if inbound.state.APIKey != nil && inbound.state.APIKey.Type != apikey.TypePersonal {
			if profile := inbound.state.APIKey.GetActiveProfile(); profile != nil {
				if len(profile.ChannelIDs) > 0 {
					selector = WithSelectedChannelsSelector(selector, profile.ChannelIDs)
				}

				if len(profile.ChannelTags) > 0 {
					selector = WithChannelTagsFilterSelector(selector, profile.ChannelTags, profile.ChannelTagsMatchMode)
				}
			}
		}

		// Apply Google native tools filter (only for Gemini native API format)
		if llmRequest.APIFormat == llm.APIFormatGeminiContents {
			selector = WithGoogleNativeToolsSelector(selector)
		}

		// Apply Anthropic native tools filter (only for Anthropic message API format)
		if llmRequest.APIFormat == llm.APIFormatAnthropicMessage {
			selector = WithAnthropicNativeToolsSelector(selector)
		}

		selector = WithStreamPolicySelector(selector)

		candidates, err := selector.Select(ctx, llmRequest)
		if err != nil {
			return nil, err
		}

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "selected candidates",
				log.Int("candidate_count", len(candidates)),
				log.String("model", llmRequest.Model),
				log.Any("candidates", lo.Map(candidates, func(candidate *ChannelModelsCandidate, _ int) map[string]any {
					return map[string]any{
						"channel_name": candidate.Channel.Name,
						"channel_id":   candidate.Channel.ID,
						"priority":     candidate.Priority,
						"models": lo.Map(candidate.Models, func(entry biz.ChannelModelEntry, _ int) map[string]any {
							return map[string]any{
								"request_model": entry.RequestModel,
								"actual_model":  entry.ActualModel,
								"source":        entry.Source,
							}
						}),
					}
				})),
			)
		}

		if len(candidates) == 0 {
			return nil, fmt.Errorf("%w: %s", biz.ErrInvalidModel, llmRequest.Model)
		}

		// Production selection has one ordering authority: a stable, unweighted
		// per-model channel ring. Availability is deliberately ignored here and
		// is consulted only after this primary route fails.
		inbound.state.ChannelModelsCandidates = orderUnifiedCandidates(ctx, inbound.state, llmRequest.Model, candidates)

		return llmRequest, nil
	})
}

func orderUnifiedCandidates(
	ctx context.Context,
	state *PersistenceState,
	model string,
	candidates []*ChannelModelsCandidate,
) []*ChannelModelsCandidate {
	if state != nil && state.UnifiedRoutes == nil && state.ChannelService != nil {
		state.UnifiedRoutes = state.ChannelService.UnifiedRouteState()
	}
	if len(candidates) < 2 || state == nil || state.UnifiedRoutes == nil {
		return candidates
	}

	// One channel gets one outer-ring slot even when associations or credentials
	// produce duplicate candidates. Preserve all distinct model mappings inside
	// that channel's slot.
	byChannel := make(map[int]*ChannelModelsCandidate, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.Channel == nil {
			continue
		}
		if existing := byChannel[candidate.Channel.ID]; existing != nil {
			existing.Models = appendUniqueModelEntries(existing.Models, candidate.Models)
			continue
		}
		clone := *candidate
		clone.Models = append([]biz.ChannelModelEntry(nil), candidate.Models...)
		byChannel[candidate.Channel.ID] = &clone
	}

	ordered := make([]*ChannelModelsCandidate, 0, len(byChannel))
	for _, candidate := range byChannel {
		ordered = append(ordered, candidate)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Channel.ID < ordered[j].Channel.ID })
	if len(ordered) < 2 {
		return ordered
	}

	primaryID := sessionAffinityChannelFromContext(ctx)
	if primaryID != 0 {
		found := false
		for _, candidate := range ordered {
			if candidate.Channel.ID == primaryID {
				found = true
				break
			}
		}
		if !found {
			// A deleted/filtered affinity target must not silently bias the first
			// sorted channel, and it must not freeze the fair cursor.
			primaryID = 0
		}
	}
	if primaryID == 0 {
		ids := lo.Map(ordered, func(candidate *ChannelModelsCandidate, _ int) int { return candidate.Channel.ID })
		primaryID, _ = state.UnifiedRoutes.NextPrimary(model, ids)
	}

	primaryIndex := -1
	for index, candidate := range ordered {
		if candidate.Channel.ID == primaryID {
			primaryIndex = index
			break
		}
	}
	if primaryIndex <= 0 {
		return ordered
	}

	rotated := make([]*ChannelModelsCandidate, 0, len(ordered))
	rotated = append(rotated, ordered[primaryIndex:]...)
	rotated = append(rotated, ordered[:primaryIndex]...)
	return rotated
}

func appendUniqueModelEntries(dst, src []biz.ChannelModelEntry) []biz.ChannelModelEntry {
	seen := make(map[string]struct{}, len(dst)+len(src))
	for _, entry := range dst {
		seen[entry.RequestModel+"\x00"+entry.ActualModel] = struct{}{}
	}
	for _, entry := range src {
		key := entry.RequestModel + "\x00" + entry.ActualModel
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		dst = append(dst, entry)
	}
	return dst
}
