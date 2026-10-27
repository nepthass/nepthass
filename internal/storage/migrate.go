package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"
)

var (
	// ErrSchemaNewer means the database was written by a newer NeptHass build.
	ErrSchemaNewer = errors.New("database schema is newer than this build supports")
	// ErrChecksumMismatch means an already-applied migration was edited.
	ErrChecksumMismatch = errors.New("applied migration was modified")
	// ErrInvalidMigrations means the embedded migration set is malformed.
	ErrInvalidMigrations = errors.New("invalid migration set")
)

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

// Migrate applies every migration in fsys that the database has not seen yet.
// Each migration runs in its own transaction. It refuses to touch a database
// that contains migrations unknown to this build or whose recorded checksum no
// longer matches, so an upgrade can never silently rewrite history.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	known, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		checksum   TEXT NOT NULL,
		applied_at TEXT NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	applied, err := appliedChecksums(ctx, db)
	if err != nil {
		return err
	}
	byVersion := make(map[int]migration, len(known))
	for _, m := range known {
		byVersion[m.version] = m
	}
	for version, sum := range applied {
		m, ok := byVersion[version]
		if !ok {
			return fmt.Errorf("%w: database has migration %04d", ErrSchemaNewer, version)
		}
		if m.checksum != sum {
			return fmt.Errorf("%w: %s", ErrChecksumMismatch, m.name)
		}
	}
	for _, m := range known {
		if _, done := applied[m.version]; done {
			continue
		}
		if err := apply(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(files)
	out := make([]migration, 0, len(files))
	for i, name := range files {
		match := migrationName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("%w: %q must be named NNNN_description.sql", ErrInvalidMigrations, name)
		}
		version, err := strconv.Atoi(match[1])
		if err != nil || version != i+1 {
			return nil, fmt.Errorf("%w: %q breaks the contiguous 0001.. sequence", ErrInvalidMigrations, name)
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{version: version, name: name, sql: string(body), checksum: hex.EncodeToString(sum[:])})
	}
	return out, nil
}

func appliedChecksums(ctx context.Context, db *sql.DB) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]string)
	for rows.Next() {
		var version int
		var sum string
		if err := rows.Scan(&version, &sum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = sum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

func apply(ctx context.Context, db *sql.DB, m migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
		m.version, m.name, m.checksum, FormatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("record migration %s: %w", m.name, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}
