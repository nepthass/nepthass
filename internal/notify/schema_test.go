package notify

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/nepthass/nepthass/internal/storage"
	"github.com/nepthass/nepthass/migrations"
)

func openUnmigrated(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "old.db"))+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigration_upgradesAV030Database_keepingEverything_andStartingDisabled(t *testing.T) {
	ctx := context.Background()
	db := openUnmigrated(t)
	v030 := fstest.MapFS{}
	for _, name := range []string{"0001_foundation.sql", "0002_interactive_setup.sql"} {
		data, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		v030[name] = &fstest.MapFile{Data: data}
	}
	if err := storage.Migrate(ctx, db, v030); err != nil {
		t.Fatalf("install the v0.3.0 schema: %v", err)
	}
	for _, q := range []string{
		"INSERT INTO entity_permissions (entity_id, access, granted_by, updated_at) VALUES ('binary_sensor.door', 'read', 'admin', '2026-10-27T10:00:00.000Z')",
		"INSERT INTO entity_aliases (entity_id, alias, alias_key, updated_by, updated_at) VALUES ('binary_sensor.door', 'Front door', 'front door', 'admin', '2026-10-27T10:00:00.000Z')",
		"INSERT INTO command_audit_logs (occurred_at, actor_id, command, entity_id, action, outcome, detail) VALUES ('2026-10-27T10:00:00.000Z', 'admin', 'permissions.allow', 'binary_sensor.door', 'read', 'success', '')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := checksums(t, db)

	if err := storage.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("upgrade to the current schema: %v", err)
	}
	after := checksums(t, db)
	for v, sum := range before {
		if after[v] != sum {
			t.Errorf("migration %d was altered by the upgrade", v)
		}
	}
	if len(after) != len(before)+1 || after[3] == "" {
		t.Errorf("exactly migration 3 must have been added: before=%d after=%d", len(before), len(after))
	}
	for table, want := range map[string]int{"entity_permissions": 1, "entity_aliases": 1, "command_audit_logs": 1, "notification_settings": 0, "notification_rules": 0} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if n != want {
			t.Errorf("%s has %d rows, want %d", table, n, want)
		}
	}
	// Running the migrations again changes nothing.
	if err := storage.Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, db: db}
	svc := NewService(Deps{GuildID: testGuild, DB: db, Store: NewStore(db), Permissions: f.perms, Sensors: nil})
	cfg, err := svc.Config(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Configured || cfg.Active() || cfg.Rules != 0 {
		t.Errorf("an upgraded database must have notifications disabled: %+v", cfg)
	}
}

func checksums(t *testing.T, db *sql.DB) map[int]string {
	t.Helper()
	rows, err := db.Query("SELECT version, checksum FROM schema_migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			t.Fatal(err)
		}
		out[v] = sum
	}
	return out
}

func TestSchema_settingsConstraintsRejectInvalidRows(t *testing.T) {
	f := newFixture(t)
	insert := func(guild, channel string, enabled int, by string) error {
		_, err := f.db.Exec("INSERT INTO notification_settings (guild_id, channel_id, enabled, updated_by, updated_at) VALUES (?, ?, ?, ?, 't')", guild, channel, enabled, by)
		return err
	}
	bad := map[string][4]any{
		"guild not numeric":    {"abc123456", testChannel, 1, "a"},
		"guild too short":      {"1234", testChannel, 1, "a"},
		"channel not numeric":  {testGuild, "12x45", 1, "a"},
		"channel empty":        {testGuild, "", 1, "a"},
		"enabled out of range": {testGuild, testChannel, 2, "a"},
		"updated_by empty":     {testGuild, testChannel, 1, ""},
	}
	for name, v := range bad {
		if err := insert(v[0].(string), v[1].(string), v[2].(int), v[3].(string)); err == nil {
			t.Errorf("%s: the database accepted an invalid row", name)
		}
	}
	if err := insert(testGuild, testChannel, 1, "a"); err != nil {
		t.Fatalf("a valid row: %v", err)
	}
	if err := insert(testGuild, "300000000000000003", 1, "a"); err == nil {
		t.Error("one settings row per guild")
	}
}

