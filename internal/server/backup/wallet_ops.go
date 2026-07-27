package backup

import (
	"context"
	"fmt"
	"math"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/tokenwalletledger"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/user"
)

type walletTokenTotals struct {
	credited int64
	debited  int64
}

func (t walletTokenTotals) balance() (int64, error) {
	if t.debited > t.credited {
		return 0, fmt.Errorf("wallet debits exceed credits")
	}

	return t.credited - t.debited, nil
}

func addWalletTokens(current, amount int64) (int64, error) {
	if amount < 0 || current > math.MaxInt64-amount {
		return 0, fmt.Errorf("wallet token total overflow")
	}

	return current + amount, nil
}

// backupTokenWalletData exports the materialized wallet state and its
// append-only ledger as one validated group. It intentionally contains only
// stable identity/reference snapshots and never serializes credentials, API
// key values, request headers, or request/response bodies.
func (svc *BackupService) backupTokenWalletData(
	ctx context.Context,
) ([]*BackupUserTokenWallet, []*BackupTokenWalletLedger, error) {
	walletRows, err := svc.db.UserTokenWallet.Query().
		Order(ent.Asc("user_id")).
		All(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("backup token wallets: %w", err)
	}

	ledgerRows, err := svc.db.TokenWalletLedger.Query().
		Order(ent.Asc(tokenwalletledger.FieldID)).
		All(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("backup token wallet ledger: %w", err)
	}

	userIDs := make([]int, 0, len(walletRows)+len(ledgerRows))
	channelIDs := make([]int, 0, len(ledgerRows))
	requestIDs := make([]int, 0, len(ledgerRows))
	usageLogIDs := make([]int, 0, len(ledgerRows))
	for _, wallet := range walletRows {
		userIDs = append(userIDs, wallet.UserID)
	}
	for _, ledger := range ledgerRows {
		userIDs = append(userIDs, ledger.UserID)
		if ledger.ChannelID > 0 {
			channelIDs = append(channelIDs, ledger.ChannelID)
		}
		if ledger.RequestID > 0 {
			requestIDs = append(requestIDs, ledger.RequestID)
		}
		if ledger.UsageLogID > 0 {
			usageLogIDs = append(usageLogIDs, ledger.UsageLogID)
		}
	}

	allRowsCtx := schematype.SkipSoftDelete(ctx)
	userEmails, err := backupWalletUserEmails(allRowsCtx, svc.db, uniquePositiveIDs(userIDs))
	if err != nil {
		return nil, nil, err
	}
	channelNames, err := backupWalletChannelNames(allRowsCtx, svc.db, uniquePositiveIDs(channelIDs))
	if err != nil {
		return nil, nil, err
	}
	requestSnapshots, err := backupWalletRequestSnapshots(ctx, svc.db, uniquePositiveIDs(requestIDs))
	if err != nil {
		return nil, nil, err
	}
	usageSnapshots, err := backupWalletUsageSnapshots(ctx, svc.db, uniquePositiveIDs(usageLogIDs))
	if err != nil {
		return nil, nil, err
	}

	wallets := make([]*BackupUserTokenWallet, 0, len(walletRows))
	for _, wallet := range walletRows {
		email, ok := userEmails[wallet.UserID]
		if !ok {
			return nil, nil, fmt.Errorf("backup token wallet %d: user %d not found", wallet.ID, wallet.UserID)
		}

		wallets = append(wallets, &BackupUserTokenWallet{
			ID:                     wallet.ID,
			CreatedAt:              wallet.CreatedAt,
			UpdatedAt:              wallet.UpdatedAt,
			UserID:                 wallet.UserID,
			UserEmail:              email,
			BalanceTokens:          wallet.BalanceTokens,
			LifetimeCreditedTokens: wallet.LifetimeCreditedTokens,
			LifetimeDebitedTokens:  wallet.LifetimeDebitedTokens,
			Version:                wallet.Version,
		})
	}

	ledgers := make([]*BackupTokenWalletLedger, 0, len(ledgerRows))
	for _, ledger := range ledgerRows {
		email, ok := userEmails[ledger.UserID]
		if !ok {
			return nil, nil, fmt.Errorf("backup token wallet ledger %d: user %d not found", ledger.ID, ledger.UserID)
		}

		data := &BackupTokenWalletLedger{
			ID:                      ledger.ID,
			CreatedAt:               ledger.CreatedAt,
			UpdatedAt:               ledger.UpdatedAt,
			UserID:                  ledger.UserID,
			UserEmail:               email,
			RequestID:               ledger.RequestID,
			UsageLogID:              ledger.UsageLogID,
			ChannelID:               ledger.ChannelID,
			ChannelName:             channelNames[ledger.ChannelID],
			ChannelNameSnapshot:     ledger.ChannelNameSnapshot,
			Kind:                    ledger.Kind,
			AmountTokens:            ledger.AmountTokens,
			EffectiveTokensSnapshot: ledger.EffectiveTokensSnapshot,
		}
		if req, ok := requestSnapshots[ledger.RequestID]; ok {
			data.RequestCreatedAt = req.CreatedAt
			data.RequestExternalID = req.ExternalID
		}
		if usage, ok := usageSnapshots[ledger.UsageLogID]; ok {
			data.UsageRequestID = usage.RequestID
			if err := validateWalletLedgerUsage(data, usage); err != nil {
				return nil, nil, fmt.Errorf("backup token wallet ledger %d: %w", ledger.ID, err)
			}
		}
		ledgers = append(ledgers, data)
	}

	if err := validateBackupWalletGroup(wallets, ledgers); err != nil {
		return nil, nil, err
	}

	return wallets, ledgers, nil
}

