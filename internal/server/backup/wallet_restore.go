package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/tokenwalletledger"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/usertokenwallet"
)

type usageRestoreIDMaps struct {
	requestIDs           map[int]int
	usageLogIDs          map[int]int
	usageLogIDsByRequest map[int]int
}

func newUsageRestoreIDMaps() *usageRestoreIDMaps {
	return &usageRestoreIDMaps{
		requestIDs:           map[int]int{},
		usageLogIDs:          map[int]int{},
		usageLogIDsByRequest: map[int]int{},
	}
}

func buildRestoredUsageLogIDMap(
	ctx context.Context,
	db *ent.Client,
	usageLogs []*BackupUsageLog,
	requestIDMap map[int]int,
) (map[int]int, map[int]int, error) {
	result := map[int]int{}
	requestIDs := make([]int, 0, len(requestIDMap))
	for _, requestID := range requestIDMap {
		requestIDs = append(requestIDs, requestID)
	}

	byRequestID := make(map[int]int, len(requestIDs))
	for start := 0; start < len(requestIDs); start += usageBackupBatchSize {
		end := min(start+usageBackupBatchSize, len(requestIDs))
		rows, err := db.UsageLog.Query().
			Where(usagelog.RequestIDIn(requestIDs[start:end]...)).
			Select(usagelog.FieldID, usagelog.FieldRequestID).
			All(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("build restored usage log ID map: %w", err)
		}
		for _, row := range rows {
			byRequestID[row.RequestID] = row.ID
		}
	}

	for _, usageData := range usageLogs {
		if usageData == nil || usageData.ID <= 0 {
			continue
		}
		requestID, ok := requestIDMap[usageData.RequestID]
		if !ok {
			continue
		}
		if usageLogID, ok := byRequestID[requestID]; ok {
			result[usageData.ID] = usageLogID
		}
	}

	return result, byRequestID, nil
}

func validateExistingUsageWalletFields(existing *ent.UsageLog, backup *BackupUsageLog) error {
	// Zero-valued fields were absent from pre-wallet backup versions. Validate
	// only data the backup proves it carries, then perform strict ledger-linked
	// validation in restoreTokenWalletData.
	if backup.EffectiveTokens != 0 && existing.EffectiveTokens != backup.EffectiveTokens {
		return fmt.Errorf("effective_tokens differs")
	}
	if backup.CacheReadTokensKnown && !existing.CacheReadTokensKnown {
		return fmt.Errorf("cache_read_tokens_known differs")
	}
	if backup.WalletConsumedTokens != 0 && existing.WalletConsumedTokens != backup.WalletConsumedTokens {
		return fmt.Errorf("wallet_consumed_tokens differs")
	}
	if backup.DonorCreditTokens != 0 && existing.DonorCreditTokens != backup.DonorCreditTokens {
		return fmt.Errorf("donor_credit_tokens differs")
	}

	return nil
}

type walletRestoreResolver struct {
	userByID              map[int]int
	userByExactEmail      map[string]int
	userByNormalizedEmail map[string]int
	ambiguousEmail        map[string]struct{}
	channelByID           map[int]int
	channelByName         map[string]int
}

func newWalletRestoreResolver(ctx context.Context, db *ent.Client) (*walletRestoreResolver, error) {
	allRowsCtx := schematype.SkipSoftDelete(ctx)
	users, err := db.User.Query().
		Select(user.FieldID, user.FieldEmail).
		All(allRowsCtx)
	if err != nil {
		return nil, fmt.Errorf("load users for token wallet restore: %w", err)
	}
	channels, err := db.Channel.Query().
		Select(channel.FieldID, channel.FieldName).
		All(allRowsCtx)
	if err != nil {
		return nil, fmt.Errorf("load channels for token wallet restore: %w", err)
	}

	resolver := &walletRestoreResolver{
		userByID:              make(map[int]int, len(users)),
		userByExactEmail:      make(map[string]int, len(users)),
		userByNormalizedEmail: make(map[string]int, len(users)),
		ambiguousEmail:        map[string]struct{}{},
		channelByID:           make(map[int]int, len(channels)),
		channelByName:         make(map[string]int, len(channels)),
	}
	for _, row := range users {
		resolver.userByID[row.ID] = row.ID
		resolver.userByExactEmail[row.Email] = row.ID
		normalized := normalizeWalletEmail(row.Email)
		if normalized == "" {
			continue
		}
		if existingID, ok := resolver.userByNormalizedEmail[normalized]; ok && existingID != row.ID {
			delete(resolver.userByNormalizedEmail, normalized)
			resolver.ambiguousEmail[normalized] = struct{}{}
			continue
		}
		if _, ambiguous := resolver.ambiguousEmail[normalized]; !ambiguous {
			resolver.userByNormalizedEmail[normalized] = row.ID
		}
	}
	for _, row := range channels {
		resolver.channelByID[row.ID] = row.ID
		resolver.channelByName[row.Name] = row.ID
	}

	return resolver, nil
}

func normalizeWalletEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func (r *walletRestoreResolver) resolveUserID(oldID int, email string) (int, bool) {
	if email != "" {
		if id, ok := r.userByExactEmail[email]; ok {
			return id, true
		}
		normalized := normalizeWalletEmail(email)
		if _, ambiguous := r.ambiguousEmail[normalized]; !ambiguous {
			if id, ok := r.userByNormalizedEmail[normalized]; ok {
				return id, true
			}
		}

		// An email-bearing backup must not silently transfer a permanent
		// balance to an unrelated account that happens to reuse the old numeric
		// ID. Numeric fallback is reserved for legacy rows without email.
		return 0, false
	}

	id, ok := r.userByID[oldID]
	return id, ok
}

func (r *walletRestoreResolver) resolveChannelID(
	oldID int,
	name string,
	channelIDMap map[int]int,
) int {
	if name != "" {
		if id, ok := r.channelByName[name]; ok {
			return id
		}
	}
	if id, ok := channelIDMap[oldID]; ok {
		return id
	}
	if id, ok := r.channelByID[oldID]; ok {
		return id
	}

	// Ledger references are deliberately not foreign keys. Preserve the source
	// ID as an immutable snapshot when its channel is no longer retained.
	return oldID
}

func (svc *BackupService) restoreTokenWalletData(
	ctx context.Context,
	db *ent.Client,
	wallets []*BackupUserTokenWallet,
	ledgers []*BackupTokenWalletLedger,
	usageMaps *usageRestoreIDMaps,
	channelIDMap map[int]int,
) error {
	if len(wallets) == 0 && len(ledgers) == 0 {
		// Pre-1.4 backups do not contain wallet data.
		return nil
	}
	if err := validateBackupWalletGroup(wallets, ledgers); err != nil {
		return fmt.Errorf("invalid token wallet backup: %w", err)
	}
	if usageMaps == nil {
		usageMaps = newUsageRestoreIDMaps()
	}

	resolver, err := newWalletRestoreResolver(ctx, db)
	if err != nil {
		return err
	}

	walletByUserID := make(map[int]*BackupUserTokenWallet, len(wallets))
	oldUserIDMap := make(map[int]int, len(wallets))
	for _, wallet := range wallets {
		if wallet == nil {
			continue
		}
		userID, ok := resolver.resolveUserID(wallet.UserID, wallet.UserEmail)
		if !ok {
			return fmt.Errorf("restore token wallet: user %q (old ID %d) not found", wallet.UserEmail, wallet.UserID)
		}
		if _, duplicate := walletByUserID[userID]; duplicate {
			return fmt.Errorf("restore token wallet: multiple backup users map to user %d", userID)
		}
		walletByUserID[userID] = wallet
		oldUserIDMap[wallet.UserID] = userID
	}

	mappedLedgers := make([]*BackupTokenWalletLedger, 0, len(ledgers))
	mappedUsage := make(map[*BackupTokenWalletLedger]bool, len(ledgers))
	for _, ledger := range ledgers {
		if ledger == nil {
			continue
		}
		userID, ok := resolver.resolveUserID(ledger.UserID, ledger.UserEmail)
		if !ok {
			return fmt.Errorf("restore token wallet ledger %d: user %q (old ID %d) not found", ledger.ID, ledger.UserEmail, ledger.UserID)
		}
		expectedUserID, ok := oldUserIDMap[ledger.UserID]
		if !ok || expectedUserID != userID {
			return fmt.Errorf("restore token wallet ledger %d: materialized wallet identity is missing", ledger.ID)
		}

		mapped := *ledger
		mapped.UserID = userID
		mapped.RequestID = svc.resolveWalletRequestID(ctx, db, ledger, usageMaps.requestIDs)
		mapped.UsageLogID, ok = resolveWalletUsageLogID(ledger, usageMaps)
		mapped.ChannelID = resolver.resolveChannelID(ledger.ChannelID, ledger.ChannelName, channelIDMap)
		mappedLedger := &mapped
		mappedLedgers = append(mappedLedgers, mappedLedger)
		mappedUsage[mappedLedger] = ok
	}

	affectedUserIDs := make([]int, 0, len(walletByUserID))
	for userID := range walletByUserID {
		affectedUserIDs = append(affectedUserIDs, userID)
	}
	if err := validateExistingWalletMaterialization(ctx, db, affectedUserIDs); err != nil {
		return err
	}

	for _, ledger := range mappedLedgers {
		existing, err := db.TokenWalletLedger.Query().
			Where(
				tokenwalletledger.RequestIDEQ(ledger.RequestID),
				tokenwalletledger.UserIDEQ(ledger.UserID),
				tokenwalletledger.KindEQ(ledger.Kind),
			).
			Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return fmt.Errorf("find existing token wallet ledger %d: %w", ledger.ID, err)
		}
		if existing != nil {
			if !sameTokenWalletLedger(existing, ledger) {
				return fmt.Errorf(
					"restore token wallet ledger %d: conflicting entry for request %d, user %d, kind %s",
					ledger.ID,
					ledger.RequestID,
					ledger.UserID,
					ledger.Kind,
				)
			}
		} else {
			create := db.TokenWalletLedger.Create().
				SetUserID(ledger.UserID).
				SetRequestID(ledger.RequestID).
				SetNillableUsageLogID(nilIfZero(ledger.UsageLogID)).
				SetNillableChannelID(nilIfZero(ledger.ChannelID)).
				SetChannelNameSnapshot(ledger.ChannelNameSnapshot).
				SetKind(ledger.Kind).
				SetAmountTokens(ledger.AmountTokens).
				SetEffectiveTokensSnapshot(ledger.EffectiveTokensSnapshot)
			if !ledger.CreatedAt.IsZero() {
				create.SetCreatedAt(ledger.CreatedAt)
			}
			if !ledger.UpdatedAt.IsZero() {
				create.SetUpdatedAt(ledger.UpdatedAt)
			}
			if _, err := create.Save(ctx); err != nil {
				return fmt.Errorf("restore token wallet ledger %d: %w", ledger.ID, err)
			}
		}

		if mappedUsage[ledger] {
			usage, err := db.UsageLog.Get(ctx, ledger.UsageLogID)
			if err != nil {
				return fmt.Errorf("read restored usage log %d for wallet ledger %d: %w", ledger.UsageLogID, ledger.ID, err)
			}
			if err := validateWalletLedgerUsage(ledger, usage); err != nil {
				return fmt.Errorf("restored wallet ledger %d is inconsistent with usage log: %w", ledger.ID, err)
			}
		}
	}

	totals, err := tokenWalletTotalsForUsers(ctx, db, affectedUserIDs)
	if err != nil {
		return err
	}
	for userID, backupWallet := range walletByUserID {
		total := totals[userID]
		balance, err := total.balance()
		if err != nil {
			return fmt.Errorf("restore token wallet for user %d: %w", userID, err)
		}

		existing, err := db.UserTokenWallet.Query().
			Where(usertokenwallet.UserIDEQ(userID)).
			Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return fmt.Errorf("read token wallet for user %d: %w", userID, err)
		}
		if existing == nil {
			create := db.UserTokenWallet.Create().
				SetUserID(userID).
				SetBalanceTokens(balance).
				SetLifetimeCreditedTokens(total.credited).
				SetLifetimeDebitedTokens(total.debited).
				SetVersion(backupWallet.Version)
			if !backupWallet.CreatedAt.IsZero() {
				create.SetCreatedAt(backupWallet.CreatedAt)
			}
			if !backupWallet.UpdatedAt.IsZero() {
				create.SetUpdatedAt(backupWallet.UpdatedAt)
			}
			if _, err := create.Save(ctx); err != nil {
				return fmt.Errorf("create restored token wallet for user %d: %w", userID, err)
			}
			continue
		}

		if existing.BalanceTokens == balance &&
			existing.LifetimeCreditedTokens == total.credited &&
			existing.LifetimeDebitedTokens == total.debited {
			continue
		}
		version := max(existing.Version+1, backupWallet.Version)
		if _, err := db.UserTokenWallet.UpdateOne(existing).
			SetBalanceTokens(balance).
			SetLifetimeCreditedTokens(total.credited).
			SetLifetimeDebitedTokens(total.debited).
			SetVersion(version).
			Save(ctx); err != nil {
			return fmt.Errorf("update restored token wallet for user %d: %w", userID, err)
		}
	}

	return nil
}

