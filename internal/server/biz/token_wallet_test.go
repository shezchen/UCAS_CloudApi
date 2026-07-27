package biz

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/ent/tokenwalletledger"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/usertokenwallet"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/llm"
)

type tokenWalletTestFixture struct {
	ctx     context.Context
	client  *ent.Client
	service *UsageLogService
	project *ent.Project
	caller  *ent.User
	donor   *ent.User
	apiKey  *ent.APIKey
	channel *ent.Channel
}

func newTokenWalletTestFixture(t *testing.T, name string) *tokenWalletTestFixture {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:"+name+"?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	return newTokenWalletTestFixtureWithClient(t, name, client)
}

func newConcurrentTokenWalletTestFixture(t *testing.T, name string) *tokenWalletTestFixture {
	t.Helper()

	dsn := filepath.Join(t.TempDir(), name+".db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)

	client := enttest.NewClient(t, enttest.WithOptions(
		ent.Driver(entsql.OpenDB("sqlite3", sqlDB)),
	))
	t.Cleanup(func() { _ = client.Close() })

	return newTokenWalletTestFixtureWithClient(t, name, client)
}

func newTokenWalletTestFixtureWithClient(
	t *testing.T,
	name string,
	client *ent.Client,
) *tokenWalletTestFixture {
	t.Helper()

	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	projectRow, err := client.Project.Create().
		SetName(name).
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	caller, err := client.User.Create().
		SetEmail(name + "-caller@example.edu").
		SetPassword("password").
		Save(ctx)
	require.NoError(t, err)

	donor, err := client.User.Create().
		SetEmail(name + "-donor@example.edu").
		SetPassword("password").
		Save(ctx)
	require.NoError(t, err)

	apiKeyRow, err := client.APIKey.Create().
		SetName(name + "-key").
		SetKey("ah-" + name).
		SetProjectID(projectRow.ID).
		SetUserID(caller.ID).
		Save(ctx)
	require.NoError(t, err)

	channelRow, err := client.Channel.Create().
		SetName(name + "-channel").
		SetType("openai").
		SetBaseURL("https://example.invalid/v1").
		SetSupportedModels([]string{"test-model"}).
		SetDefaultTestModel("test-model").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetUserID(donor.ID).
		Save(ctx)
	require.NoError(t, err)

	systemService := NewSystemService(SystemServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})

	return &tokenWalletTestFixture{
		ctx:     ctx,
		client:  client,
		service: NewUsageLogService(client, systemService, NewChannelServiceForTest(client)),
		project: projectRow,
		caller:  caller,
		donor:   donor,
		apiKey:  apiKeyRow,
		channel: channelRow,
	}
}