func backupWalletUserEmails(ctx context.Context, db *ent.Client, ids []int) (map[int]string, error) {
	result := make(map[int]string, len(ids))
	for start := 0; start < len(ids); start += usageBackupBatchSize {
		end := min(start+usageBackupBatchSize, len(ids))
		rows, err := db.User.Query().
			Where(user.IDIn(ids[start:end]...)).
			Select(user.FieldID, user.FieldEmail).
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("backup token wallet users: %w", err)
		}
		for _, row := range rows {
			result[row.ID] = row.Email
		}
	}

	return result, nil
}

func backupWalletChannelNames(ctx context.Context, db *ent.Client, ids []int) (map[int]string, error) {
	result := make(map[int]string, len(ids))
	for start := 0; start < len(ids); start += usageBackupBatchSize {
		end := min(start+usageBackupBatchSize, len(ids))
		rows, err := db.Channel.Query().
			Where(channel.IDIn(ids[start:end]...)).
			Select(channel.FieldID, channel.FieldName).
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("backup token wallet channels: %w", err)
		}
		for _, row := range rows {
			result[row.ID] = row.Name
		}
	}

	return result, nil
}

func backupWalletRequestSnapshots(ctx context.Context, db *ent.Client, ids []int) (map[int]*ent.Request, error) {
	result := make(map[int]*ent.Request, len(ids))
	for start := 0; start < len(ids); start += usageBackupBatchSize {
		end := min(start+usageBackupBatchSize, len(ids))
		rows, err := db.Request.Query().
			Where(request.IDIn(ids[start:end]...)).
			Select(request.FieldID, request.FieldCreatedAt, request.FieldExternalID).
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("backup token wallet requests: %w", err)
		}
		for _, row := range rows {
			result[row.ID] = row
		}
	}

	return result, nil
}

func backupWalletUsageSnapshots(ctx context.Context, db *ent.Client, ids []int) (map[int]*ent.UsageLog, error) {
	result := make(map[int]*ent.UsageLog, len(ids))
	for start := 0; start < len(ids); start += usageBackupBatchSize {
		end := min(start+usageBackupBatchSize, len(ids))
		rows, err := db.UsageLog.Query().
			Where(usagelog.IDIn(ids[start:end]...)).
			Select(
				usagelog.FieldID,
				usagelog.FieldRequestID,
				usagelog.FieldEffectiveTokens,
				usagelog.FieldWalletConsumedTokens,
				usagelog.FieldDonorCreditTokens,
			).
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("backup token wallet usage logs: %w", err)
		}
		for _, row := range rows {
			result[row.ID] = row
		}
	}

	return result, nil
}