func TestSchema_rulesConstraints_uniquenessCooldownSemantics_andNoForeignKey(t *testing.T) {
	f := newFixture(t)
	insert := func(entity, kind string, threshold any, cooldown any) error {
		_, err := f.db.Exec(`INSERT INTO notification_rules (entity_id, kind, threshold, cooldown_seconds, created_by, created_at, updated_by, updated_at)
VALUES (?, ?, ?, ?, 'a', '2026-10-30T12:00:00.000Z', 'a', '2026-10-30T12:00:00.000Z')`, entity, kind, threshold, cooldown)
		return err
	}
	rejected := map[string]error{
		"unknown kind":              insert("sensor.a", "between", nil, nil),
		"state with a threshold":    insert("sensor.a", "state", 1.0, nil),
		"above without a threshold": insert("sensor.a", "above", nil, nil),
		"below without a threshold": insert("sensor.a", "below", nil, nil),
		"NaN becomes NULL":          insert("sensor.a", "above", math.NaN(), nil),
		"infinite threshold":        insert("sensor.a", "above", math.Inf(1), nil),
		"huge threshold":            insert("sensor.a", "above", 2e6, nil),
		"negative cooldown":         insert("sensor.a", "state", nil, -1),
		"cooldown over a day":       insert("sensor.a", "state", nil, 86401),
		"entity id too short":       insert("ab", "state", nil, nil),
	}
	for name, err := range rejected {
		if err == nil {
			t.Errorf("%s: the database accepted an invalid rule", name)
		}
	}
	if err := insert("sensor.a", "above", 28.5, nil); err != nil {
		t.Fatalf("valid above rule: %v", err)
	}
	if err := insert("sensor.a", "above", 30.0, nil); err == nil {
		t.Error("a second rule of the same kind for one entity must be rejected")
	}
	if err := insert("sensor.a", "below", 5.0, 0); err != nil {
		t.Fatalf("a different kind for the same entity is allowed: %v", err)
	}
	if err := insert("binary_sensor.d", "state", nil, 86400); err != nil {
		t.Fatalf("a one day cooldown is allowed: %v", err)
	}

	// NULL means "category default"; zero means "no cooldown": they must stay distinct.
	rules, err := f.store.ListRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]Rule{}
	for _, r := range rules {
		byKind[string(r.Entity)+"/"+string(r.Kind)] = r
	}
	if r := byKind["sensor.a/above"]; r.Cooldown != nil {
		t.Errorf("NULL cooldown must read back as nil (category default), got %v", *r.Cooldown)
	}
	if r := byKind["sensor.a/below"]; r.Cooldown == nil || *r.Cooldown != 0 {
		t.Errorf("a zero cooldown must read back as an explicit zero, got %v", r.Cooldown)
	}

	// No foreign key: a rule needs no permission row, and deleting a permission keeps it.
	f.share("sensor.a", "read")
	if _, err := f.perms.Revoke(context.Background(), "sensor.a"); err != nil {
		t.Fatal(err)
	}
	if f.count("notification_rules") != 3 {
		t.Error("revoking a permission must not delete rules")
	}
}

func TestSchema_theRuleLimitTriggerMatchesTheServiceConstant(t *testing.T) {
	f := newFixture(t)
	insert := func(i int) error {
		_, err := f.db.Exec(`INSERT INTO notification_rules (entity_id, kind, created_by, created_at, updated_by, updated_at)
VALUES (?, 'state', 'a', 't', 'a', 't')`, fmt.Sprintf("binary_sensor.s%03d", i))
		return err
	}
	for i := 0; i < MaxRules; i++ {
		if err := insert(i); err != nil {
			t.Fatalf("rule %d of %d must be accepted: %v", i+1, MaxRules, err)
		}
	}
	err := insert(MaxRules)
	if err == nil || !strings.Contains(err.Error(), "rule limit") {
		t.Fatalf("rule %d must be refused by the trigger, got %v", MaxRules+1, err)
	}
}
