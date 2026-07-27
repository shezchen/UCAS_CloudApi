package backup

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/tokenwalletledger"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/usertokenwallet"
	"github.com/looplj/axonhub/internal/objects"
)

func createWalletBackupUsage(
	t *testing.T,
	client *ent.Client,
	ctx context.Context,
	projectID int,
	channelID int,
	apiKeyID int,
	effectiveTokens int64,
	walletConsumedTokens int64,
	donorCreditTokens int64,
) (*ent.Request, *ent.UsageLog) {
	t.Helper()

	req, err := client.Request.Create().
		SetProjectID(projectID).
		SetAPIKeyID(apiKeyID).
		SetChannelID(channelID).
		SetSource(request.SourceAPI).
		SetModelID("wallet-model").
		SetFormat("openai/chat_completions").
		SetRequestBody(objects.JSONRawMessage(`{"private":"must-not-be-in-usage-only-backup"}`)).
		SetStatus(request.StatusCompleted).
		SetStream(false).
		SetClientIP("127.0.0.1").
		Save(ctx)
	require.NoError(t, err)

	usage, err := client.UsageLog.Create().
		SetRequestID(req.ID).
		SetAPIKeyID(apiKeyID).
		SetProjectID(projectID).
		SetChannelID(channelID).
		SetModelID("wallet-model").
		SetPromptTokens(effectiveTokens).
		SetCompletionTokens(0).
		SetTotalTokens(effectiveTokens).
		SetEffectiveTokens(effectiveTokens).
		SetCacheReadTokensKnown(true).
		SetWalletConsumedTokens(walletConsumedTokens).
		SetDonorCreditTokens(donorCreditTokens).
		SetSource(usagelog.SourceAPI).
		SetFormat("openai/chat_completions").
		Save(ctx)
	require.NoError(t, err)

	return req, usage
}

