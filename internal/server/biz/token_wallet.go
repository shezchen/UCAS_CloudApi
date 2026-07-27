package biz

import (
	"context"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/ent/tokenwalletledger"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/usertokenwallet"
)

const SystemKeyTokenWalletStartedAt = "campus_token_wallet_started_at"

var errTokenWalletWriteConflict = errors.New("token wallet write conflict")

// TokenWalletService owns permanent donation reward settlement. It is
// intentionally invoked only from the successful usage-log transaction.
type TokenWalletService struct {
	*AbstractService
}

func NewTokenWalletService(entClient *ent.Client) *TokenWalletService {
	return &TokenWalletService{
		AbstractService: &AbstractService{db: entClient},
	}
}

type tokenWalletSettlement struct {
	CallerUserID  int
	DonorUserID   int
	ChannelID     int
	ChannelName   string
	WalletDebit   int64
	DonorCredit   int64
	EffectiveUsed int64
}

// TokenWalletSummary is the durable wallet state presented to a user. The
// lifetime counters do not reset with daily or weekly account quota windows.
type TokenWalletSummary struct {
	BalanceTokens          int64
	LifetimeCreditedTokens int64
	LifetimeDebitedTokens  int64
}

// DonationChannelTokenAggregate describes rewarded use of one donated channel.
// EffectiveTokens excludes cache-read input; CreditedTokens is the permanent
// 50% wallet reward actually granted after self-use and Owner exclusions.
type DonationChannelTokenAggregate struct {
	ChannelID       int
	ChannelName     string
	RequestCount    int64
	EffectiveTokens int64
	CreditedTokens  int64
}

// DonationTokenSummary is the all-time, post-cutover reward aggregate for one
// donor. It is ledger-backed so channel expiry or deletion cannot erase it.
type DonationTokenSummary struct {
	RequestCount    int64
	EffectiveTokens int64
	CreditedTokens  int64
	Channels        []DonationChannelTokenAggregate
}

// EnsureStartedAt persists the wallet cutover exactly once. Restarts therefore
// cannot move the boundary backwards and historical usage is never rewarded.
func (s *TokenWalletService) EnsureStartedAt(ctx context.Context, now time.Time) (time.Time, error) {
	ctx = authz.WithSystemBypass(ctx, "token-wallet-cutover")
	client := s.entFromContext(ctx)

	value := now.UTC().Format(time.RFC3339Nano)
	if err := client.System.Create().
		SetKey(SystemKeyTokenWalletStartedAt).
		SetValue(value).
		OnConflict(sql.ConflictColumns(system.FieldKey)).
		Ignore().
		Exec(ctx); err != nil {
		return time.Time{}, fmt.Errorf("initialize token wallet cutover: %w", err)
	}

	row, err := client.System.Query().
		Where(system.KeyEQ(SystemKeyTokenWalletStartedAt)).
		Only(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read token wallet cutover: %w", err)
	}

	startedAt, err := time.Parse(time.RFC3339Nano, row.Value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse token wallet cutover: %w", err)
	}

	return startedAt.UTC(), nil
}

func (s *TokenWalletService) Balance(ctx context.Context, userID int) (int64, error) {
	if userID <= 0 {
		return 0, nil
	}

	ctx = authz.WithSystemBypass(ctx, "token-wallet-balance")
	summary, err := s.Summary(ctx, userID)
	if err != nil {
		return 0, err
	}

	return summary.BalanceTokens, nil
}

// Summary returns a user's own materialized wallet totals. Ent privacy limits
// ordinary user contexts to their own user_id; Owner and system contexts may
// request any account.
func (s *TokenWalletService) Summary(ctx context.Context, userID int) (TokenWalletSummary, error) {
	if userID <= 0 {
		return TokenWalletSummary{}, nil
	}

	row, err := s.entFromContext(ctx).UserTokenWallet.Query().
		Where(usertokenwallet.UserIDEQ(userID)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return TokenWalletSummary{}, nil
	}
	if err != nil {
		return TokenWalletSummary{}, fmt.Errorf("read token wallet summary: %w", err)
	}

	return TokenWalletSummary{
		BalanceTokens:          row.BalanceTokens,
		LifetimeCreditedTokens: row.LifetimeCreditedTokens,
		LifetimeDebitedTokens:  row.LifetimeDebitedTokens,
	}, nil
}

