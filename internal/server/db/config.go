package db

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

type Config struct {
	Dialect              string        `conf:"dialect" yaml:"dialect" json:"dialect"`
	DSN                  string        `conf:"dsn" yaml:"dsn" json:"dsn"`
	Debug                bool          `conf:"debug" yaml:"debug" json:"debug"`
	MaxOpenConns         int           `conf:"max_open_conns" yaml:"max_open_conns" json:"max_open_conns"`
	MaxIdleConns         int           `conf:"max_idle_conns" yaml:"max_idle_conns" json:"max_idle_conns"`
	ConnMaxLifetime      time.Duration `conf:"conn_max_lifetime" yaml:"conn_max_lifetime" json:"conn_max_lifetime"`
	ConnMaxIdleTime      time.Duration `conf:"conn_max_idle_time" yaml:"conn_max_idle_time" json:"conn_max_idle_time"`
	DisableSQLiteAutoWAL bool          `conf:"disable_sqlite_auto_wal" yaml:"disable_sqlite_auto_wal" json:"disable_sqlite_auto_wal"`
	DisableAutoMigration bool          `conf:"disable_auto_migration" yaml:"disable_auto_migration" json:"disable_auto_migration"`

	ReadReplica ReadReplicaConfig `conf:"read_replica" yaml:"read_replica" json:"read_replica"`
}

type ReadReplicaConfig struct {
	DSN          string `conf:"read_dsn" yaml:"read_dsn" json:"read_dsn"`
	MaxOpenConns int    `conf:"read_max_open_conns" yaml:"read_max_open_conns" json:"read_max_open_conns"`
	MaxIdleConns int    `conf:"read_max_idle_conns" yaml:"read_max_idle_conns" json:"read_max_idle_conns"`
}

// redactedDSNPassword is the placeholder url.URL.Redacted substitutes for the
// password; the fallback path below reuses it so both forms look the same.
const redactedDSNPassword = "xxxxx"

// serializableConfig drops the methods below so marshalling it does not recurse.
type serializableConfig Config

// MarshalJSON strips the password out of both DSNs. Configuration is only ever
// decoded through mapstructure (the `conf` tags), so nothing round-trips this
// struct through JSON and the redacted form cannot be loaded back by mistake.
// Host, port and database name are kept because the config debug log in
// conf.Load and `axonhub config preview` are the main tools for diagnosing
// connection problems.
func (c Config) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.redacted())
}

// MarshalYAML mirrors MarshalJSON for `axonhub config preview -f yaml`.
func (c Config) MarshalYAML() (any, error) {
	return c.redacted(), nil
}

func (c Config) redacted() serializableConfig {
	c.DSN = redactDSN(c.DSN)
	c.ReadReplica.DSN = redactDSN(c.ReadReplica.DSN)

	return serializableConfig(c)
}

// redactDSN replaces the password in a DSN with a placeholder. It handles both
// the URL form used by PostgreSQL ("postgres://user:pw@host/db") and the
// authority form used by the MySQL driver ("user:pw@tcp(host:3306)/db").
// SQLite DSNs carry no credential and are returned unchanged.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}

	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if _, ok := u.User.Password(); !ok {
			return dsn
		}

		return u.Redacted()
	}

	authority := dsn
	if slash := strings.IndexByte(dsn, '/'); slash >= 0 {
		authority = dsn[:slash]
	}

	at := strings.LastIndexByte(authority, '@')
	if at < 0 {
		return dsn
	}

	colon := strings.IndexByte(authority[:at], ':')
	if colon < 0 {
		return dsn
	}

	return dsn[:colon+1] + redactedDSNPassword + dsn[at:]
}
