package db

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/migrate/schemahook"
)

func TestPasswordResetSchemaMigrationFromUCAS14(t *testing.T) {
	ctx := context.Background()
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", filepath.ToSlash(filepath.Join(t.TempDir(), "ucas14.db")))
	_, sqlDB, err := openDB("sqlite3", dsn, 1, 1, 0, 0)
	if err != nil {
		t.Fatalf("open legacy SQLite database: %v", err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close migrated database: %v", err)
		}
	})

	legacyStatements := []string{
		`CREATE TABLE users (
			id integer NOT NULL PRIMARY KEY AUTOINCREMENT,
			created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at integer NOT NULL DEFAULT 0,
			email varchar(255) NOT NULL,
			status varchar(11) NOT NULL DEFAULT 'activated',
			prefer_language varchar(255) NOT NULL DEFAULT 'en',
			password varchar(255) NOT NULL,
			nickname varchar(24) NOT NULL DEFAULT '',
			first_name varchar(255) NOT NULL DEFAULT '',
			last_name varchar(255) NOT NULL DEFAULT '',
			avatar varchar(255) NULL,
			is_owner bool NOT NULL DEFAULT false,
			daily_token_limit integer NOT NULL DEFAULT 200000000,
			scopes json NULL
		)`,
		`CREATE UNIQUE INDEX user_email_deleted_at ON users (email, deleted_at)`,
		`CREATE TABLE email_verification_challenges (
			id integer NOT NULL PRIMARY KEY AUTOINCREMENT,
			created_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
			email varchar(320) NOT NULL,
			code_digest varchar(64) NOT NULL,
			source_hash varchar(64) NOT NULL,
			expires_at datetime NOT NULL,
			attempts integer NOT NULL DEFAULT 0,
			consumed_at datetime NULL
		)`,
		`CREATE INDEX emailverificationchallenge_email_created_at ON email_verification_challenges (email, created_at)`,
		`CREATE INDEX emailverificationchallenge_source_hash_created_at ON email_verification_challenges (source_hash, created_at)`,
		`CREATE INDEX emailverificationchallenge_expires_at ON email_verification_challenges (expires_at)`,
		`CREATE INDEX emailverificationchallenge_email_consumed_at_expires_at ON email_verification_challenges (email, consumed_at, expires_at)`,
	}
	for _, statement := range legacyStatements {
		if _, err := sqlDB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create UCAS14 fixture schema: %v", err)
		}
	}

	createdAt := time.Date(2026, time.August, 12, 8, 30, 0, 0, time.UTC)
	expiresAt := createdAt.Add(10 * time.Minute)
	if _, err := sqlDB.ExecContext(ctx, `
		INSERT INTO users (
			id, created_at, updated_at, deleted_at, email, status, prefer_language,
			password, nickname, first_name, last_name, is_owner, daily_token_limit, scopes
		) VALUES (?, ?, ?, 0, ?, 'activated', 'zh-CN', ?, ?, '', '', true, ?, ?)`,
		17, createdAt, createdAt, "legacy-owner@example.com", "legacy-password-hash",
		"旧版用户", int64(200_000_000), `["read_channels"]`); err != nil {
		t.Fatalf("insert UCAS14 user: %v", err)
	}
	codeDigest := strings.Repeat("a", 64)
	sourceHash := strings.Repeat("b", 64)
	if _, err := sqlDB.ExecContext(ctx, `
		INSERT INTO email_verification_challenges (
			id, created_at, updated_at, email, code_digest, source_hash,
			expires_at, attempts, consumed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 2, NULL)`,
		23, createdAt, createdAt, "legacy@mails.ucas.ac.cn", codeDigest, sourceHash, expiresAt); err != nil {
		t.Fatalf("insert UCAS14 verification challenge: %v", err)
	}

	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, sqlDB)))
	if err := client.Schema.Create(
		ctx,
		migrate.WithGlobalUniqueID(false),
		migrate.WithForeignKeys(false),
		migrate.WithDropIndex(true),
		migrate.WithDropColumn(true),
		schema.WithHooks(schemahook.V0_3_0),
	); err != nil {
		t.Fatalf("migrate UCAS14 schema to current schema: %v", err)
	}

	var (
		userEmail    string
		passwordHash string
		nickname     string
		authVersion  int64
	)
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT email, password, nickname, auth_version FROM users WHERE id = ?`, 17).
		Scan(&userEmail, &passwordHash, &nickname, &authVersion); err != nil {
		t.Fatalf("read migrated user: %v", err)
	}
	if userEmail != "legacy-owner@example.com" || passwordHash != "legacy-password-hash" || nickname != "旧版用户" {
		t.Fatalf("migrated user data changed: email=%q password=%q nickname=%q", userEmail, passwordHash, nickname)
	}
	if authVersion != 0 {
		t.Fatalf("migrated auth_version = %d, want 0", authVersion)
	}

	var (
		challengeEmail  string
		migratedCode    string
		migratedSource  string
		purpose         string
		attempts        int
		migratedExpires time.Time
	)
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT email, code_digest, source_hash, purpose, attempts, expires_at
		FROM email_verification_challenges WHERE id = ?`, 23).
		Scan(&challengeEmail, &migratedCode, &migratedSource, &purpose, &attempts, &migratedExpires); err != nil {
		t.Fatalf("read migrated verification challenge: %v", err)
	}
	if challengeEmail != "legacy@mails.ucas.ac.cn" || migratedCode != codeDigest || migratedSource != sourceHash || attempts != 2 {
		t.Fatalf("migrated challenge data changed: email=%q code=%q source=%q attempts=%d", challengeEmail, migratedCode, migratedSource, attempts)
	}
	if purpose != "registration" {
		t.Fatalf("migrated purpose = %q, want registration", purpose)
	}
	if !migratedExpires.Equal(expiresAt) {
		t.Fatalf("migrated expires_at = %s, want %s", migratedExpires, expiresAt)
	}

	var quickCheck string
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck); err != nil {
		t.Fatalf("run SQLite quick_check: %v", err)
	}
	if quickCheck != "ok" {
		t.Fatalf("SQLite quick_check = %q, want ok", quickCheck)
	}

	foreignKeyRows, err := sqlDB.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("run SQLite foreign_key_check: %v", err)
	}
	defer foreignKeyRows.Close()
	if foreignKeyRows.Next() {
		t.Fatal("SQLite foreign_key_check reported a violation")
	}
	if err := foreignKeyRows.Err(); err != nil {
		t.Fatalf("read SQLite foreign_key_check: %v", err)
	}
}
