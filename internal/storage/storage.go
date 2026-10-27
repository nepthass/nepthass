// Package storage opens the SQLite database and applies schema migrations.
package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/nepthass/nepthass/migrations"
)

// TimeFormat is the fixed-width UTC layout used for every stored timestamp.
// Fixed width keeps lexicographic order identical to chronological order.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// FormatTime renders t in the storage layout (always UTC).
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime reads a timestamp written by FormatTime.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeFormat, s) }

const maxOpenConns = 4

// Open creates the database file if needed (owner-only permissions), applies
// all pending migrations and returns a ready connection pool.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	// Pre-create the file so it is owner-only; SQLite gives the -wal and -shm
	// companions the same permissions.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close database file: %w", err)
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := Migrate(ctx, db, migrations.FS); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// dsn builds a SQLite URI that enables foreign keys, WAL journaling, a busy
// timeout and immediate write transactions for every pooled connection.
func dsn(path string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
	q := url.Values{}
	for _, p := range []string{
		"foreign_keys(1)", "journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)",
	} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	return "file:" + escaped + "?" + q.Encode()
}
