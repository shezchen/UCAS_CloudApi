package datamigrate_test

import (
	"context"
	"database/sql"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/migrate/datamigrate"
	"github.com/looplj/axonhub/internal/ent/migrate/schemahook"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xapikey"
	"github.com/looplj/axonhub/internal/pkg/xcrypto"
)

// newSecurityTestClient builds an ent client that shares its single sqlite
// connection with a raw *sql.DB handle, so the test can simulate legacy rows
// that predate the API key hashing hook.
func newSecurityTestClient(t *testing.T) (*ent.Client, *sql.DB) {
	t.Helper()

	sqlDB, err := sql.Open("sqlite3", "file:security_backfill?mode=memory&_fk=1")
	require.NoError(t, err)

	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	client := enttest.NewClient(t,
		enttest.WithOptions(ent.Driver(entsql.OpenDB("sqlite3", sqlDB))),
		enttest.WithMigrateOptions(
			migrate.WithGlobalUniqueID(false),
			migrate.WithForeignKeys(false),
			migrate.WithDropIndex(true),
			migrate.WithDropColumn(true),
			schema.WithHooks(schemahook.V0_3_0),
		),
	)
	t.Cleanup(func() { _ = client.Close() })

	return client, sqlDB
}

func TestRunSecurityBackfill_APIKeyHashes(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure(""))

	client, sqlDB := newSecurityTestClient(t)

	ctx := context.Background()
	seedCtx := authz.WithTestBypass(schematype.SkipSoftDelete(ent.NewContext(ctx, client)))

	proj, err := client.Project.Create().SetName("default").Save(seedCtx)
	require.NoError(t, err)

	created, err := client.APIKey.Create().
		SetName("legacy-key").
		SetKey("ah-temporary-value").
		SetProjectID(proj.ID).
		Save(seedCtx)
	require.NoError(t, err)

	// Rewrite the row into its pre-upgrade shape: plaintext key, no hash.
	const rawKey = "ah-legacy-plaintext-key-0123456789abcdef"
	_, err = sqlDB.Exec(`UPDATE api_keys SET "key" = ?, key_hash = NULL, key_prefix = '' WHERE id = ?`, rawKey, created.ID)
	require.NoError(t, err)

	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))

	migrated, err := client.APIKey.Get(seedCtx, created.ID)
	require.NoError(t, err)
	require.Equal(t, xapikey.Hash(rawKey), migrated.KeyHash)
	require.Equal(t, xapikey.Prefix(rawKey), migrated.KeyPrefix)
	require.Equal(t, xapikey.Redact(rawKey), migrated.Key)

	// Idempotent: a second run leaves the row untouched.
	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))

	again, err := client.APIKey.Get(seedCtx, created.ID)
	require.NoError(t, err)
	require.Equal(t, migrated.Key, again.Key)
	require.Equal(t, migrated.KeyHash, again.KeyHash)
	require.Equal(t, migrated.UpdatedAt, again.UpdatedAt)
}

func TestRunSecurityBackfill_ChannelCredentials(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })

	// Channel is created while encryption is disabled → stored as plaintext,
	// exactly like rows that predate the encryption key rollout.
	require.NoError(t, xcrypto.Configure(""))

	client, sqlDB := newSecurityTestClient(t)

	ctx := context.Background()
	seedCtx := authz.WithTestBypass(ent.NewContext(ctx, client))

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("legacy-channel").
		SetBaseURL("https://api.example.com").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-provider-secret"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		Save(seedCtx)
	require.NoError(t, err)

	readCredentialsColumn := func() string {
		var raw string
		require.NoError(t, sqlDB.QueryRow(`SELECT credentials FROM channels WHERE id = ?`, ch.ID).Scan(&raw))
		return raw
	}

	require.Contains(t, readCredentialsColumn(), "sk-provider-secret")

	// Without a key the backfill is a no-op.
	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))
	require.Contains(t, readCredentialsColumn(), "sk-provider-secret")

	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))
	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))

	encryptedColumn := readCredentialsColumn()
	require.NotContains(t, encryptedColumn, "sk-provider-secret")
	require.Contains(t, encryptedColumn, "__axonhub_enc")

	// The transparent codec still yields the plaintext credentials.
	loaded, err := client.Channel.Get(seedCtx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, "sk-provider-secret", loaded.Credentials.APIKey)
	require.True(t, loaded.Credentials.StoredEncrypted())

	// Idempotent: already-encrypted rows are skipped (bytes unchanged, no
	// re-encryption with a fresh nonce).
	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))
	require.Equal(t, encryptedColumn, readCredentialsColumn())
}