// DonationSummary returns post-cutover wallet-reward aggregates for channels
// donated by userID. Test traffic, self-use and Owner-owned channels never
// create credit ledger rows and are therefore absent by construction.
func (s *TokenWalletService) DonationSummary(ctx context.Context, userID int) (DonationTokenSummary, error) {
	if userID <= 0 {
		return DonationTokenSummary{Channels: []DonationChannelTokenAggregate{}}, nil
	}

	type aggregateRow struct {
		ChannelID       int    `json:"channel_id"`
		ChannelName     string `json:"channel_name"`
		RequestCount    int64  `json:"request_count"`
		EffectiveTokens int64  `json:"effective_tokens"`
		CreditedTokens  int64  `json:"credited_tokens"`
	}

	var rows []aggregateRow
	err := s.entFromContext(ctx).TokenWalletLedger.Query().
		Where(
			tokenwalletledger.UserIDEQ(userID),
			tokenwalletledger.KindEQ(tokenwalletledger.KindCredit),
		).
		Modify(func(selector *sql.Selector) {
			channelID := selector.C(tokenwalletledger.FieldChannelID)
			channelName := selector.C(tokenwalletledger.FieldChannelNameSnapshot)
			effective := selector.C(tokenwalletledger.FieldEffectiveTokensSnapshot)
			amount := selector.C(tokenwalletledger.FieldAmountTokens)

			selector.
				Select(
					channelID,
					sql.As(fmt.Sprintf("MAX(%s)", channelName), "channel_name"),
					sql.As("COUNT(*)", "request_count"),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", effective), "effective_tokens"),
					sql.As(fmt.Sprintf("COALESCE(SUM(%s), 0)", amount), "credited_tokens"),
				).
				GroupBy(channelID).
				OrderBy(sql.Desc("credited_tokens"))
		}).
		Scan(ctx, &rows)
	if err != nil {
		return DonationTokenSummary{}, fmt.Errorf("aggregate donated channel rewards: %w", err)
	}

	summary := DonationTokenSummary{
		Channels: make([]DonationChannelTokenAggregate, 0, len(rows)),
	}
	for _, row := range rows {
		summary.RequestCount += row.RequestCount
		summary.EffectiveTokens += row.EffectiveTokens
		summary.CreditedTokens += row.CreditedTokens
		summary.Channels = append(summary.Channels, DonationChannelTokenAggregate{
			ChannelID:       row.ChannelID,
			ChannelName:     row.ChannelName,
			RequestCount:    row.RequestCount,
			EffectiveTokens: row.EffectiveTokens,
			CreditedTokens:  row.CreditedTokens,
		})
	}

	return summary, nil
}

func (s *TokenWalletService) prepareSettlement(
	ctx context.Context,
	apiKeyID *int,
	channelID int,
	effectiveTokens int64,
	sourceIsTest bool,
	usageCreatedAt time.Time,
	walletStartedAt time.Time,
) (tokenWalletSettlement, error) {
	settlement := tokenWalletSettlement{
		ChannelID:     channelID,
		EffectiveUsed: max(effectiveTokens, int64(0)),
	}
	if sourceIsTest || effectiveTokens <= 0 || usageCreatedAt.Before(walletStartedAt) {
		return settlement, nil
	}

	client := s.entFromContext(ctx)
	readCtx := schematype.SkipSoftDelete(ctx)

	if apiKeyID != nil && *apiKeyID > 0 {
		key, err := client.APIKey.Query().
			Where(apikey.IDEQ(*apiKeyID)).
			Select(apikey.FieldUserID).
			Only(readCtx)
		if err != nil && !ent.IsNotFound(err) {
			return tokenWalletSettlement{}, fmt.Errorf("load wallet caller API key: %w", err)
		}
		if err == nil {
			settlement.CallerUserID = key.UserID
		}
	}

	if settlement.CallerUserID > 0 {
		if err := s.ensureWallet(ctx, settlement.CallerUserID); err != nil {
			return tokenWalletSettlement{}, err
		}

		wallet, err := client.UserTokenWallet.Query().
			Where(usertokenwallet.UserIDEQ(settlement.CallerUserID)).
			Only(ctx)
		if err != nil {
			return tokenWalletSettlement{}, fmt.Errorf("load caller token wallet: %w", err)
		}
		settlement.WalletDebit = min(wallet.BalanceTokens, settlement.EffectiveUsed)
	}

	ch, err := client.Channel.Query().
		Where(channel.IDEQ(channelID)).
		Select(channel.FieldID, channel.FieldName, channel.FieldUserID).
		Only(readCtx)
	if ent.IsNotFound(err) {
		return settlement, nil
	}
	if err != nil {
		return tokenWalletSettlement{}, fmt.Errorf("load contribution channel: %w", err)
	}

	settlement.ChannelName = ch.Name
	if ch.UserID == nil || *ch.UserID <= 0 || *ch.UserID == settlement.CallerUserID {
		return settlement, nil
	}

	donor, err := client.User.Query().
		Where(user.IDEQ(*ch.UserID)).
		Select(user.FieldID, user.FieldIsOwner).
		Only(readCtx)
	if ent.IsNotFound(err) {
		return settlement, nil
	}
	if err != nil {
		return tokenWalletSettlement{}, fmt.Errorf("load channel donor: %w", err)
	}
	if donor.IsOwner {
		return settlement, nil
	}

	settlement.DonorUserID = donor.ID
	settlement.DonorCredit = settlement.EffectiveUsed / 2
	if settlement.DonorCredit == 0 {
		settlement.DonorUserID = 0
	}

	return settlement, nil
}

