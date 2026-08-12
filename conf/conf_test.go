package conf

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/looplj/axonhub/internal/server/biz"
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

// The loaded config is written verbatim to the debug log in Load and to
// `axonhub config preview`, so every credential it carries has to survive a
// round of json.Marshal / yaml.Marshal without appearing in the output.

func TestLoadRedisPasswordsWithoutLeakingThem(t *testing.T) {
	t.Setenv("AXONHUB_CACHE_MODE", "redis")
	t.Setenv("AXONHUB_CACHE_REDIS_ADDRS", "redis.example.com:6379")
	t.Setenv("AXONHUB_CACHE_REDIS_PASSWORD", "redis-test-secret")
	t.Setenv("AXONHUB_CACHE_REDIS_SENTINEL_PASSWORD", "sentinel-test-secret")

	config, err := Load()
	require.NoError(t, err)
	require.Equal(t, "redis-test-secret", config.Cache.Redis.Password)
	require.Equal(t, "sentinel-test-secret", config.Cache.Redis.SentinelPassword)

	requireNotSerialized(t, config, "redis-test-secret", "sentinel-test-secret")
}

func TestLoadDatabaseDSNWithoutLeakingPassword(t *testing.T) {
	t.Setenv("AXONHUB_DB_DIALECT", "postgres")
	t.Setenv("AXONHUB_DB_DSN", "postgres://axonhub:db-test-secret@db.example.com:5432/axonhub?sslmode=require")

	config, err := Load()
	require.NoError(t, err)
	require.Equal(t,
		"postgres://axonhub:db-test-secret@db.example.com:5432/axonhub?sslmode=require",
		config.DB.DSN,
		"the live config must keep the real DSN for the connection pool",
	)

	raw := requireNotSerialized(t, config, "db-test-secret")
	require.Contains(t, raw, "db.example.com:5432/axonhub",
		"host and database name stay readable for diagnostics")
}

func TestOIDCClientSecretIsNotSerialized(t *testing.T) {
	config := Config{
		OIDC: biz.OIDCConfig{
			Providers: []biz.OIDCProvider{
				{
					ID:           "campus",
					IssuerURL:    "https://idp.example.com",
					ClientID:     "axonhub",
					ClientSecret: "oidc-test-secret",
				},
			},
		},
	}

	raw := requireNotSerialized(t, config, "oidc-test-secret")
	require.Contains(t, raw, "https://idp.example.com")
}

// requireNotSerialized asserts that none of the secrets show up in either
// serialization of the config, and returns the JSON form for further checks.
func requireNotSerialized(t *testing.T, config Config, secrets ...string) string {
	t.Helper()

	raw, err := json.Marshal(config)
	require.NoError(t, err)

	out, err := yaml.Marshal(config)
	require.NoError(t, err)

	for _, secret := range secrets {
		require.NotContains(t, string(raw), secret)
		require.NotContains(t, string(out), secret)
	}

	return string(raw)
}
