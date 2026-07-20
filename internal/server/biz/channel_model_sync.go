package biz

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xregexp"
)

const (
	immediateChannelModelSyncTimeout     = 5 * time.Second
	immediateChannelModelSyncConcurrency = 8
)

// syncChannelModels syncs supported models for all channels with auto_sync_supported_models enabled.
// This function is called periodically (every hour) to keep model lists up to date.
func (svc *ChannelService) syncChannelModels(ctx context.Context) {
	// Query all enabled channels with auto_sync_supported_models = true
	channels, err := svc.entFromContext(ctx).Channel.
		Query().
		Where(
			channel.StatusEQ(channel.StatusEnabled),
			channel.AutoSyncSupportedModelsEQ(true),
			channelAvailableAtPredicate(time.Now()),
		).
		All(ctx)
	if err != nil {
		log.Error(ctx, "failed to query channels for model sync", log.Cause(err))
		return
	}

	if len(channels) == 0 {
		log.Debug(ctx, "no channels with auto_sync_supported_models enabled")
		return
	}

	log.Info(ctx, "starting model sync for channels", log.Int("count", len(channels)))

	successCount := 0
	failureCount := 0

	for _, ch := range channels {
		if _, err := svc.syncChannelModelsForChannel(ctx, ch, nil); err != nil {
			log.Warn(ctx, "failed to sync models for channel",
				log.Int("channel_id", ch.ID),
				log.String("channel_name", ch.Name),
				log.Cause(err))

			failureCount++
		} else {
			successCount++
		}
	}

	if successCount > 0 {
		// Model changes affect both the public model catalog and routing
		// candidates. Notify the enabled-channel cache immediately instead of
		// waiting for its periodic refresh.
		svc.asyncReloadChannels()
	}

	log.Info(ctx, "completed model sync for channels",
		log.Int("success", successCount),
		log.Int("failure", failureCount))
}

// syncChannelModelsForChannel syncs supported models for a single channel.
func (svc *ChannelService) syncChannelModelsForChannel(ctx context.Context, ch *ent.Channel, patternOverride *string) (*ent.Channel, error) {
	if err := rejectExpiredChannel(ch, time.Now()); err != nil {
		return nil, err
	}

	modelFetcher := NewModelFetcher(svc.httpClient, svc)

	result, err := modelFetcher.FetchModels(ctx, FetchModelsInput{
		ChannelType: ch.Type.String(),
		BaseURL:     ch.BaseURL,
		ChannelID:   lo.ToPtr(ch.ID),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch models: %w", err)
	}

	// Check if there was an error in the result
	if result.Error != nil {
		return nil, fmt.Errorf("model fetch returned error: %s", *result.Error)
	}

	// Extract model IDs from fetched models
	fetchedModelIDs := lo.Map(result.Models, func(m ModelIdentify, _ int) string {
		return m.ID
	})

	pattern := ch.AutoSyncModelPattern
	if patternOverride != nil {
		pattern = *patternOverride
	}

	// Filter by auto_sync_model_pattern if set
	if pattern != "" {
		if err := xregexp.ValidateRegex(pattern); err != nil {
			log.Warn(ctx, "invalid auto_sync_model_pattern, skipping filter",
				log.Int("channel_id", ch.ID),
				log.String("pattern", pattern),
				log.Cause(err))
		} else {
			before := len(fetchedModelIDs)
			fetchedModelIDs = xregexp.Filter(fetchedModelIDs, pattern)
			log.Info(ctx, "filtered models by pattern",
				log.Int("channel_id", ch.ID),
				log.String("pattern", pattern),
				log.Int("before", before),
				log.Int("after", len(fetchedModelIDs)))
		}
	}

	// Read existing manual models from the channel
	manualModels := ch.ManualModels
	if manualModels == nil {
		manualModels = []string{}
	}
	// Volcengine does not expose a usable catalog through this integration.
	// Its empty result means "discovery unsupported", not "all models were
	// removed", so keep an explicitly submitted list intact.
	if ch.Type == channel.TypeVolcengine && len(fetchedModelIDs) == 0 && len(manualModels) == 0 {
		return ch, nil
	}

	// Merge fetched models with manual models, removing duplicates
	mergedModels := lo.Uniq(append(manualModels, fetchedModelIDs...))

	// Update channel's supported models with merged list
	// Keep manual_models unchanged (preserve user's manually added models)
	update := svc.entFromContext(ctx).Channel.
		UpdateOneID(ch.ID).
		SetSupportedModels(mergedModels)
	if !lo.Contains(mergedModels, ch.DefaultTestModel) {
		defaultTestModel := ""
		if len(mergedModels) > 0 {
			defaultTestModel = mergedModels[0]
		}
		update.SetDefaultTestModel(defaultTestModel)
	}

	updatedCh, err := update.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to update channel supported models: %w", err)
	}

	log.Info(ctx, "successfully synced models for channel",
		log.Int("channel_id", ch.ID),
		log.String("channel_name", ch.Name),
		log.Int("fetched_count", len(fetchedModelIDs)),
		log.Int("manual_count", len(manualModels)),
		log.Int("total_count", len(mergedModels)))

	return updatedCh, nil
}

