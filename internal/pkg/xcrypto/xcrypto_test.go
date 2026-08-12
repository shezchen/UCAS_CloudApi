package xcrypto_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/pkg/xcrypto"
)

var testAAD = []byte("axonhub/test")

func TestConfigure(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })

	require.NoError(t, xcrypto.Configure(""))
	require.False(t, xcrypto.Enabled())

	require.Error(t, xcrypto.Configure("too-short"), "keys below the minimum length must be rejected")
	require.False(t, xcrypto.Enabled())

	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))
	require.True(t, xcrypto.Enabled())
	require.NotEmpty(t, xcrypto.PrimaryKeyID())

	// Reconfiguring with an empty key disables encryption again.
	require.NoError(t, xcrypto.Configure(""))
	require.False(t, xcrypto.Enabled())
	require.Empty(t, xcrypto.PrimaryKeyID())

	require.Error(t, xcrypto.Configure("", "a-sufficiently-long-master-key"),
		"decryption keys without a primary key must be rejected")
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	plaintext := []byte(`{"apiKey":"sk-secret-value"}`)

	encrypted, keyID, err := xcrypto.Encrypt(plaintext, testAAD)
	require.NoError(t, err)
	require.NotContains(t, encrypted, "sk-secret-value")
	require.Equal(t, xcrypto.PrimaryKeyID(), keyID)

	decrypted, err := xcrypto.Decrypt(encrypted, keyID, testAAD)
	require.NoError(t, err)
	require.Equal(t, plaintext, decrypted)

	// GCM uses a random nonce, so equal plaintexts produce distinct ciphertexts.
	encrypted2, _, err := xcrypto.Encrypt(plaintext, testAAD)
	require.NoError(t, err)
	require.NotEqual(t, encrypted, encrypted2)
}

func TestDecrypt_WrongAAD(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	encrypted, keyID, err := xcrypto.Encrypt([]byte("secret"), testAAD)
	require.NoError(t, err)

	_, err = xcrypto.Decrypt(encrypted, keyID, []byte("axonhub/some-other-field"))
	require.Error(t, err, "a ciphertext must not decrypt under different associated data")
}

func TestDecrypt_WrongKey(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	encrypted, keyID, err := xcrypto.Encrypt([]byte("secret"), testAAD)
	require.NoError(t, err)

	require.NoError(t, xcrypto.Configure("another-different-master-key!"))

	_, err = xcrypto.Decrypt(encrypted, keyID, testAAD)
	require.Error(t, err, "decryption with a different key must fail")
	require.Contains(t, err.Error(), "credential_decryption_keys",
		"the error must tell the operator how to recover")
}

func TestKeyIDs_AreStableAndDistinct(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })

	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))
	first := xcrypto.PrimaryKeyID()

	require.NoError(t, xcrypto.Configure("another-different-master-key!"))
	second := xcrypto.PrimaryKeyID()

	require.NotEqual(t, first, second)

	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))
	require.Equal(t, first, xcrypto.PrimaryKeyID(), "key ids must be deterministic")
}

// TestKeyRotation covers the sequence an operator follows to replace a key:
// the superseded key moves to the decryption list while rows are rewritten.
func TestKeyRotation(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })

	const (
		oldKey = "the-original-master-key-value"
		newKey = "the-replacement-master-key!!!"
	)

	require.NoError(t, xcrypto.Configure(oldKey))

	legacy, legacyKeyID, err := xcrypto.Encrypt([]byte("secret"), testAAD)
	require.NoError(t, err)

	// New primary, old key retained for reads.
	require.NoError(t, xcrypto.Configure(newKey, oldKey))
	require.NotEqual(t, legacyKeyID, xcrypto.PrimaryKeyID())

	decrypted, err := xcrypto.Decrypt(legacy, legacyKeyID, testAAD)
	require.NoError(t, err)
	require.Equal(t, []byte("secret"), decrypted)

	// Rewriting the value moves it onto the new key.
	rewritten, rewrittenKeyID, err := xcrypto.Encrypt(decrypted, testAAD)
	require.NoError(t, err)
	require.Equal(t, xcrypto.PrimaryKeyID(), rewrittenKeyID)

	// Once nothing references the old key it can be dropped.
	require.NoError(t, xcrypto.Configure(newKey))

	decrypted, err = xcrypto.Decrypt(rewritten, rewrittenKeyID, testAAD)
	require.NoError(t, err)
	require.Equal(t, []byte("secret"), decrypted)

	_, err = xcrypto.Decrypt(legacy, legacyKeyID, testAAD)
	require.Error(t, err, "values under a dropped key must fail loudly, not silently")
}

func TestDisabled_Errors(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure(""))

	_, _, err := xcrypto.Encrypt([]byte("secret"), testAAD)
	require.Error(t, err)

	_, err = xcrypto.Decrypt("Zm9v", "", testAAD)
	require.Error(t, err)
}

func TestDecrypt_MalformedPayload(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	_, err := xcrypto.Decrypt("not-base64!!!", xcrypto.PrimaryKeyID(), testAAD)
	require.Error(t, err)

	_, err = xcrypto.Decrypt("Zm9v", xcrypto.PrimaryKeyID(), testAAD) // valid base64, shorter than a nonce
	require.Error(t, err)
}
