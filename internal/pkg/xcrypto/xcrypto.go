// Package xcrypto provides process-wide symmetric encryption for sensitive
// values stored at rest (e.g. provider credentials).
//
// The master key is supplied once at startup (from configuration, see
// security.credential_encryption_key) via Configure. Encryption uses
// AES-256-GCM with a random nonce per value; the AES key is derived from the
// configured passphrase with SHA-256, so operators may supply any
// sufficiently long secret string.
package xcrypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"sync/atomic"
)

// MinKeyLength is the minimum accepted length for the configured passphrase.
// A short passphrase would make the derived AES key trivially brute-forceable.
const MinKeyLength = 16

type keeper struct {
	aead cipher.AEAD
}

var current atomic.Pointer[keeper]

// Configure derives the process-wide encryption key from the given
// passphrase. An empty passphrase disables encryption (plaintext
// passthrough); a non-empty passphrase shorter than MinKeyLength is rejected.
func Configure(passphrase string) error {
	if passphrase == "" {
		current.Store(nil)
		return nil
	}

	if len(passphrase) < MinKeyLength {
		return fmt.Errorf("encryption key must be at least %d characters", MinKeyLength)
	}

	key := sha256.Sum256([]byte(passphrase))

	block, err := aes.NewCipher(key[:])
	if err != nil {
		return fmt.Errorf("failed to init cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("failed to init GCM: %w", err)
	}

	current.Store(&keeper{aead: gcm})

	return nil
}

// Enabled reports whether an encryption key has been configured.
func Enabled() bool {
	return current.Load() != nil
}

// Encrypt seals plaintext with AES-256-GCM and returns
// base64(nonce || ciphertext). It fails when no key is configured.
func Encrypt(plaintext []byte) (string, error) {
	k := current.Load()
	if k == nil {
		return "", fmt.Errorf("encryption key not configured")
	}

	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}

	sealed := k.aead.Seal(nonce, nonce, plaintext, nil)

	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt. It fails when no key is configured, when the
// payload is malformed, or when the key does not match (authentication
// failure).
func Decrypt(encoded string) ([]byte, error) {
	k := current.Load()
	if k == nil {
		return nil, fmt.Errorf("encryption key not configured")
	}

	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid encrypted payload: %w", err)
	}

	nonceSize := k.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, fmt.Errorf("invalid encrypted payload: too short")
	}

	plaintext, err := k.aead.Open(nil, sealed[:nonceSize], sealed[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt payload (wrong or rotated key?): %w", err)
	}

	return plaintext, nil
}
