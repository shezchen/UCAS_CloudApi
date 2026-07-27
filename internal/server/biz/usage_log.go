package biz

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
)

// UsageLogService handles usage log operations.
type UsageLogService struct {
	*AbstractService

	SystemService  *SystemService
	ChannelService *ChannelService
	WalletService  *TokenWalletService

	// OnUsageLogCreated is called after a usage log is successfully created.
	// Used to invalidate caches that depend on usage log data.
	OnUsageLogCreated func()
}

func (s *UsageLogService) computeUsageCost(ctx context.Context, channelID int, modelID string, usage *llm.Usage) ([]objects.CostItem, *float64, string) {
	if usage == nil {
		return nil, nil, ""
	}

	ch := s.ChannelService.GetEnabledChannel(channelID)
	if ch == nil {
		log.Warn(ctx, "channel not enabled for cost calculation",
			log.Int("channel_id", channelID),
			log.String("model_id", modelID),
		)

		return nil, nil, ""
	}

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "checking cached model price",
			log.Int("channel_id", channelID),
			log.String("model_id", modelID),
			log.Int("cached_price_count", len(ch.cachedModelPrices)),
		)
	}

	if modelPrice, ok := ch.cachedModelPrices[modelID]; ok {
		items, total := ComputeUsageCost(usage, modelPrice.Price)

		totalCost := total.InexactFloat64()
		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "computed usage cost from cache",
				log.Int("channel_id", channelID),
				log.String("model_id", modelID),
				log.Float64("total_cost", totalCost),
				log.Int64("total_tokens", usage.TotalTokens),
				log.String("price_reference_id", modelPrice.ReferenceID),
			)
		}

		return items, lo.ToPtr(totalCost), modelPrice.ReferenceID
	}

	return nil, nil, ""
}

// NewUsageLogService creates a new UsageLogService.
func NewUsageLogService(ent *ent.Client, systemService *SystemService, channelService *ChannelService) *UsageLogService {
	return &UsageLogService{
		AbstractService: &AbstractService{
			db: ent,
		},
		SystemService:  systemService,
		ChannelService: channelService,
		WalletService:  NewTokenWalletService(ent),
	}
}

// CreateUsageLogParams represents the parameters for creating a usage log.
type CreateUsageLogParams struct {
	RequestID     int
	ProjectID     int
	ChannelID     int
	ActualModelID string // The channel actual model ID, not the request model ID.
	Usage         *llm.Usage
	Source        usagelog.Source
	Format        string
	APIKeyID      *int
}

