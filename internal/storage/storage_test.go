package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"
	"time"
)

func openTemp(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sub", "nepthass.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpen_createsSchemaWithForeignKeysAndWAL(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	var fk int
	var journal string
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE name IN ('entity_permissions','command_audit_logs','schema_migrations')",
	).Scan(&tables); err != nil {
		t.Fatal(err)
	}

	if fk != 1 || journal != "wal" || tables != 3 {
		t.Errorf("foreign_keys=%d journal_mode=%s tables=%d", fk, journal, tables)
	}
}

func TestOpen_createsOwnerOnlyDatabaseFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	_, path := openTemp(t)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("database permissions = %o, want 600", perm)
	}
}

func TestOpen_isIdempotent_whenReopenedWithSameMigrations(t *testing.T) {
	_, path := openTemp(t)

	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	defer db.Close()

	var n int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", n)
	}
}

func memDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrate_refusesDatabaseFromNewerBuild(t *testing.T) {
	db := memDB(t)
	ctx := context.Background()
	newer := fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER) STRICT;")},
		"0002_b.sql": {Data: []byte("CREATE TABLE b (id INTEGER) STRICT;")},
	}
	if err := Migrate(ctx, db, newer); err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{"0001_a.sql": newer["0001_a.sql"]}

	err := Migrate(ctx, db, older)

	if !errors.Is(err, ErrSchemaNewer) {
		t.Errorf("error = %v, want ErrSchemaNewer", err)
	}
}

func TestMigrate_detectsEditedMigration(t *testing.T) {
	db := memDB(t)
	ctx := context.Background()
	original := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER) STRICT;")}}
	if err := Migrate(ctx, db, original); err != nil {
		t.Fatal(err)
	}
	edited := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER, extra TEXT) STRICT;")}}

	err := Migrate(ctx, db, edited)

	if !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("error = %v, want ErrChecksumMismatch", err)
	}
}

func TestMigrate_rollsBackFailedMigration_andKeepsEarlierOnes(t *testing.T) {
	db := memDB(t)
	ctx := context.Background()
	set := fstest.MapFS{
		"0001_ok.sql":  {Data: []byte("CREATE TABLE ok (id INTEGER) STRICT;")},
		"0002_bad.sql": {Data: []byte("CREATE TABLE half (id INTEGER) STRICT; INSERT INTO missing_table VALUES (1);")},
	}

	if err := Migrate(ctx, db, set); err == nil {
		t.Fatal("Migrate() succeeded with a broken migration")
	}

	var versions, half int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'half'").Scan(&half); err != nil {
		t.Fatal(err)
	}
	if versions != 1 || half != 0 {
		t.Errorf("versions=%d half-created tables=%d, want 1 and 0", versions, half)
	}
}

func TestMigrate_rejectsMalformedMigrationSets(t *testing.T) {
	tests := map[string]fstest.MapFS{
		"bad file name": {"init.sql": {Data: []byte("SELECT 1;")}},
		"gap in sequence": {
			"0001_a.sql": {Data: []byte("SELECT 1;")},
			"0003_c.sql": {Data: []byte("SELECT 1;")},
		},
		"does not start at one": {"0002_b.sql": {Data: []byte("SELECT 1;")}},
	}
	for name, set := range tests {
		t.Run(name, func(t *testing.T) {
			err := Migrate(context.Background(), memDB(t), set)

			if !errors.Is(err, ErrInvalidMigrations) {
				t.Errorf("error = %v, want ErrInvalidMigrations", err)
			}
		})
	}
}

func TestFormatTime_sortsChronologically(t *testing.T) {
	earlier, err := ParseTime("2026-01-02T03:04:05.100Z")
	if err != nil {
		t.Fatal(err)
	}
	later := earlier.Add(time.Millisecond)

	if FormatTime(earlier) != "2026-01-02T03:04:05.100Z" {
		t.Errorf("FormatTime() = %q", FormatTime(earlier))
	}
	if FormatTime(earlier) >= FormatTime(later) {
		t.Error("formatted timestamps do not sort chronologically")
	}
}
