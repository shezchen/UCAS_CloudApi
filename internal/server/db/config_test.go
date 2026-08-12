package db

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestRedactDSN(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "empty",
			dsn:  "",
			want: "",
		},
		{
			name: "sqlite has no credential",
			dsn:  "file:axonhub.db?cache=shared&_fk=1&_pragma=journal_mode(WAL)",
			want: "file:axonhub.db?cache=shared&_fk=1&_pragma=journal_mode(WAL)",
		},
		{
			name: "postgres url",
			dsn:  "postgres://axonhub:s3cr3t@db.example.com:5432/axonhub?sslmode=require",
			want: "postgres://axonhub:xxxxx@db.example.com:5432/axonhub?sslmode=require",
		},
		{
			name: "postgres url without password",
			dsn:  "postgres://axonhub@db.example.com:5432/axonhub?sslmode=require",
			want: "postgres://axonhub@db.example.com:5432/axonhub?sslmode=require",
		},
		{
			name: "mysql authority form",
			dsn:  "axonhub:s3cr3t@tcp(mysql:3306)/axonhub?charset=utf8mb4&parseTime=True&loc=Local",
			want: "axonhub:xxxxx@tcp(mysql:3306)/axonhub?charset=utf8mb4&parseTime=True&loc=Local",
		},
		{
			name: "mysql authority form without password",
			dsn:  "axonhub@tcp(mysql:3306)/axonhub",
			want: "axonhub@tcp(mysql:3306)/axonhub",
		},
		{
			name: "password containing an at sign",
			dsn:  "postgres://axonhub:p@ss@db.example.com:5432/axonhub",
			want: "postgres://axonhub:xxxxx@db.example.com:5432/axonhub",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, redactDSN(tt.dsn))
		})
	}
}

func TestConfigMarshalRedactsBothDSNs(t *testing.T) {
	cfg := Config{
		Dialect: "postgres",
		DSN:     "postgres://axonhub:primary-secret@primary.example.com:5432/axonhub",
		ReadReplica: ReadReplicaConfig{
			DSN: "postgres://axonhub:replica-secret@replica.example.com:5432/axonhub",
		},
	}

	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "primary-secret")
	require.NotContains(t, string(raw), "replica-secret")
	require.Contains(t, string(raw), "primary.example.com:5432/axonhub")
	require.Contains(t, string(raw), "replica.example.com:5432/axonhub")

	out, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NotContains(t, string(out), "primary-secret")
	require.NotContains(t, string(out), "replica-secret")
	require.Contains(t, string(out), "primary.example.com:5432/axonhub")

	// Marshalling must not mutate the receiver: the live config still has to
	// carry the real credential for the connection pool.
	require.Equal(t, "postgres://axonhub:primary-secret@primary.example.com:5432/axonhub", cfg.DSN)
	require.Equal(t, "postgres://axonhub:replica-secret@replica.example.com:5432/axonhub", cfg.ReadReplica.DSN)
}
