package storage

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/nepthass/nepthass/migrations"
)

// v030FS is the schema a v0.3.0 build ships: migrations 0001 and 0002 only.
func v030FS(t *testing.T) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	for _, name := range []string{"0001_foundation.sql", "0002_interactive_setup.sql"} {
		b, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		m[name] = &fstest.MapFile{Data: b}
	}
	return m
}

func TestMigrate_upgradesAV030Database_to0003_keepingData_survivesARestart_andARollbackBuildIsRefused(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	if err := Migrate(ctx, db, v030FS(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO command_audit_logs (occurred_at, actor_id, command, action, outcome, detail) VALUES ('2026-10-01T00:00:00.000000Z','1','permissions','allow','success','kept')"); err != nil {
		t.Fatal(err)
	}
	var hasNotifications int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name IN ('notification_settings','notification_rules')").Scan(&hasNotifications); err != nil || hasNotifications != 0 {
		t.Fatalf("a v0.3.0 database has no notification tables: %d %v", hasNotifications, err)
	}

	// The v0.4.0 build starts: migration 0003 is applied on top.
	if err := Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	var applied, settings, rules, audits int
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied)
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM notification_settings").Scan(&settings)
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM notification_rules").Scan(&rules)
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM command_audit_logs WHERE detail = 'kept'").Scan(&audits)
	if applied != 3 || settings != 0 || rules != 0 || audits != 1 {
		t.Fatalf("migrations=%d settings=%d rules=%d (no settings row means notifications are disabled) audit rows kept=%d", applied, settings, rules, audits)
	}

	// A restart re-runs Migrate and changes nothing.
	if err := Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("restart: %v", err)
	}
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&applied)
	if applied != 3 {
		t.Errorf("a restart must not re-apply or add migrations: %d", applied)
	}

	// Rolling back to the v0.3.0 build is refused, not silently run on a newer schema.
	if err := Migrate(ctx, db, v030FS(t)); !errors.Is(err, ErrSchemaNewer) {
		t.Errorf("a v0.3.0 build must refuse a database that has migration 0003: %v", err)
	}
	_ = db.QueryRowContext(ctx, "SELECT count(*) FROM command_audit_logs WHERE detail = 'kept'").Scan(&audits)
	if audits != 1 {
		t.Error("a refused rollback must leave the data untouched")
	}
}