func (f *tokenWalletTestFixture) createUsage(
	t *testing.T,
	source usagelog.Source,
	usage *llm.Usage,
) *ent.UsageLog {
	t.Helper()

	requestRow, err := f.client.Request.Create().
		SetProjectID(f.project.ID).
		SetAPIKeyID(f.apiKey.ID).
		SetChannelID(f.channel.ID).
		SetModelID("test-model").
		SetFormat("openai/chat_completions").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(f.ctx)
	require.NoError(t, err)

	created, err := f.service.CreateUsageLog(f.ctx, CreateUsageLogParams{
		RequestID:     requestRow.ID,
		ProjectID:     f.project.ID,
		ChannelID:     f.channel.ID,
		ActualModelID: "test-model",
		Usage:         usage,
		Source:        source,
		Format:        "openai/chat_completions",
		APIKeyID:      &f.apiKey.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, created)

	return created
}

func TestUsageLogService_SettlesWalletAtomicallyAfterCutover(t *testing.T) {
	f := newTokenWalletTestFixture(t, "wallet-settlement")

	_, err := f.client.UserTokenWallet.Create().
		SetUserID(f.caller.ID).
		SetBalanceTokens(100).
		SetLifetimeCreditedTokens(100).
		Save(f.ctx)
	require.NoError(t, err)

	created := f.createUsage(t, usagelog.SourceAPI, &llm.Usage{
		PromptTokens:     100,
		CompletionTokens: 20,
		TotalTokens:      120,
		PromptTokensDetails: &llm.PromptTokensDetails{
			CachedTokens: 40,
		},
	})

	require.Equal(t, int64(80), created.EffectiveTokens)
	require.True(t, created.CacheReadTokensKnown)
	require.Equal(t, int64(80), created.WalletConsumedTokens)
	require.Equal(t, int64(40), created.DonorCreditTokens)

	callerWallet, err := f.client.UserTokenWallet.Query().
		Where(usertokenwallet.UserIDEQ(f.caller.ID)).
		Only(f.ctx)
	require.NoError(t, err)
	require.Equal(t, int64(20), callerWallet.BalanceTokens)
	require.Equal(t, int64(80), callerWallet.LifetimeDebitedTokens)

	donorWallet, err := f.client.UserTokenWallet.Query().
		Where(usertokenwallet.UserIDEQ(f.donor.ID)).
		Only(f.ctx)
	require.NoError(t, err)
	require.Equal(t, int64(40), donorWallet.BalanceTokens)
	require.Equal(t, int64(40), donorWallet.LifetimeCreditedTokens)

	ledgerRows, err := f.client.TokenWalletLedger.Query().All(f.ctx)
	require.NoError(t, err)
	require.Len(t, ledgerRows, 2)

	credit, err := f.client.TokenWalletLedger.Query().
		Where(tokenwalletledger.KindEQ(tokenwalletledger.KindCredit)).
		Only(f.ctx)
	require.NoError(t, err)
	require.Equal(t, f.donor.ID, credit.UserID)
	require.Equal(t, int64(40), credit.AmountTokens)
	require.Equal(t, int64(80), credit.EffectiveTokensSnapshot)
	require.Equal(t, f.channel.Name, credit.ChannelNameSnapshot)

	donations, err := f.service.WalletService.DonationSummary(f.ctx, f.donor.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), donations.RequestCount)
	require.Equal(t, int64(80), donations.EffectiveTokens)
	require.Equal(t, int64(40), donations.CreditedTokens)
	require.Len(t, donations.Channels, 1)
	require.Equal(t, f.channel.ID, donations.Channels[0].ChannelID)

	cutover, err := f.client.System.Query().
		Where(system.KeyEQ(SystemKeyTokenWalletStartedAt)).
		Only(f.ctx)
	require.NoError(t, err)
	require.NotEmpty(t, cutover.Value)
}

func TestUsageLogService_WalletSettlementIsIdempotent(t *testing.T) {
	f := newTokenWalletTestFixture(t, "wallet-idempotent")

	requestRow, err := f.client.Request.Create().
		SetProjectID(f.project.ID).
		SetAPIKeyID(f.apiKey.ID).
		SetChannelID(f.channel.ID).
		SetModelID("test-model").
		SetFormat("openai/chat_completions").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(f.ctx)
	require.NoError(t, err)

	params := CreateUsageLogParams{
		RequestID:     requestRow.ID,
		ProjectID:     f.project.ID,
		ChannelID:     f.channel.ID,
		ActualModelID: "test-model",
		Usage: &llm.Usage{
			PromptTokens:     80,
			CompletionTokens: 20,
			TotalTokens:      100,
		},
		Source:   usagelog.SourceAPI,
		Format:   "openai/chat_completions",
		APIKeyID: &f.apiKey.ID,
	}

	first, err := f.service.CreateUsageLog(f.ctx, params)
	require.NoError(t, err)
	second, err := f.service.CreateUsageLog(f.ctx, params)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)

	donorWallet, err := f.service.WalletService.Summary(f.ctx, f.donor.ID)
	require.NoError(t, err)
	require.Equal(t, int64(50), donorWallet.BalanceTokens)
	require.Equal(t, int64(50), donorWallet.LifetimeCreditedTokens)

	ledgerCount, err := f.client.TokenWalletLedger.Query().Count(f.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, ledgerCount)
}

