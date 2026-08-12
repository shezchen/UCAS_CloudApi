# Upgrade Guide

## Upgrading to credential encryption and hashed API keys

> **This upgrade requires downtime.** Stop every running AxonHub instance
> before starting the new version. A rolling upgrade breaks API key
> authentication on every instance still running the old version.

This release changes how two kinds of secrets are stored:

- **API keys** are no longer stored in plaintext. The database keeps a
  SHA-256 hash for verification plus a redacted display value
  (`ah-1a2b3c4d5...e5f6`) for the UI.
- **Channel provider credentials** are encrypted with AES-256-GCM when
  `security.credential_encryption_key` is configured.

Existing rows are converted by a backfill that runs automatically at startup.

### Why the upgrade needs downtime

The first new instance to start rewrites the whole `key` column of the
`api_keys` table, replacing every plaintext key with its redacted display
value. An old instance authenticates by comparing the incoming key against
that column, so from that moment it rejects **every** API request — not a
subset, and not just new keys.

The two versions also stop exchanging cache invalidation events, because the
cache identity scheme is versioned alongside the storage format. An API key
revoked on one version would keep working on the other until its cache entry
expired.

### Upgrade procedure

1. **Back up the database.** The backfill is irreversible: once a plaintext
   key has been replaced with its hash and display value, the original cannot
   be recovered from the database. This backup is the only way back.

2. **Choose a credential encryption key** if you want channel credentials
   encrypted at rest. Generate one with:

   ```bash
   openssl rand -base64 32
   ```

   Store it somewhere other than the database — a secrets manager, or an
   environment variable populated from one. **Losing this key means losing
   every channel's provider credentials**, and no backup of the database
   alone will bring them back.

3. **Stop all AxonHub instances.**

4. **Start one new instance** with the key configured:

   ```bash
   export AXONHUB_SECURITY_CREDENTIAL_ENCRYPTION_KEY="<the key from step 2>"
   ```

   It runs the backfill during startup and refuses to come up if anything is
   wrong, so watch the logs before proceeding. Expect:

   ```
   backfilled api key hashes         {"count": 42}
   encrypted stored channel credentials  {"count": 7, "key_id": "94d9e4bf66cf"}
   ```

5. **Start the remaining instances.**

### After the upgrade

**Existing API keys keep working.** Their raw values are unchanged; only the
storage format changed. What changes is that the raw value is no longer
readable from AxonHub: the UI shows the redacted display form, and a key's
full value is shown exactly once, when it is created or rotated. Users who
have not recorded their key must rotate it to get a new one.

### Configuring the encryption key later

You can start without `security.credential_encryption_key` and add it later;
the next restart encrypts the channel credentials that are still in
plaintext. The reverse is not true — once credentials are encrypted, an
instance started without the key refuses to start rather than come up unable
to read any channel.

### Replacing the encryption key

Keys can be replaced without downtime:

1. Set the new key as `security.credential_encryption_key` and move the
   previous one to `security.credential_decryption_keys`:

   ```yaml
   security:
     credential_encryption_key: "<new key>"
     credential_decryption_keys:
       - "<previous key>"
   ```

2. Restart. The backfill re-encrypts every channel under the new key and logs
   the new `key_id`.

3. Remove the previous key from `credential_decryption_keys` and restart
   again.

Do not skip step 3's restart before removing the old key: any row still
sealed with it would become unreadable, and the instance would refuse to
start.

### Restoring a backup

Backups taken by this version (format 1.5) carry API key hashes, so restored
keys stay usable. Backups taken by earlier versions carry raw keys, which are
hashed on import.

Channel credentials inside a backup file are encrypted with whatever key was
configured when the backup was written. Keep the key with the backup, or the
backup cannot be restored.

## Related Documentation

- [Configuration Guide](configuration.md)
- [Docker Deployment](docker.md)