func TestBackupService_TokenWalletRoundTripAndIdempotentRestore(t *testing.T) {
	source, sourceService, sourceCtx := setupBackupTest(t)
	defer source.Close()

	donor, err := source.User.Create().
		SetEmail("donor@mails.ucas.ac.cn").
		SetPassword("donor-password-secret").
		Save(sourceCtx)
	require.NoError(t, err)
	caller, err := source.User.Create().
		SetEmail("caller@mails.ucas.ac.cn").
		SetPassword("caller-password-secret").
		Save(sourceCtx)
	require.NoError(t, err)
	project := createBackupTestProject(t, source, sourceCtx, "Campus", "wallet project")

	donatedChannel, err := source.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Donated Channel").
		SetBaseURL("https://api.example.com").
		SetStatus(channel.StatusEnabled).
		SetUserID(donor.ID).
		SetCredentials(objects.ChannelCredentials{APIKey: "donated-channel-secret"}).
		SetSupportedModels([]string{"wallet-model"}).
		SetDefaultTestModel("wallet-model").
		Save(sourceCtx)
	require.NoError(t, err)
	ownerChannel := createBackupTestChannel(t, source, sourceCtx, "Owner Channel", channel.TypeAnthropic)

	callerKey := createBackupTestAPIKey(t, source, sourceCtx, caller, project, "caller-key", "sk-caller-secret")
	donorKey := createBackupTestAPIKey(t, source, sourceCtx, donor, project, "donor-key", "sk-donor-secret")

	creditRequest, creditUsage := createWalletBackupUsage(
		t, source, sourceCtx, project.ID, donatedChannel.ID, callerKey.ID, 120, 0, 60,
	)
	debitRequest, debitUsage := createWalletBackupUsage(
		t, source, sourceCtx, project.ID, ownerChannel.ID, donorKey.ID, 20, 20, 0,
	)

	_, err = source.TokenWalletLedger.Create().
		SetUserID(donor.ID).
		SetRequestID(creditRequest.ID).
		SetUsageLogID(creditUsage.ID).
		SetChannelID(donatedChannel.ID).
		SetChannelNameSnapshot(donatedChannel.Name).
		SetKind(tokenwalletledger.KindCredit).
		SetAmountTokens(60).
		SetEffectiveTokensSnapshot(120).
		Save(sourceCtx)
	require.NoError(t, err)
	_, err = source.TokenWalletLedger.Create().
		SetUserID(donor.ID).
		SetRequestID(debitRequest.ID).
		SetUsageLogID(debitUsage.ID).
		SetChannelID(ownerChannel.ID).
		SetChannelNameSnapshot(ownerChannel.Name).
		SetKind(tokenwalletledger.KindDebit).
		SetAmountTokens(20).
		SetEffectiveTokensSnapshot(20).
		Save(sourceCtx)
	require.NoError(t, err)
	sourceWallet, err := source.UserTokenWallet.Create().
		SetUserID(donor.ID).
		SetBalanceTokens(40).
		SetLifetimeCreditedTokens(60).
		SetLifetimeDebitedTokens(20).
		SetVersion(2).
		Save(sourceCtx)
	require.NoError(t, err)

	coreData, err := sourceService.Backup(sourceCtx, BackupOptions{})
	require.NoError(t, err)
	var coreBackup BackupData
	require.NoError(t, json.Unmarshal(coreData, &coreBackup))
	require.Empty(t, coreBackup.UsageLogs)
	require.Len(t, coreBackup.TokenWallets, 1)
	require.Len(t, coreBackup.TokenWalletLedgers, 2)

	data, err := sourceService.Backup(sourceCtx, BackupOptions{IncludeUsageStats: true})
	require.NoError(t, err)
	require.NotContains(t, string(data), "sk-caller-secret")
	require.NotContains(t, string(data), "sk-donor-secret")
	require.NotContains(t, string(data), "donor-password-secret")
	require.NotContains(t, string(data), "caller-password-secret")
	require.NotContains(t, string(data), "donated-channel-secret")
	require.NotContains(t, string(data), "must-not-be-in-usage-only-backup")
	require.NotContains(t, string(data), `"credentials"`)

	var backupData BackupData
	require.NoError(t, json.Unmarshal(data, &backupData))
	require.Equal(t, BackupVersion, backupData.Version)
	require.Len(t, backupData.TokenWallets, 1)
	require.Len(t, backupData.TokenWalletLedgers, 2)
	require.Equal(t, donor.Email, backupData.TokenWallets[0].UserEmail)
	require.Equal(t, sourceWallet.BalanceTokens, backupData.TokenWallets[0].BalanceTokens)

	target, targetService, targetCtx := setupBackupTest(t)
	defer target.Close()

	// Create users and channels in the opposite order so raw IDs differ.
	targetCaller, err := target.User.Create().
		SetEmail(caller.Email).
		SetPassword("target-caller-password").
		Save(targetCtx)
	require.NoError(t, err)
	targetDonor, err := target.User.Create().
		SetEmail(donor.Email).
		SetPassword("target-donor-password").
		Save(targetCtx)
	require.NoError(t, err)
	require.NotEqual(t, donor.ID, targetDonor.ID)
	_ = targetCaller

	targetProject := createBackupTestProject(t, target, targetCtx, project.Name, project.Description)
	targetOwnerChannel := createBackupTestChannel(t, target, targetCtx, ownerChannel.Name, ownerChannel.Type)
	targetDonatedChannel := createBackupTestChannel(t, target, targetCtx, donatedChannel.Name, donatedChannel.Type)
	require.NotEqual(t, donatedChannel.ID, targetDonatedChannel.ID)

	err = targetService.Restore(targetCtx, data, RestoreOptions{IncludeUsageStats: true})
	require.NoError(t, err)

	restoredWallet, err := target.UserTokenWallet.Query().
		Where(usertokenwallet.UserIDEQ(targetDonor.ID)).
		Only(targetCtx)
	require.NoError(t, err)
	require.Equal(t, int64(40), restoredWallet.BalanceTokens)
	require.Equal(t, int64(60), restoredWallet.LifetimeCreditedTokens)
	require.Equal(t, int64(20), restoredWallet.LifetimeDebitedTokens)
	require.Equal(t, sourceWallet.Version, restoredWallet.Version)

	restoredLedgers, err := target.TokenWalletLedger.Query().
		Where(tokenwalletledger.UserIDEQ(targetDonor.ID)).
		Order(ent.Asc(tokenwalletledger.FieldKind)).
		All(targetCtx)
	require.NoError(t, err)
	require.Len(t, restoredLedgers, 2)
	for _, ledger := range restoredLedgers {
		require.Equal(t, targetDonor.ID, ledger.UserID)
		_, err := target.Request.Get(targetCtx, ledger.RequestID)
		require.NoError(t, err)
		usage, err := target.UsageLog.Get(targetCtx, ledger.UsageLogID)
		require.NoError(t, err)
		require.Equal(t, ledger.RequestID, usage.RequestID)
		require.Equal(t, ledger.EffectiveTokensSnapshot, usage.EffectiveTokens)
		switch ledger.Kind {
		case tokenwalletledger.KindCredit:
			require.Equal(t, targetDonatedChannel.ID, ledger.ChannelID)
			require.Equal(t, ledger.AmountTokens, usage.DonorCreditTokens)
		case tokenwalletledger.KindDebit:
			require.Equal(t, targetOwnerChannel.ID, ledger.ChannelID)
			require.Equal(t, ledger.AmountTokens, usage.WalletConsumedTokens)
		}
	}

	restoredUsage, err := target.UsageLog.Query().All(targetCtx)
	require.NoError(t, err)
	require.Len(t, restoredUsage, 2)
	for _, usage := range restoredUsage {
		require.Equal(t, targetProject.ID, usage.ProjectID)
	}

	walletID := restoredWallet.ID
	walletVersion := restoredWallet.Version
	err = targetService.Restore(targetCtx, data, RestoreOptions{IncludeUsageStats: true})
	require.NoError(t, err)

	ledgerCount, err := target.TokenWalletLedger.Query().Count(targetCtx)
	require.NoError(t, err)
	require.Equal(t, 2, ledgerCount)
	usageCount, err := target.UsageLog.Query().Count(targetCtx)
	require.NoError(t, err)
	require.Equal(t, 2, usageCount)
	requestCount, err := target.Request.Query().Count(targetCtx)
	require.NoError(t, err)
	require.Equal(t, 2, requestCount)

	restoredWallet, err = target.UserTokenWallet.Get(targetCtx, walletID)
	require.NoError(t, err)
	require.Equal(t, int64(40), restoredWallet.BalanceTokens)
	require.Equal(t, walletVersion, restoredWallet.Version)
}