func (s *TokenWalletService) ensureWallet(ctx context.Context, userID int) error {
	if userID <= 0 {
		return nil
	}

	if err := s.entFromContext(ctx).UserTokenWallet.Create().
		SetUserID(userID).
		OnConflict(sql.ConflictColumns(usertokenwallet.FieldUserID)).
		Ignore().
		Exec(ctx); err != nil {
		return fmt.Errorf("ensure token wallet for user %d: %w", userID, err)
	}

	return nil
}

func (s *TokenWalletService) applySettlement(
	ctx context.Context,
	requestID, usageLogID int,
	settlement tokenWalletSettlement,
) error {
	client := s.entFromContext(ctx)

	if settlement.WalletDebit > 0 {
		wallet, err := client.UserTokenWallet.Query().
			Where(usertokenwallet.UserIDEQ(settlement.CallerUserID)).
			Only(ctx)
		if err != nil {
			return fmt.Errorf("reload caller token wallet: %w", err)
		}

		updated, err := client.UserTokenWallet.Update().
			Where(
				usertokenwallet.UserIDEQ(settlement.CallerUserID),
				usertokenwallet.VersionEQ(wallet.Version),
				usertokenwallet.BalanceTokensGTE(settlement.WalletDebit),
			).
			AddBalanceTokens(-settlement.WalletDebit).
			AddLifetimeDebitedTokens(settlement.WalletDebit).
			AddVersion(1).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("debit caller token wallet: %w", err)
		}
		if updated != 1 {
			return errTokenWalletWriteConflict
		}

		if _, err := client.TokenWalletLedger.Create().
			SetUserID(settlement.CallerUserID).
			SetRequestID(requestID).
			SetUsageLogID(usageLogID).
			SetChannelID(settlement.ChannelID).
			SetChannelNameSnapshot(settlement.ChannelName).
			SetKind(tokenwalletledger.KindDebit).
			SetAmountTokens(settlement.WalletDebit).
			SetEffectiveTokensSnapshot(settlement.EffectiveUsed).
			Save(ctx); err != nil {
			if ent.IsConstraintError(err) {
				return errTokenWalletWriteConflict
			}
			return fmt.Errorf("record token wallet debit: %w", err)
		}
	}

	if settlement.DonorCredit > 0 {
		if err := s.ensureWallet(ctx, settlement.DonorUserID); err != nil {
			return err
		}

		updated, err := client.UserTokenWallet.Update().
			Where(usertokenwallet.UserIDEQ(settlement.DonorUserID)).
			AddBalanceTokens(settlement.DonorCredit).
			AddLifetimeCreditedTokens(settlement.DonorCredit).
			AddVersion(1).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("credit donor token wallet: %w", err)
		}
		if updated != 1 {
			return fmt.Errorf("credit donor token wallet: %w", errTokenWalletWriteConflict)
		}

		if _, err := client.TokenWalletLedger.Create().
			SetUserID(settlement.DonorUserID).
			SetRequestID(requestID).
			SetUsageLogID(usageLogID).
			SetChannelID(settlement.ChannelID).
			SetChannelNameSnapshot(settlement.ChannelName).
			SetKind(tokenwalletledger.KindCredit).
			SetAmountTokens(settlement.DonorCredit).
			SetEffectiveTokensSnapshot(settlement.EffectiveUsed).
			Save(ctx); err != nil {
			if ent.IsConstraintError(err) {
				return errTokenWalletWriteConflict
			}
			return fmt.Errorf("record token wallet credit: %w", err)
		}
	}

	return nil
}
