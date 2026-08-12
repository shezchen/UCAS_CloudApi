// Package xapikey provides the canonical hashing and display helpers for user
// API keys. Raw keys are never stored: the database keeps only the SHA-256
// hash (for lookup/verification) plus a redacted display form.
package xapikey

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	// prefixLength is how many leading characters of the raw key are kept for
	// display/identification (e.g. "ah-1a2b3c4d5").
	prefixLength = 12

	// suffixLength is how many trailing characters are kept in the redacted
	// display form.
	suffixLength = 4

	// redactionMarker separates prefix and suffix in the redacted display
	// form. Generated keys ("<prefix>-<64 hex>") never contain dots, so its
	// presence reliably identifies an already-redacted value.
	redactionMarker = "..."
)

// Hash returns the lowercase hex SHA-256 digest of the raw key.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Prefix returns the display prefix of the raw key.
func Prefix(raw string) string {
	if len(raw) <= prefixLength {
		return raw
	}

	return raw[:prefixLength]
}

// Redact returns the display form stored in place of the raw key,
// e.g. "ah-1a2b3c4d5...e5f6". Keys too short to keep a distinct suffix are
// truncated to the prefix plus the marker.
func Redact(raw string) string {
	if len(raw) <= prefixLength+suffixLength {
		return Prefix(raw) + redactionMarker
	}

	return Prefix(raw) + redactionMarker + raw[len(raw)-suffixLength:]
}

// IsRedacted reports whether the value is a redacted display form rather than
// a raw key.
func IsRedacted(value string) bool {
	return strings.Contains(value, redactionMarker)
}