func (svc *BackupService) resolveWalletRequestID(
	ctx context.Context,
	db *ent.Client,
	ledger *BackupTokenWalletLedger,
	requestIDMap map[int]int,
) int {
	if id, ok := requestIDMap[ledger.RequestID]; ok {
		return id
	}

	if !ledger.RequestCreatedAt.IsZero() {
		query := db.Request.Query().Where(request.CreatedAtEQ(ledger.RequestCreatedAt))
		if ledger.RequestExternalID != "" {
			query.Where(request.ExternalIDEQ(ledger.RequestExternalID))
		}
		rows, err := query.Limit(2).All(ctx)
		if err == nil && len(rows) == 1 {
			return rows[0].ID
		}
	}

	// Orphaned ledger references are valid by design. Preserve the old ID when
	// the corresponding request is no longer retained.
	return ledger.RequestID
}

func resolveWalletUsageLogID(ledger *BackupTokenWalletLedger, usageMaps *usageRestoreIDMaps) (int, bool) {
	if id, ok := usageMaps.usageLogIDs[ledger.UsageLogID]; ok {
		return id, true
	}
	if requestID, ok := usageMaps.requestIDs[ledger.UsageRequestID]; ok {
		if usageLogID, ok := usageMaps.usageLogIDsByRequest[requestID]; ok {
			return usageLogID, true
		}
	}
	if requestID, ok := usageMaps.requestIDs[ledger.RequestID]; ok {
		if usageLogID, ok := usageMaps.usageLogIDsByRequest[requestID]; ok {
			return usageLogID, true
		}
	}

	return ledger.UsageLogID, false
}