func TestUsageLogService_APIKeyPrincipalSettlesAndExactlyExhaustsWallet(t *testing.T) {
	f := newTokenWalletTestFixture(t, "wallet-apikey-principal")

	_, err := f.client.UserTokenWallet.Create().
		SetUserID(f.caller.ID).
		SetBalanceTokens(80).
		SetLifetimeCreditedTokens(80).
		Save(f.ctx)
	require.NoError(t, err)

	requestRow, err := f.client.Request.Create().
		SetProjectID(f.project.ID).
		SetAPIKeyID(f.apiKey.ID).
		SetChannelID(f.channel.ID).
		SetModelID("test-model").
		SetFormat("openai/chat_completions").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(f.ctx)
	require.NoError(t, err)

	apiKeyCtx := authz.NewAPIKeyContext(
		ent.NewContext(context.Background(), f.client),
		f.apiKey.ID,
		f.project.ID,
	)
	apiKeyCtx = contexts.WithAPIKey(apiKeyCtx, f.apiKey)

	created, err := f.service.CreateUsageLog(apiKeyCtx, CreateUsageLogParams{
		RequestID:     requestRow.ID,
		ProjectID:     f.project.ID,
		ChannelID:     f.channel.ID,
		ActualModelID: "test-model",
		Usage: &llm.Usage{
			PromptTokens:     100,
			CompletionTokens: 20,
			TotalTokens:      120,
			PromptTokensDetails: &llm.PromptTokensDetails{
				CachedTokens: 40,
			},
		},
		Source: usagelog.SourceAPI,
		Format: "openai/chat_completions",
		// Intentionally omit APIKeyID: production requests obtain it from the
		// authenticated API-key context.
	})
	require.NoError(t, err)
	require.Equal(t, f.apiKey.ID, created.APIKeyID)
	require.Equal(t, int64(80), created.EffectiveTokens)
	require.Equal(t, int64(80), created.WalletConsumedTokens)
	require.Equal(t, int64(40), created.DonorCreditTokens)

	callerWallet, err := f.service.WalletService.Summary(f.ctx, f.caller.ID)
	require.NoError(t, err)
	require.Zero(t, callerWallet.BalanceTokens)
	require.Equal(t, int64(80), callerWallet.LifetimeDebitedTokens)

	donorWallet, err := f.service.WalletService.Summary(f.ctx, f.donor.ID)
	require.NoError(t, err)
	require.Equal(t, int64(40), donorWallet.BalanceTokens)
	require.Equal(t, int64(40), donorWallet.LifetimeCreditedTokens)
}