// CreateUsageLog creates a new usage log record from LLM response usage data.
func (s *UsageLogService) CreateUsageLog(ctx context.Context, params CreateUsageLogParams) (*ent.UsageLog, error) {
	if params.Usage == nil {
		return nil, nil // No usage data to log
	}

	totalTokens := max(params.Usage.TotalTokens, int64(0))
	effectiveTokens, cacheReadTokensKnown := EffectiveTokens(params.Usage)

	apiKeyID := params.APIKeyID
	if apiKeyID == nil {
		if ctxAPIKey, ok := contexts.GetAPIKey(ctx); ok && ctxAPIKey != nil {
			apiKeyID = lo.ToPtr(ctxAPIKey.ID)
		}
	}

	// Calculate cost if price is configured
	var (
		totalCost        *float64
		costItems        []objects.CostItem
		priceReferenceID string
	)

	costItems, totalCost, priceReferenceID = s.computeUsageCost(ctx, params.ChannelID, params.ActualModelID, params.Usage)

	// Initialize/read the cutover before timestamping this usage. On a fresh
	// deployment several first requests can arrive concurrently; timestamping
	// first lets a later goroutine win the insert with a later cutover and
	// incorrectly classify the earlier in-flight request as historical.
	walletStartedAt, err := s.WalletService.EnsureStartedAt(ctx, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	usageCreatedAt := time.Now().UTC()

	const maxWalletSettlementAttempts = 8

	var usageLog *ent.UsageLog

	for attempt := 0; attempt < maxWalletSettlementAttempts; attempt++ {
		err = s.RunInTransaction(ctx, func(txCtx context.Context) error {
			// Wallet settlement is trusted internal accounting. The caller's
			// personal API-key scope intentionally cannot read another donor's
			// channel/user/wallet records, so use a narrow system bypass while
			// retaining the same transaction-bound Ent client.
			settlementCtx := authz.WithSystemBypass(txCtx, "usage-log-wallet-settlement")
			client := s.entFromContext(settlementCtx)

			existing, queryErr := client.UsageLog.Query().
				Where(usagelog.RequestIDEQ(params.RequestID)).
				First(settlementCtx)
			if queryErr == nil {
				usageLog = existing
				return nil
			}
			if !ent.IsNotFound(queryErr) {
				return fmt.Errorf("check existing usage log: %w", queryErr)
			}

			settlement, prepareErr := s.WalletService.prepareSettlement(
				settlementCtx,
				apiKeyID,
				params.ChannelID,
				effectiveTokens,
				params.Source == usagelog.SourceTest,
				usageCreatedAt,
				walletStartedAt,
			)
			if prepareErr != nil {
				return prepareErr
			}

			mut := client.UsageLog.Create().
				SetCreatedAt(usageCreatedAt).
				SetRequestID(params.RequestID).
				SetProjectID(params.ProjectID).
				SetModelID(params.ActualModelID).
				SetChannelID(params.ChannelID).
				SetPromptTokens(params.Usage.PromptTokens).
				SetCompletionTokens(params.Usage.CompletionTokens).
				SetTotalTokens(totalTokens).
				SetEffectiveTokens(effectiveTokens).
				SetCacheReadTokensKnown(cacheReadTokensKnown).
				SetWalletConsumedTokens(settlement.WalletDebit).
				SetDonorCreditTokens(settlement.DonorCredit).
				SetSource(params.Source).
				SetFormat(params.Format)

			if apiKeyID != nil {
				mut = mut.SetAPIKeyID(*apiKeyID)
			}

			// Set prompt tokens details if available
			if params.Usage.PromptTokensDetails != nil {
				mut = mut.
					SetPromptAudioTokens(params.Usage.PromptTokensDetails.AudioTokens).
					SetPromptCachedTokens(params.Usage.PromptTokensDetails.CachedTokens).
					SetPromptWriteCachedTokens(params.Usage.PromptTokensDetails.WriteCachedTokens).
					SetPromptWriteCachedTokens5m(params.Usage.PromptTokensDetails.WriteCached5MinTokens).
					SetPromptWriteCachedTokens1h(params.Usage.PromptTokensDetails.WriteCached1HourTokens)
			}

			// Set completion tokens details if available
			if params.Usage.CompletionTokensDetails != nil {
				mut = mut.
					SetCompletionAudioTokens(params.Usage.CompletionTokensDetails.AudioTokens).
					SetCompletionReasoningTokens(params.Usage.CompletionTokensDetails.ReasoningTokens).
					SetCompletionAcceptedPredictionTokens(params.Usage.CompletionTokensDetails.AcceptedPredictionTokens).
					SetCompletionRejectedPredictionTokens(params.Usage.CompletionTokensDetails.RejectedPredictionTokens)
			}

			mut = mut.
				SetNillableTotalCost(totalCost).
				SetCostItems(costItems)

			if priceReferenceID != "" {
				mut = mut.SetCostPriceReferenceID(priceReferenceID)
			}

			usageLog, prepareErr = mut.Save(settlementCtx)
			if prepareErr != nil {
				return fmt.Errorf("create usage log: %w", prepareErr)
			}

			if prepareErr := s.WalletService.applySettlement(settlementCtx, params.RequestID, usageLog.ID, settlement); prepareErr != nil {
				return prepareErr
			}

			return nil
		})
		if !isRetryableTokenWalletSettlementError(err) {
			break
		}
		if attempt+1 < maxWalletSettlementAttempts {
			retryDelay := time.Duration(attempt+1) * 10 * time.Millisecond
			timer := time.NewTimer(retryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	if err != nil {
		// A concurrent call for the same request may have committed while this
		// transaction was waiting or retrying. Returning that atomic result is
		// the idempotent success path; wallet mutations cannot exist without
		// the usage log because they share the transaction.
		lookupCtx := authz.WithSystemBypass(ctx, "usage-log-wallet-idempotency")
		existing, lookupErr := s.entFromContext(lookupCtx).UsageLog.Query().
			Where(usagelog.RequestIDEQ(params.RequestID)).
			First(lookupCtx)
		if lookupErr == nil {
			return existing, nil
		}

		return nil, fmt.Errorf("failed to create usage log and settle token wallet: %w", err)
	}

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "Created usage log",
			log.Int("usage_log_id", usageLog.ID),
			log.Int("request_id", params.RequestID),
			log.String("model_id", params.ActualModelID),
			log.Int64("total_tokens", totalTokens),
		)
	}

	if s.OnUsageLogCreated != nil {
		s.OnUsageLogCreated()
	}

	return usageLog, nil
}

func isRetryableTokenWalletSettlementError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errTokenWalletWriteConflict) {
		return true
	}

	// modernc SQLite exposes primary/extended result codes through Code().
	// SQLite permits only one writer and a read-to-write transaction upgrade
	// can return BUSY immediately even with busy_timeout configured.
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED.
			return true
		}
	}

	// Keep compatibility with wrapped SQLite drivers that do not expose Code.
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "sqlite_locked") ||
		strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked")
}

// CreateUsageLogFromRequest creates a usage log from request and response data.
func (s *UsageLogService) CreateUsageLogFromRequest(
	ctx context.Context,
	request *ent.Request,
	requestExec *ent.RequestExecution,
	usage *llm.Usage,
) (*ent.UsageLog, error) {
	if request == nil || usage == nil {
		return nil, nil
	}

	return s.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID:     request.ID,
		ProjectID:     request.ProjectID,
		ChannelID:     requestExec.ChannelID,
		ActualModelID: requestExec.ModelID,
		Usage:         usage,
		Source:        usagelog.Source(request.Source),
		Format:        request.Format,
		APIKeyID:      lo.ToPtr(request.APIKeyID),
	})
}