// syncChannelModelsBestEffort performs an immediate catalog refresh after a
// channel is created, updated, duplicated, or enabled. Providers that do not
// expose a model-list endpoint must not make an otherwise valid custom/Coding
// Plan channel impossible, so failures leave the submitted model list intact
// and are retried by an explicit or scheduled sync.
func (svc *ChannelService) syncChannelModelsBestEffort(ctx context.Context, ch *ent.Channel) *ent.Channel {
	if ch == nil || !ch.AutoSyncSupportedModels || channelExpiredAt(ch, time.Now()) {
		return ch
	}

	syncCtx, cancel := context.WithTimeout(ctx, immediateChannelModelSyncTimeout)
	defer cancel()
	updated, err := svc.syncChannelModelsForChannel(syncCtx, ch, nil)
	if err != nil {
		log.Warn(ctx, "immediate channel model sync failed; preserving submitted models",
			log.Int("channel_id", ch.ID),
			log.String("channel_name", ch.Name),
			log.Cause(err))

		return ch
	}

	return updated
}

// channelModelDiscoveryConfigChanged reports whether an update changed data
// used by model discovery. Comparing the saved entities avoids turning a
// no-op form submission into a provider request.
func channelModelDiscoveryConfigChanged(before, after *ent.Channel) bool {
	if before == nil || after == nil || !after.AutoSyncSupportedModels {
		return false
	}
	if !before.AutoSyncSupportedModels {
		return true
	}

	return before.Type != after.Type ||
		before.BaseURL != after.BaseURL ||
		!reflect.DeepEqual(before.Credentials, after.Credentials) ||
		!slices.Equal(before.SupportedModels, after.SupportedModels) ||
		!slices.Equal(before.ManualModels, after.ManualModels) ||
		before.AutoSyncModelPattern != after.AutoSyncModelPattern ||
		!reflect.DeepEqual(before.Endpoints, after.Endpoints) ||
		!reflect.DeepEqual(before.Settings, after.Settings)
}

// syncChannelsBestEffort bounds both total latency and outbound concurrency for
// bulk mutations. Each channel retains the same best-effort failure semantics
// as the single-channel helper, while independent catalog endpoints are fetched
// in parallel instead of accumulating timeout delays row by row.
func (svc *ChannelService) syncChannelsBestEffort(ctx context.Context, channels []*ent.Channel) []*ent.Channel {
	result := make([]*ent.Channel, len(channels))
	copy(result, channels)
	if len(result) == 0 {
		return result
	}

	batchCtx, cancel := context.WithTimeout(ctx, immediateChannelModelSyncTimeout)
	defer cancel()

	workerCount := min(len(result), immediateChannelModelSyncConcurrency)
	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				result[index] = svc.syncChannelModelsBestEffort(batchCtx, result[index])
			}
		}()
	}
	for index := range result {
		jobs <- index
	}
	close(jobs)
	workers.Wait()

	return result
}

func (svc *ChannelService) SyncChannelModels(ctx context.Context, channelID int, patternOverride *string) (*ent.Channel, error) {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}

	updated, err := svc.syncChannelModelsForChannel(ctx, ch, patternOverride)
	if err != nil {
		return nil, err
	}

	svc.asyncReloadChannels()

	return updated, nil
}
