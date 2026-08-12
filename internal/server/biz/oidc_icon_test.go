package biz

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// chdirTemp moves into a fresh working directory for the duration of the test.
func chdirTemp(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	// t.TempDir can sit behind a symlink (/tmp -> /private/tmp), which is the
	// case verifyPathInsideWorkingDir has to tolerate rather than reject.
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	t.Chdir(resolved)

	return resolved
}

func TestResolveIconURL_PassesThroughRemoteAndInlineValues(t *testing.T) {
	for _, raw := range []string{
		"",
		"https://example.com/icon.png",
		"http://example.com/icon.png",
		"data:image/png;base64,AAAA",
	} {
		resolved, err := resolveIconURL(raw)
		require.NoError(t, err)
		require.Equal(t, raw, resolved)
	}
}

func TestResolveIconURL_ReadsRelativeFile(t *testing.T) {
	chdirTemp(t)

	require.NoError(t, os.WriteFile("icon.png", []byte("icon-bytes"), 0o600))

	resolved, err := resolveIconURL("icon.png")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(resolved, "data:"))
	require.Contains(t, resolved, base64.StdEncoding.EncodeToString([]byte("icon-bytes")))
}

func TestResolveIconURL_RejectsPathsOutsideWorkingDir(t *testing.T) {
	root := chdirTemp(t)

	outside := filepath.Join(filepath.Dir(root), "outside-secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))

	// A symlink with no ".." segment survives filepath.Clean, so only symlink
	// resolution can catch it.
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "linked.png")))
	require.NoError(t, os.Symlink(filepath.Dir(root), filepath.Join(root, "up")))

	tests := []struct {
		name string
		raw  string
	}{
		{name: "absolute path", raw: outside},
		{name: "parent traversal", raw: "../outside-secret"},
		{name: "symlink to file outside", raw: "linked.png"},
		{name: "path through symlinked directory", raw: "up/outside-secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := resolveIconURL(tt.raw)
			require.Error(t, err)
			require.Empty(t, resolved)
			require.NotContains(t, err.Error(), "secret-bytes")
		})
	}
}