func TestUsageLogService_ConcurrentDuplicateRequestSettlesOnce(t *testing.T) {
	f := newConcurrentTokenWalletTestFixture(t, "wallet-concurrent-idempotent")

	_, err := f.client.UserTokenWallet.Create().
		SetUserID(f.caller.ID).
		SetBalanceTokens(100).
		SetLifetimeCreditedTokens(100).
		Save(f.ctx)
	require.NoError(t, err)

	requestRow, err := f.client.Request.Create().
		SetProjectID(f.project.ID).
		SetAPIKeyID(f.apiKey.ID).
		SetChannelID(f.channel.ID).
		SetModelID("test-model").
		SetFormat("openai/chat_completions").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(f.ctx)
	require.NoError(t, err)

	params := CreateUsageLogParams{
		RequestID:     requestRow.ID,
		ProjectID:     f.project.ID,
		ChannelID:     f.channel.ID,
		ActualModelID: "test-model",
		Usage: &llm.Usage{
			PromptTokens: 100,
			TotalTokens:  100,
		},
		Source:   usagelog.SourceAPI,
		Format:   "openai/chat_completions",
		APIKeyID: &f.apiKey.ID,
	}

	const callers = 8
	start := make(chan struct{})
	results := make(chan *ent.UsageLog, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			created, createErr := f.service.CreateUsageLog(f.ctx, params)
			results <- created
			errs <- createErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for createErr := range errs {
		require.NoError(t, createErr)
	}

	var usageLogID int
	for created := range results {
		require.NotNil(t, created)
		if usageLogID == 0 {
			usageLogID = created.ID
		}
		require.Equal(t, usageLogID, created.ID)
	}

	require.Equal(t, 1, f.client.UsageLog.Query().
		Where(usagelog.RequestIDEQ(requestRow.ID)).
		CountX(f.ctx))

	callerWallet, err := f.service.WalletService.Summary(f.ctx, f.caller.ID)
	require.NoError(t, err)
	require.Zero(t, callerWallet.BalanceTokens)
	require.Equal(t, int64(100), callerWallet.LifetimeDebitedTokens)

	donorWallet, err := f.service.WalletService.Summary(f.ctx, f.donor.ID)
	require.NoError(t, err)
	require.Equal(t, int64(50), donorWallet.BalanceTokens)
	require.Equal(t, int64(50), donorWallet.LifetimeCreditedTokens)

	require.Equal(t, 1, f.client.TokenWalletLedger.Query().
		Where(tokenwalletledger.KindEQ(tokenwalletledger.KindDebit)).
		CountX(f.ctx))
	require.Equal(t, 1, f.client.TokenWalletLedger.Query().
		Where(tokenwalletledger.KindEQ(tokenwalletledger.KindCredit)).
		CountX(f.ctx))
}

func TestUsageLogService_ConcurrentRequestsCannotOverdrawWallet(t *testing.T) {
	f := newConcurrentTokenWalletTestFixture(t, "wallet-concurrent-exhaustion")

	_, err := f.client.UserTokenWallet.Create().
		SetUserID(f.caller.ID).
		SetBalanceTokens(100).
		SetLifetimeCreditedTokens(100).
		Save(f.ctx)
	require.NoError(t, err)

	const requestCount = 8
	requestIDs := make([]int, 0, requestCount)
	for range requestCount {
		requestRow, createErr := f.client.Request.Create().
			SetProjectID(f.project.ID).
			SetAPIKeyID(f.apiKey.ID).
			SetChannelID(f.channel.ID).
			SetModelID("test-model").
			SetFormat("openai/chat_completions").
			SetStatus(request.StatusCompleted).
			SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
			Save(f.ctx)
		require.NoError(t, createErr)
		requestIDs = append(requestIDs, requestRow.ID)
	}

	start := make(chan struct{})
	logs := make(chan *ent.UsageLog, requestCount)
	errs := make(chan error, requestCount)
	var wg sync.WaitGroup
	wg.Add(requestCount)
	for _, requestID := range requestIDs {
		go func() {
			defer wg.Done()
			<-start
			created, createErr := f.service.CreateUsageLog(f.ctx, CreateUsageLogParams{
				RequestID:     requestID,
				ProjectID:     f.project.ID,
				ChannelID:     f.channel.ID,
				ActualModelID: "test-model",
				Usage: &llm.Usage{
					PromptTokens: 30,
					TotalTokens:  30,
				},
				Source:   usagelog.SourceAPI,
				Format:   "openai/chat_completions",
				APIKeyID: &f.apiKey.ID,
			})
			logs <- created
			errs <- createErr
		}()
	}
	close(start)
	wg.Wait()
	close(logs)
	close(errs)

	for createErr := range errs {
		require.NoError(t, createErr)
	}

	var consumed int64
	for created := range logs {
		require.NotNil(t, created)
		consumed += created.WalletConsumedTokens
	}
	require.Equal(t, int64(100), consumed)

	callerWallet, err := f.service.WalletService.Summary(f.ctx, f.caller.ID)
	require.NoError(t, err)
	require.Zero(t, callerWallet.BalanceTokens)
	require.Equal(t, int64(100), callerWallet.LifetimeDebitedTokens)

	donorWallet, err := f.service.WalletService.Summary(f.ctx, f.donor.ID)
	require.NoError(t, err)
	require.Equal(t, int64(requestCount*15), donorWallet.BalanceTokens)
	require.Equal(t, int64(requestCount*15), donorWallet.LifetimeCreditedTokens)
	require.Equal(t, requestCount, f.client.TokenWalletLedger.Query().
		Where(tokenwalletledger.KindEQ(tokenwalletledger.KindCredit)).
		CountX(f.ctx))
}

func TestTokenWalletService_CutoverIsStableAndDoesNotBackfill(t *testing.T) {
	f := newTokenWalletTestFixture(t, "wallet-cutover")

	requestRow, err := f.client.Request.Create().
		SetProjectID(f.project.ID).
		SetAPIKeyID(f.apiKey.ID).
		SetChannelID(f.channel.ID).
		SetModelID("test-model").
		SetFormat("openai/chat_completions").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(f.ctx)
	require.NoError(t, err)

	historicalCreatedAt := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	historical, err := f.client.UsageLog.Create().
		SetCreatedAt(historicalCreatedAt).
		SetRequestID(requestRow.ID).
		SetAPIKeyID(f.apiKey.ID).
		SetProjectID(f.project.ID).
		SetChannelID(f.channel.ID).
		SetModelID("test-model").
		SetPromptTokens(100).
		SetTotalTokens(100).
		SetEffectiveTokens(100).
		SetSource(usagelog.SourceAPI).
		SetFormat("openai/chat_completions").
		Save(f.ctx)
	require.NoError(t, err)

	cutoverAt := time.Date(2026, time.July, 27, 12, 0, 0, 123, time.UTC)
	first, err := f.service.WalletService.EnsureStartedAt(f.ctx, cutoverAt)
	require.NoError(t, err)
	second, err := f.service.WalletService.EnsureStartedAt(f.ctx, cutoverAt.Add(24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, cutoverAt, first)
	require.Equal(t, first, second)

	reloaded, err := f.client.UsageLog.Get(f.ctx, historical.ID)
	require.NoError(t, err)
	require.Zero(t, reloaded.WalletConsumedTokens)
	require.Zero(t, reloaded.DonorCreditTokens)
	require.Zero(t, f.client.UserTokenWallet.Query().CountX(f.ctx))
	require.Zero(t, f.client.TokenWalletLedger.Query().CountX(f.ctx))
}

func TestUsageLogService_TestAndPreCutoverUsageDoNotSettleWallet(t *testing.T) {
	f := newTokenWalletTestFixture(t, "wallet-exclusions")

	_, err := f.client.UserTokenWallet.Create().
		SetUserID(f.caller.ID).
		SetBalanceTokens(100).
		SetLifetimeCreditedTokens(100).
		Save(f.ctx)
	require.NoError(t, err)

	testLog := f.createUsage(t, usagelog.SourceTest, &llm.Usage{
		PromptTokens: 100,
		TotalTokens:  100,
	})
	require.Zero(t, testLog.WalletConsumedTokens)
	require.Zero(t, testLog.DonorCreditTokens)

	future := time.Now().UTC().Add(time.Hour)
	require.NoError(t, f.client.System.Update().
		Where(system.KeyEQ(SystemKeyTokenWalletStartedAt)).
		SetValue(future.Format(time.RFC3339Nano)).
		Exec(f.ctx))

	preCutoverLog := f.createUsage(t, usagelog.SourceAPI, &llm.Usage{
		PromptTokens: 100,
		TotalTokens:  100,
	})
	require.Zero(t, preCutoverLog.WalletConsumedTokens)
	require.Zero(t, preCutoverLog.DonorCreditTokens)

	callerWallet, err := f.service.WalletService.Summary(f.ctx, f.caller.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100), callerWallet.BalanceTokens)

	_, err = f.service.WalletService.Summary(f.ctx, f.donor.ID)
	require.NoError(t, err)
	ledgerCount, err := f.client.TokenWalletLedger.Query().Count(f.ctx)
	require.NoError(t, err)
	require.Zero(t, ledgerCount)
}

func TestUsageLogService_SelfAndOwnerChannelsDoNotEarnCredit(t *testing.T) {
	t.Run("self use", func(t *testing.T) {
		f := newTokenWalletTestFixture(t, "wallet-self")

		selfChannel, err := f.client.Channel.Create().
			SetName("wallet-self-own-channel").
			SetType("openai").
			SetBaseURL("https://example.invalid/v1").
			SetSupportedModels([]string{"test-model"}).
			SetDefaultTestModel("test-model").
			SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
			SetUserID(f.caller.ID).
			Save(f.ctx)
		require.NoError(t, err)
		f.channel = selfChannel

		created := f.createUsage(t, usagelog.SourceAPI, &llm.Usage{
			PromptTokens: 100,
			TotalTokens:  100,
		})
		require.Zero(t, created.DonorCreditTokens)
		require.Zero(t, f.client.TokenWalletLedger.Query().
			Where(tokenwalletledger.KindEQ(tokenwalletledger.KindCredit)).
			CountX(f.ctx))
	})

	t.Run("owner channel", func(t *testing.T) {
		f := newTokenWalletTestFixture(t, "wallet-owner")
		require.NoError(t, f.client.User.UpdateOne(f.donor).
			SetIsOwner(true).
			Exec(f.ctx))

		created := f.createUsage(t, usagelog.SourceAPI, &llm.Usage{
			PromptTokens: 100,
			TotalTokens:  100,
		})
		require.Zero(t, created.DonorCreditTokens)
		require.Zero(t, f.client.TokenWalletLedger.Query().
			Where(tokenwalletledger.KindEQ(tokenwalletledger.KindCredit)).
			CountX(f.ctx))
	})
}