func sameTokenWalletLedger(existing *ent.TokenWalletLedger, backup *BackupTokenWalletLedger) bool {
	usageMatches := backup.UsageLogID == 0 || existing.UsageLogID == backup.UsageLogID
	channelMatches := backup.ChannelID == 0 || existing.ChannelID == backup.ChannelID
	createdAtMatches := backup.CreatedAt.IsZero() || existing.CreatedAt.Equal(backup.CreatedAt)

	return existing.UserID == backup.UserID &&
		existing.RequestID == backup.RequestID &&
		usageMatches &&
		channelMatches &&
		existing.ChannelNameSnapshot == backup.ChannelNameSnapshot &&
		existing.Kind == backup.Kind &&
		existing.AmountTokens == backup.AmountTokens &&
		existing.EffectiveTokensSnapshot == backup.EffectiveTokensSnapshot &&
		createdAtMatches
}

func validateExistingWalletMaterialization(
	ctx context.Context,
	db *ent.Client,
	userIDs []int,
) error {
	if len(userIDs) == 0 {
		return nil
	}
	totals, err := tokenWalletTotalsForUsers(ctx, db, userIDs)
	if err != nil {
		return err
	}
	wallets, err := db.UserTokenWallet.Query().
		Where(usertokenwallet.UserIDIn(userIDs...)).
		All(ctx)
	if err != nil {
		return fmt.Errorf("read existing token wallets: %w", err)
	}
	walletByUser := make(map[int]*ent.UserTokenWallet, len(wallets))
	for _, wallet := range wallets {
		walletByUser[wallet.UserID] = wallet
	}

	for _, userID := range userIDs {
		total := totals[userID]
		balance, err := total.balance()
		if err != nil {
			return fmt.Errorf("existing token wallet for user %d: %w", userID, err)
		}
		wallet := walletByUser[userID]
		if wallet == nil {
			if balance != 0 || total.credited != 0 || total.debited != 0 {
				return fmt.Errorf("existing token wallet ledger has no materialized wallet for user %d", userID)
			}
			continue
		}
		if wallet.BalanceTokens != balance ||
			wallet.LifetimeCreditedTokens != total.credited ||
			wallet.LifetimeDebitedTokens != total.debited {
			return fmt.Errorf("existing token wallet materialization is inconsistent for user %d", userID)
		}
	}

	return nil
}

func tokenWalletTotalsForUsers(
	ctx context.Context,
	db *ent.Client,
	userIDs []int,
) (map[int]walletTokenTotals, error) {
	totals := make(map[int]walletTokenTotals, len(userIDs))
	if len(userIDs) == 0 {
		return totals, nil
	}
	rows, err := db.TokenWalletLedger.Query().
		Where(tokenwalletledger.UserIDIn(userIDs...)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("read token wallet ledger totals: %w", err)
	}
	for _, ledger := range rows {
		total := totals[ledger.UserID]
		switch ledger.Kind {
		case tokenwalletledger.KindCredit:
			total.credited, err = addWalletTokens(total.credited, ledger.AmountTokens)
		case tokenwalletledger.KindDebit:
			total.debited, err = addWalletTokens(total.debited, ledger.AmountTokens)
		default:
			err = fmt.Errorf("invalid token wallet ledger kind %q", ledger.Kind)
		}
		if err != nil {
			return nil, fmt.Errorf("aggregate token wallet for user %d: %w", ledger.UserID, err)
		}
		totals[ledger.UserID] = total
	}

	return totals, nil
}
