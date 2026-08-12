package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/conf"
	"github.com/looplj/axonhub/internal/server"
)

func validConfig() conf.Config {
	config := conf.Config{}
	config.APIServer.Port = 8090
	config.DB.DSN = "file:axonhub.db?cache=shared&_fk=1"
	config.Log.Name = "axonhub"

	return config
}

func TestValidateConfigAcceptsAMinimalConfig(t *testing.T) {
	require.Empty(t, validateConfig(validConfig()))
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*conf.Config)
		problem string
	}{
		{
			name:    "port out of range",
			mutate:  func(c *conf.Config) { c.APIServer.Port = 70000 },
			problem: "server.port must be between 1 and 65535",
		},
		{
			name:    "port unset",
			mutate:  func(c *conf.Config) { c.APIServer.Port = 0 },
			problem: "server.port must be between 1 and 65535",
		},
		{
			name:    "empty dsn",
			mutate:  func(c *conf.Config) { c.DB.DSN = "" },
			problem: "db.dsn cannot be empty",
		},
		{
			name:    "empty log name",
			mutate:  func(c *conf.Config) { c.Log.Name = "" },
			problem: "log.name cannot be empty",
		},
		{
			name: "cors enabled without origins",
			mutate: func(c *conf.Config) {
				c.APIServer.CORS = server.CORS{Enabled: true}
			},
			problem: "server.cors.allowed_origins cannot be empty when CORS is enabled",
		},
		{
			name: "wildcard origin with credentials",
			mutate: func(c *conf.Config) {
				c.APIServer.CORS = server.CORS{
					Enabled:          true,
					AllowedOrigins:   []string{"https://app.example", "*"},
					AllowCredentials: true,
				}
			},
			problem: `server.cors.allowed_origins must not contain "*" when server.cors.allow_credentials is true; list explicit origins instead`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validConfig()
			tt.mutate(&config)

			require.Contains(t, validateConfig(config), tt.problem)
		})
	}
}

// The wildcard is only rejected together with credentials: it is the normal way
// to run a public gateway without cookie auth.
func TestValidateConfigAllowsWildcardOriginWithoutCredentials(t *testing.T) {
	config := validConfig()
	config.APIServer.CORS = server.CORS{
		Enabled:        true,
		AllowedOrigins: []string{"*"},
	}

	require.Empty(t, validateConfig(config))
}
