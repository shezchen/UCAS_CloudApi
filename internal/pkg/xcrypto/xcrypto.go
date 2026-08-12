// Package xcrypto provides process-wide symmetric encryption for sensitive
// values stored at rest (e.g. provider credentials).
//
// Keys are supplied once at startup (from configuration, see
// security.credential_encryption_key) via Configure. Encryption uses
// AES-256-GCM with a random nonce per value. The AES key is derived from the
// configured passphrase with Argon2id so that a stolen database cannot be
// attacked with fast offline guessing: a plain hash would let an attacker try
// billions of passphrases per second, which no practical length requirement
// can compensate for.
//
// Every ciphertext records the identifier of the key that produced it, and
// Configure accepts additional decryption-only keys, so a key can be replaced
// without a second at-rest format change: configure the new key as primary,
// keep the old one as a decryption key, and let callers rewrite their rows.
package xcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/argon2"
)

// MinKeyLength is the minimum accepted length for a configured passphrase.
// It is a floor against obvious mistakes, not the defence against brute
// force: that is the job of the Argon2id derivation below.
const MinKeyLength = 16

// Argon2id parameters. These apply once per distinct passphrase at startup,
// so a comparatively expensive configuration is affordable.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 4
	argonKeyLen  = 32
)

// derivationSalt is a fixed application salt. A per-deployment random salt
// would be preferable, but it would have to be read from the database, which
// is not open yet when Configure runs. Argon2id's cost makes precomputation
// against this constant impractical for anything but trivial passphrases.
var derivationSalt = []byte("axonhub/credential-encryption/v1")

type key struct {
	id   string
	aead cipher.AEAD
}

type keyring struct {
	primary *key
	byID    map[string]*key
}

func (r *keyring) ids() []string {
	ids := make([]string, 0, len(r.byID))
	for id := range r.byID {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	return ids
}

var current atomic.Pointer[keyring]

// derivationCache memoizes Argon2id output so repeated Configure calls with
// the same passphrase (tests, config reloads) do not pay the cost again. It
// is keyed by a digest so no passphrase is retained.
var derivationCache sync.Map // [32]byte -> [argonKeyLen]byte

func deriveKey(passphrase string) [argonKeyLen]byte {
	cacheKey := sha256.Sum256([]byte(passphrase))
	if cached, ok := derivationCache.Load(cacheKey); ok {
		return cached.([argonKeyLen]byte)
	}

	var derived [argonKeyLen]byte

	copy(derived[:], argon2.IDKey([]byte(passphrase), derivationSalt, argonTime, argonMemory, argonThreads, argonKeyLen))
	derivationCache.Store(cacheKey, derived)

	return derived
}

// keyIDFor returns a short, non-secret fingerprint of the derived key. It is
// stored alongside every ciphertext so decryption can select the right key
// and report a precise error when none matches.
func keyIDFor(derived []byte) string {
	sum := sha256.Sum256(append([]byte("axonhub/credential-key-id/v1"), derived...))
	return hex.EncodeToString(sum[:6])
}

func newKey(passphrase string) (*key, error) {
	if len(passphrase) < MinKeyLength {
		return nil, fmt.Errorf("encryption key must be at least %d characters", MinKeyLength)
	}

	derived := deriveKey(passphrase)

	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return nil, fmt.Errorf("failed to init cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to init GCM: %w", err)
	}

	return &key{id: keyIDFor(derived[:]), aead: gcm}, nil
}

// Configure installs the process-wide encryption keys. The primary key
// encrypts new values; additional keys are accepted for decryption only, so
// values written under a superseded key stay readable until they are
// rewritten. An empty primary passphrase disables encryption entirely.
func Configure(primary string, additional ...string) error {
	if primary == "" {
		if len(additional) > 0 {
			return fmt.Errorf("additional decryption keys require a primary encryption key")
		}

		current.Store(nil)

		return nil
	}

	primaryKey, err := newKey(primary)
	if err != nil {
		return err
	}

	ring := &keyring{
		primary: primaryKey,
		byID:    map[string]*key{primaryKey.id: primaryKey},
	}

	for i, passphrase := range additional {
		if passphrase == "" {
			continue
		}

		extra, err := newKey(passphrase)
		if err != nil {
			return fmt.Errorf("additional decryption key #%d: %w", i+1, err)
		}

		if _, exists := ring.byID[extra.id]; exists {
			continue
		}

		ring.byID[extra.id] = extra
	}

	current.Store(ring)

	return nil
}

// Enabled reports whether an encryption key has been configured.
func Enabled() bool {
	return current.Load() != nil
}

// PrimaryKeyID returns the identifier of the key used for new ciphertexts, or
// an empty string when encryption is disabled.
func PrimaryKeyID() string {
	r := current.Load()
	if r == nil {
		return ""
	}

	return r.primary.id
}

// Encrypt seals plaintext with AES-256-GCM under the primary key and returns
// base64(nonce || ciphertext) together with the key's identifier, which the
// caller must persist so Decrypt can select the same key.
//
// aad binds the ciphertext to the kind of value being stored; it must be
// supplied unchanged to Decrypt. It is not a substitute for binding to a
// specific row — see the callers for what that does and does not cover.
func Encrypt(plaintext []byte, aad []byte) (string, string, error) {
	r := current.Load()
	if r == nil {
		return "", "", fmt.Errorf("encryption key not configured")
	}

	k := r.primary

	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	sealed := k.aead.Seal(nonce, nonce, plaintext, aad)

	return base64.StdEncoding.EncodeToString(sealed), k.id, nil
}

// Decrypt reverses Encrypt. keyID selects the key that produced the payload;
// an empty keyID falls back to trying every configured key. It fails when no
// key is configured, when the named key is not configured, when the payload
// is malformed, or when authentication fails.
func Decrypt(encoded string, keyID string, aad []byte) ([]byte, error) {
	r := current.Load()
	if r == nil {
		return nil, fmt.Errorf("encryption key not configured")
	}

	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid encrypted payload: %w", err)
	}

	candidates := make([]*key, 0, len(r.byID))

	if keyID != "" {
		k, ok := r.byID[keyID]
		if !ok {
			return nil, fmt.Errorf(
				"payload was encrypted with key %q, which is not configured (configured keys: %v); "+
					"add the original key to security.credential_decryption_keys",
				keyID, r.ids())
		}

		candidates = append(candidates, k)
	} else {
		candidates = append(candidates, r.primary)

		for id, k := range r.byID {
			if id != r.primary.id {
				candidates = append(candidates, k)
			}
		}
	}

	for _, k := range candidates {
		nonceSize := k.aead.NonceSize()
		if len(sealed) < nonceSize {
			return nil, fmt.Errorf("invalid encrypted payload: too short")
		}

		plaintext, err := k.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], aad)
		if err == nil {
			return plaintext, nil
		}
	}

	return nil, fmt.Errorf("failed to decrypt payload: authentication failed")
}
