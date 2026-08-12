package datamigrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xapikey"
	"github.com/looplj/axonhub/internal/pkg/xcrypto"
)

// RunSecurityBackfill migrates existing rows to the hashed/encrypted at-rest
// format:
//
//   - api_keys: rows still holding a plaintext key (key_hash empty) get their
//     SHA-256 hash and display prefix backfilled, and the plaintext key column
//     is replaced with the redacted display form.
//   - channels: when a credential encryption key is configured, rows whose
//     credentials are still stored as plaintext JSON — or are still sealed
//     with a superseded key — are rewritten through the transparent
//     encryption codec under the current primary key.
//
// Unlike the version-gated migrations above, this runs on every startup: it
// only touches unmigrated rows (idempotent, cheap once complete), and the
// credentials pass must also cover operators who configure the encryption key
// some time after upgrading. Soft-deleted rows are included so no plaintext
// survives in them either.
func RunSecurityBackfill(ctx context.Context, client *ent.Client) error {
	ctx = authz.WithSystemBypass(ctx, "security-backfill")
	ctx = schematype.SkipSoftDelete(ctx)

	if err := backfillAPIKeyHashes(ctx, client); err != nil {
		return err
	}

	return encryptChannelCredentials(ctx, client)
}

func backfillAPIKeyHashes(ctx context.Context, client *ent.Client) error {
	legacyKeys, err := client.APIKey.Query().
		Where(apikey.Or(apikey.KeyHashIsNil(), apikey.KeyHashEQ(""))).
		All(ctx)
	if err != nil {
		return err
	}

	migrated := 0

	for _, key := range legacyKeys {
		raw := key.Key
		if raw == "" || xapikey.IsRedacted(raw) {
			// Nothing usable to hash; leave the row alone rather than guess.
			log.Warn(ctx, "api key row has no plaintext key to backfill, skipping",
				log.Int("api_key_id", key.ID))

			continue
		}

		_, err := client.APIKey.UpdateOneID(key.ID).
			SetKeyHash(xapikey.Hash(raw)).
			SetKeyPrefix(xapikey.Prefix(raw)).
			SetKey(xapikey.Redact(raw)).
			Save(ctx)
		if err != nil {
			return err
		}

		migrated++
	}

	if migrated > 0 {
		log.Info(ctx, "backfilled api key hashes", log.Int("count", migrated))
	}

	return nil
}

func encryptChannelCredentials(ctx context.Context, client *ent.Client) error {
	// Reading the channels also validates that every stored credential can be
	// decoded with the configured keys. Doing it here, before the server
	// starts listening, is what turns a missing or superseded key into a
	// startup failure instead of a healthy-looking instance that fails every
	// channel read once traffic arrives.
	channels, err := client.Channel.Query().All(ctx)
	if err != nil {
		if errors.Is(err, objects.ErrCredentialsEncryptedNoKey) {
			return fmt.Errorf(
				"refusing to start: stored channel credentials are encrypted but "+
					"security.credential_encryption_key (env AXONHUB_SECURITY_CREDENTIAL_ENCRYPTION_KEY) is not set; "+
					"restore the key those rows were written with, otherwise every channel read will fail: %w", err)
		}

		return err
	}

	if !xcrypto.Enabled() {
		log.Debug(ctx, "credential encryption key not configured, skipping channel credentials encryption backfill")
		return nil
	}

	primaryKeyID := xcrypto.PrimaryKeyID()

	migrated := 0

	for _, ch := range channels {
		// Covers plaintext rows and rows still sealed with a superseded key,
		// so promoting a replacement key to primary and keeping the previous
		// one in security.credential_decryption_keys rewrites every row on
		// the next start.
		if ch.Credentials.StoredEncrypted() && ch.Credentials.StoredKeyID() == primaryKeyID {
			continue
		}

		// Saving the decoded value re-serializes it through the transparent
		// encryption codec, which now produces the encrypted envelope.
		_, err := client.Channel.UpdateOneID(ch.ID).
			SetCredentials(ch.Credentials).
			Save(ctx)
		if err != nil {
			return err
		}

		migrated++
	}

	if migrated > 0 {
		log.Info(ctx, "encrypted stored channel credentials",
			log.Int("count", migrated), log.String("key_id", primaryKeyID))
	}

	return nil
}
