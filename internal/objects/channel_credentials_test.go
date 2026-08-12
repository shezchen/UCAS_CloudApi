package objects_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcrypto"
)

func TestChannelCredentials_JSON_PlaintextWhenDisabled(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure(""))

	creds := objects.ChannelCredentials{
		APIKey:  "sk-plain-secret",
		APIKeys: []string{"sk-a", "sk-b"},
	}

	data, err := json.Marshal(creds)
	require.NoError(t, err)
	require.Contains(t, string(data), "sk-plain-secret")
	require.NotContains(t, string(data), "__axonhub_enc")

	var decoded objects.ChannelCredentials
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, "sk-plain-secret", decoded.APIKey)
	require.Equal(t, []string{"sk-a", "sk-b"}, decoded.APIKeys)
	require.False(t, decoded.StoredEncrypted())
}

func TestChannelCredentials_JSON_EncryptedRoundTrip(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	creds := objects.ChannelCredentials{
		APIKey: "sk-encrypted-secret",
		OAuth:  &objects.OAuthCredentials{AccessToken: "oauth-token"},
	}

	data, err := json.Marshal(creds)
	require.NoError(t, err)
	require.NotContains(t, string(data), "sk-encrypted-secret")
	require.NotContains(t, string(data), "oauth-token")
	require.Contains(t, string(data), "__axonhub_enc")

	require.Contains(t, string(data), xcrypto.PrimaryKeyID(),
		"the envelope must record which key produced it")

	var decoded objects.ChannelCredentials
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, "sk-encrypted-secret", decoded.APIKey)
	require.NotNil(t, decoded.OAuth)
	require.Equal(t, "oauth-token", decoded.OAuth.AccessToken)
	require.True(t, decoded.StoredEncrypted())
	require.Equal(t, xcrypto.PrimaryKeyID(), decoded.StoredKeyID())
}

// TestChannelCredentials_JSON_SupersededKeyRoundTrip covers a rotation window:
// the row was written under the previous key, which is still configured for
// decryption while the backfill rewrites it.
func TestChannelCredentials_JSON_SupersededKeyRoundTrip(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })

	const (
		oldKey = "the-original-master-key-value"
		newKey = "the-replacement-master-key!!!"
	)

	require.NoError(t, xcrypto.Configure(oldKey))
	oldKeyID := xcrypto.PrimaryKeyID()

	data, err := json.Marshal(objects.ChannelCredentials{APIKey: "sk-under-old-key"})
	require.NoError(t, err)

	require.NoError(t, xcrypto.Configure(newKey, oldKey))

	var decoded objects.ChannelCredentials
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, "sk-under-old-key", decoded.APIKey)
	require.Equal(t, oldKeyID, decoded.StoredKeyID())
	require.NotEqual(t, xcrypto.PrimaryKeyID(), decoded.StoredKeyID(),
		"a value under a superseded key must be identifiable so it can be rewritten")

	// Re-serializing moves it onto the primary key.
	rewritten, err := json.Marshal(decoded)
	require.NoError(t, err)

	var reloaded objects.ChannelCredentials
	require.NoError(t, json.Unmarshal(rewritten, &reloaded))
	require.Equal(t, xcrypto.PrimaryKeyID(), reloaded.StoredKeyID())
}

// TestChannelCredentials_JSON_UnknownKeyIDFailsLoudly guards the case an
// operator hits after dropping a key that rows still reference.
func TestChannelCredentials_JSON_UnknownKeyIDFailsLoudly(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })

	require.NoError(t, xcrypto.Configure("the-original-master-key-value"))

	data, err := json.Marshal(objects.ChannelCredentials{APIKey: "sk-secret"})
	require.NoError(t, err)

	require.NoError(t, xcrypto.Configure("the-replacement-master-key!!!"))

	var decoded objects.ChannelCredentials
	err = json.Unmarshal(data, &decoded)
	require.Error(t, err)
	require.Contains(t, err.Error(), "credential_decryption_keys",
		"the error must name the config that recovers the value")
}

func TestChannelCredentials_JSON_LegacyPlaintextStillLoadsWithKey(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	// Rows written before the upgrade hold plain JSON; they must keep loading
	// after a key is configured (the startup backfill re-encrypts them).
	var decoded objects.ChannelCredentials
	require.NoError(t, json.Unmarshal([]byte(`{"apiKey":"sk-legacy"}`), &decoded))
	require.Equal(t, "sk-legacy", decoded.APIKey)
	require.False(t, decoded.StoredEncrypted())
}

func TestChannelCredentials_JSON_EncryptedWithoutKeyFails(t *testing.T) {
	t.Cleanup(func() { _ = xcrypto.Configure("") })
	require.NoError(t, xcrypto.Configure("a-sufficiently-long-master-key"))

	data, err := json.Marshal(objects.ChannelCredentials{APIKey: "sk-secret"})
	require.NoError(t, err)

	require.NoError(t, xcrypto.Configure(""))

	var decoded objects.ChannelCredentials
	err = json.Unmarshal(data, &decoded)
	require.Error(t, err, "encrypted credentials must not silently load without the key")
	require.Contains(t, err.Error(), "credential_encryption_key")
}
