package datamigrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
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

// backfillBatchSize bounds how many rows the backfill holds at once. It runs
// before the HTTP listener starts, so an installation with a large api_keys
// table would otherwise load the whole thing into memory and report nothing
// until it finished.
const backfillBatchSize = 500

func backfillAPIKeyHashes(ctx context.Context, client *ent.Client) error {
	var (
		// Rows that cannot be migrated stay selected by the predicate, so the
		// scan advances by id rather than re-reading the same batch forever.
		cursor   int
		migrated int
		skipped  int
	)

	for {
		batch, err := client.APIKey.Query().
			Where(
				apikey.Or(apikey.KeyHashIsNil(), apikey.KeyHashEQ("")),
				apikey.IDGT(cursor),
			).
			Order(ent.Asc(apikey.FieldID)).
			Limit(backfillBatchSize).
			All(ctx)
		if err != nil {
			return err
		}

		if len(batch) == 0 {
			break
		}

		cursor = batch[len(batch)-1].ID

		for _, key := range batch {
			raw := key.Key
			if raw == "" || xapikey.IsRedacted(raw) {
				// Nothing usable to hash; leave the row alone rather than guess.
				skipped++

				continue
			}

			// Changing the storage format of a key is not a change the key's
			// owner made, so keep updated_at where it was. Ent's update
			// default would otherwise stamp every row with the migration's
			// start time, which is visible in the UI and cannot be recovered
			// afterwards.
			_, err := client.APIKey.UpdateOneID(key.ID).
				SetKeyHash(xapikey.Hash(raw)).
				SetKeyPrefix(xapikey.Prefix(raw)).
				SetKey(xapikey.Redact(raw)).
				SetUpdatedAt(key.UpdatedAt).
				Save(ctx)
			if err != nil {
				return err
			}

			migrated++
		}

		if migrated > 0 {
			log.Info(ctx, "backfilling api key hashes",
				log.Int("migrated", migrated), log.Int("last_id", cursor))
		}
	}

	if migrated > 0 {
		log.Info(ctx, "backfilled api key hashes", log.Int("count", migrated))
	}

	if skipped > 0 {
		// Every startup re-reads these rows, so report them once as a total
		// rather than one line each.
		log.Warn(ctx, "api key rows have no plaintext key to backfill, left unchanged",
			log.Int("count", skipped))
	}

	return nil
}

func encryptChannelCredentials(ctx context.Context, client *ent.Client) error {
	primaryKeyID := xcrypto.PrimaryKeyID()

	var (
		cursor   int
		migrated int
	)

	for {
		// Reading the channels also validates that every stored credential
		// can be decoded with the configured keys. Doing it here, before the
		// HTTP listener starts, is what turns a missing or superseded key
		// into a startup failure instead of a healthy-looking instance that
		// fails every channel read once traffic arrives.
		batch, err := client.Channel.Query().
			Where(channel.IDGT(cursor)).
			Order(ent.Asc(channel.FieldID)).
			Limit(backfillBatchSize).
			All(ctx)
		if err != nil {
			if errors.Is(err, objects.ErrCredentialsEncryptedNoKey) {
				return fmt.Errorf(
					"refusing to start: stored channel credentials are encrypted but "+
						"security.credential_encryption_key (env AXONHUB_SECURITY_CREDENTIAL_ENCRYPTION_KEY) is not set; "+
						"restore the key those rows were written with, otherwise every channel read will fail: %w", err)
			}

			return err
		}

		if len(batch) == 0 {
			break
		}

		cursor = batch[len(batch)-1].ID

		if !xcrypto.Enabled() {
			// Nothing to rewrite, but the remaining batches still have to be
			// read so the check above covers every row.
			continue
		}

		for _, ch := range batch {
			// Covers plaintext rows and rows still sealed with a superseded
			// key, so promoting a replacement key to primary and keeping the
			// previous one in security.credential_decryption_keys rewrites
			// every row on the next start.
			if ch.Credentials.StoredEncrypted() && ch.Credentials.StoredKeyID() == primaryKeyID {
				continue
			}

			// Saving the decoded value re-serializes it through the
			// transparent encryption codec, which now produces the encrypted
			// envelope. updated_at is carried over for the same reason as in
			// the API key pass.
			_, err := client.Channel.UpdateOneID(ch.ID).
				SetCredentials(ch.Credentials).
				SetUpdatedAt(ch.UpdatedAt).
				Save(ctx)
			if err != nil {
				return err
			}

			migrated++
		}

		if migrated > 0 {
			log.Info(ctx, "encrypting stored channel credentials",
				log.Int("migrated", migrated), log.Int("last_id", cursor))
		}
	}

	if !xcrypto.Enabled() {
		log.Debug(ctx, "credential encryption key not configured, skipping channel credentials encryption backfill")
		return nil
	}

	if migrated > 0 {
		log.Info(ctx, "encrypted stored channel credentials",
			log.Int("count", migrated), log.String("key_id", primaryKeyID))
	}

	return nil
}