func uniquePositiveIDs(ids []int) []int {
	seen := make(map[int]struct{}, len(ids))
	result := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}

	return result
}

func validateBackupWalletGroup(
	wallets []*BackupUserTokenWallet,
	ledgers []*BackupTokenWalletLedger,
) error {
	walletByUser := make(map[int]*BackupUserTokenWallet, len(wallets))
	for _, wallet := range wallets {
		if wallet == nil {
			continue
		}
		if wallet.UserID <= 0 {
			return fmt.Errorf("invalid token wallet identity for backup user %d", wallet.UserID)
		}
		if _, duplicate := walletByUser[wallet.UserID]; duplicate {
			return fmt.Errorf("duplicate token wallet for backup user %d", wallet.UserID)
		}
		if wallet.BalanceTokens < 0 || wallet.LifetimeCreditedTokens < 0 ||
			wallet.LifetimeDebitedTokens < 0 || wallet.Version < 0 {
			return fmt.Errorf("invalid negative token wallet state for backup user %d", wallet.UserID)
		}
		walletByUser[wallet.UserID] = wallet
	}

	totals := make(map[int]walletTokenTotals, len(walletByUser))
	for _, ledger := range ledgers {
		if ledger == nil {
			continue
		}
		if ledger.UserID <= 0 ||
			ledger.RequestID <= 0 || ledger.AmountTokens <= 0 ||
			ledger.EffectiveTokensSnapshot < 0 {
			return fmt.Errorf("invalid token wallet ledger %d", ledger.ID)
		}

		total := totals[ledger.UserID]
		var err error
		switch ledger.Kind {
		case tokenwalletledger.KindCredit:
			total.credited, err = addWalletTokens(total.credited, ledger.AmountTokens)
		case tokenwalletledger.KindDebit:
			total.debited, err = addWalletTokens(total.debited, ledger.AmountTokens)
		default:
			return fmt.Errorf("invalid token wallet ledger kind %q", ledger.Kind)
		}
		if err != nil {
			return fmt.Errorf("aggregate token wallet ledger for user %d: %w", ledger.UserID, err)
		}
		totals[ledger.UserID] = total
	}

	for userID, total := range totals {
		if _, ok := walletByUser[userID]; !ok {
			return fmt.Errorf("token wallet ledger has no materialized wallet for backup user %d", userID)
		}
		if _, err := total.balance(); err != nil {
			return fmt.Errorf("invalid token wallet ledger for backup user %d: %w", userID, err)
		}
	}

	for userID, wallet := range walletByUser {
		total := totals[userID]
		balance, err := total.balance()
		if err != nil {
			return fmt.Errorf("invalid token wallet ledger for backup user %d: %w", userID, err)
		}
		if wallet.BalanceTokens != balance ||
			wallet.LifetimeCreditedTokens != total.credited ||
			wallet.LifetimeDebitedTokens != total.debited {
			return fmt.Errorf("token wallet materialized state does not match ledger for backup user %d", userID)
		}
	}

	return nil
}

func validateWalletLedgerUsage(ledger *BackupTokenWalletLedger, usage *ent.UsageLog) error {
	if ledger == nil || usage == nil {
		return nil
	}
	if usage.RequestID != ledger.RequestID {
		return fmt.Errorf("usage log request does not match ledger request")
	}
	if usage.EffectiveTokens != ledger.EffectiveTokensSnapshot {
		return fmt.Errorf("usage effective tokens do not match wallet ledger")
	}

	switch ledger.Kind {
	case tokenwalletledger.KindCredit:
		if usage.DonorCreditTokens != ledger.AmountTokens {
			return fmt.Errorf("usage donor credit does not match wallet ledger")
		}
	case tokenwalletledger.KindDebit:
		if usage.WalletConsumedTokens != ledger.AmountTokens {
			return fmt.Errorf("usage wallet consumption does not match wallet ledger")
		}
	}

	return nil
}