// TestRunSecurityBackfill_RefusesToStartWithoutKey pins the fail-fast
// behaviour for the most damaging misconfiguration: encrypted rows present,
// no key supplied. Starting successfully here would produce a healthy-looking
// instance that fails every channel read.
func TestRunSecurityBackfill_RefusesToStartWithoutKey(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	client, _ := newSecurityTestClient(t)

	ctx := context.Background()
	seedCtx := authz.WithTestBypass(ent.NewContext(ctx, client))

	_, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("encrypted-channel").
		SetBaseURL("https://api.example.com").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-provider-secret"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		Save(seedCtx)
	require.NoError(t, err)

	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))

	// Operator restarts without the key.
	require.NoError(t, xcrypto.Configure(""))

	err = datamigrate.RunSecurityBackfill(ctx, client)
	require.Error(t, err, "startup must fail rather than serve an instance that cannot read channels")
	require.Contains(t, err.Error(), "security.credential_encryption_key",
		"the error must name the missing configuration")
	require.ErrorIs(t, err, objects.ErrCredentialsEncryptedNoKey)
}

// TestRunSecurityBackfill_RewritesSupersededKey covers key replacement: with
// the previous key kept for decryption, the startup backfill moves every row
// onto the new primary key so the old one can then be dropped.
func TestRunSecurityBackfill_RewritesSupersededKey(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })

	const (
		oldKey = "the-original-master-key-value"
		newKey = "the-replacement-master-key!!!"
	)

	require.NoError(t, xcrypto.Configure(oldKey))
	oldKeyID := xcrypto.PrimaryKeyID()

	client, _ := newSecurityTestClient(t)

	ctx := context.Background()
	seedCtx := authz.WithTestBypass(ent.NewContext(ctx, client))

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("rotating-channel").
		SetBaseURL("https://api.example.com").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-provider-secret"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		Save(seedCtx)
	require.NoError(t, err)

	loaded, err := client.Channel.Get(seedCtx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, oldKeyID, loaded.Credentials.StoredKeyID())

	require.NoError(t, xcrypto.Configure(newKey, oldKey))
	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))

	rotated, err := client.Channel.Get(seedCtx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, "sk-provider-secret", rotated.Credentials.APIKey)
	require.Equal(t, xcrypto.PrimaryKeyID(), rotated.Credentials.StoredKeyID())

	// With every row rewritten the superseded key can be removed.
	require.NoError(t, xcrypto.Configure(newKey))
	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))

	final, err := client.Channel.Get(seedCtx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, "sk-provider-secret", final.Credentials.APIKey)
}

// TestRunSecurityBackfill_NoKeyWithoutEncryptedRows keeps the fail-fast check
// from blocking the supported plaintext deployment.
func TestRunSecurityBackfill_NoKeyWithoutEncryptedRows(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure(""))

	client, _ := newSecurityTestClient(t)

	ctx := context.Background()
	seedCtx := authz.WithTestBypass(ent.NewContext(ctx, client))

	_, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("plaintext-channel").
		SetBaseURL("https://api.example.com").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-provider-secret"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		Save(seedCtx)
	require.NoError(t, err)

	require.NoError(t, datamigrate.RunSecurityBackfill(ctx, client))
}
