package conf

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadSMTPFromEnvironmentWithoutLeakingPassword(t *testing.T) {
	t.Setenv("AXONHUB_SMTP_HOST", "smtp.example.com")
	t.Setenv("AXONHUB_SMTP_PORT", "465")
	t.Setenv("AXONHUB_SMTP_USERNAME", "campus@example.com")
	t.Setenv("AXONHUB_SMTP_PASSWORD", "smtp-test-secret")
	t.Setenv("AXONHUB_SMTP_FROM", "campus@example.com")
	t.Setenv("AXONHUB_SMTP_TLS_MODE", "tls")

	config, err := Load()
	require.NoError(t, err)
	require.Equal(t, "smtp.example.com", config.SMTP.Host)
	require.Equal(t, 465, config.SMTP.Port)
	require.Equal(t, "campus@example.com", config.SMTP.Username)
	require.Equal(t, "smtp-test-secret", config.SMTP.Password)
	require.Equal(t, "campus@example.com", config.SMTP.From)
	require.Equal(t, "tls", string(config.SMTP.TLSMode))

	raw, err := json.Marshal(config)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "smtp-test-secret")
}