func TestBackupService_TokenWalletBackupRequiresConsistentMaterialization(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	userRow, err := client.User.Query().Where(user.EmailEQ("test@example.com")).Only(ctx)
	require.NoError(t, err)
	_, err = client.UserTokenWallet.Create().
		SetUserID(userRow.ID).
		SetBalanceTokens(1).
		SetLifetimeCreditedTokens(1).
		Save(ctx)
	require.NoError(t, err)

	_, err = service.Backup(ctx, BackupOptions{IncludeUsageStats: true})
	require.ErrorContains(t, err, "materialized state does not match ledger")
}

func TestBackupService_RestorePreWalletBackupWithoutWalletFields(t *testing.T) {
	client, service, ctx := setupBackupTest(t)
	defer client.Close()

	data, err := json.Marshal(BackupData{
		Version: BackupVersionV4,
	})
	require.NoError(t, err)
	require.NoError(t, service.Restore(ctx, data, RestoreOptions{IncludeUsageStats: true}))

	walletCount, err := client.UserTokenWallet.Query().Count(ctx)
	require.NoError(t, err)
	require.Zero(t, walletCount)
	ledgerCount, err := client.TokenWalletLedger.Query().Count(ctx)
	require.NoError(t, err)
	require.Zero(t, ledgerCount)
}
