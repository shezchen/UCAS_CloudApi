package xcrypto_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/pkg/xcrypto"
)

func TestConfigure(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })

	require.NoError(t, xcrypto.Configure(""))
	require.False(t, xcrypto.Enabled())

	require.Error(t, xcrypto.Configure("too-short"), "keys below the minimum length must be rejected")
	require.False(t, xcrypto.Enabled())

	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))
	require.True(t, xcrypto.Enabled())

	// Reconfiguring with an empty key disables encryption again.
	require.NoError(t, xcrypto.Configure(""))
	require.False(t, xcrypto.Enabled())
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	plaintext := []byte(`{"apiKey":"sk-secret-value"}`)

	encrypted, err := xcrypto.Encrypt(plaintext)
	require.NoError(t, err)
	require.NotContains(t, encrypted, "sk-secret-value")

	decrypted, err := xcrypto.Decrypt(encrypted)
	require.NoError(t, err)
	require.Equal(t, plaintext, decrypted)

	// GCM uses a random nonce, so equal plaintexts produce distinct ciphertexts.
	encrypted2, err := xcrypto.Encrypt(plaintext)
	require.NoError(t, err)
	require.NotEqual(t, encrypted, encrypted2)
}

func TestDecrypt_WrongKey(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	encrypted, err := xcrypto.Encrypt([]byte("secret"))
	require.NoError(t, err)

	require.NoError(t, xcrypto.Configure("another-different-master-key!"))

	_, err = xcrypto.Decrypt(encrypted)
	require.Error(t, err, "decryption with a different key must fail authentication")
}

func TestDisabled_Errors(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure(""))

	_, err := xcrypto.Encrypt([]byte("secret"))
	require.Error(t, err)

	_, err = xcrypto.Decrypt("Zm9v")
	require.Error(t, err)
}

func TestDecrypt_MalformedPayload(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	_, err := xcrypto.Decrypt("not-base64!!!")
	require.Error(t, err)

	_, err = xcrypto.Decrypt("Zm9v") // valid base64, shorter than a nonce
	require.Error(t, err)
}
