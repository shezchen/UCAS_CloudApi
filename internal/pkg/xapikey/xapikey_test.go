package xapikey_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/pkg/xapikey"
)

func TestHash(t *testing.T) {
	// Deterministic, key-shaped output (lowercase hex sha-256).
	require.Equal(t,
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		xapikey.Hash("test"),
	)
	require.Equal(t, xapikey.Hash("ah-abc"), xapikey.Hash("ah-abc"))
	require.NotEqual(t, xapikey.Hash("ah-abc"), xapikey.Hash("ah-abd"))
}

func TestPrefix(t *testing.T) {
	require.Equal(t, "ah-1a2b3c4d5", xapikey.Prefix("ah-1a2b3c4d5e6f7a8b"))
	require.Equal(t, "short", xapikey.Prefix("short"))
}

func TestRedact(t *testing.T) {
	require.Equal(t, "ah-1a2b3c4d5...9f0e", xapikey.Redact("ah-1a2b3c4d5e6f7a8b9f0e"))
	// Too short to keep a distinct suffix: prefix + marker only.
	require.Equal(t, "short...", xapikey.Redact("short"))
}

func TestIsRedacted(t *testing.T) {
	require.True(t, xapikey.IsRedacted(xapikey.Redact("ah-1a2b3c4d5e6f7a8b9f0e")))
	require.True(t, xapikey.IsRedacted(xapikey.Redact("short")))
	require.False(t, xapikey.IsRedacted("ah-1a2b3c4d5e6f7a8b9f0e"))
	require.False(t, xapikey.IsRedacted(""))
}
