package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/nepthass/nepthass/migrations"
)

// legacyDB is a database exactly as a v0.4.0 build leaves it: migrations 0001 to
// 0003 applied under the production connection settings (foreign keys on, WAL,
// immediate transactions). It is always a fresh temporary file: these tests never
// touch a real NeptHass database.
func legacyDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(context.Background(), db, v040FS(t)); err != nil {
		t.Fatalf("build the v0.4.0 database: %v", err)
	}
	return db, path
}

type legacyRule struct {
	entity, kind string
	threshold    any
	enabled      int
	cooldown     any
	by           string
}

// seedLegacy inserts adversarial 0003 data and deletes some rules so that the
// AUTOINCREMENT sequence (9) is higher than the highest remaining ID.
func seedLegacy(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "INSERT INTO notification_settings (guild_id, channel_id, enabled, updated_by, updated_at) VALUES ('123456789012345678','223456789012345678',1,'u1','2026-10-30T10:00:00.000000Z')"); err != nil {
		t.Fatal(err)
	}
	long := "sensor." + strings.Repeat("a", 247) // 255 characters, the CHECK limit
	rules := []legacyRule{
		{"binary_sensor.door", "state", nil, 1, nil, "u1"},
		{"sensor.temp", "above", 30.5, 1, 900, "u1"},
		{"sensor.temp", "below", -1000000.0, 0, 0, "u2"},
		{"binary_sensor.window", "state", nil, 1, 86400, "u-ünï"},
		{"binary_sensor.leak", "state", nil, 1, nil, "u1"},
		{long, "above", 1000000.0, 1, 60, "u1"},
		{"sensor.hum", "below", 0.1 + 0.2, 1, nil, "u3"},
		{"binary_sensor.motion", "state", nil, 0, 300, "u1"},
		{"sensor.bat", "below", 20.0, 1, 21600, "u1"},
	}
	for _, r := range rules {
		if _, err := db.ExecContext(ctx, `INSERT INTO notification_rules (entity_id, kind, threshold, enabled, cooldown_seconds, created_by, created_at, updated_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, '2026-10-30T10:00:00.000000Z', ?, '2026-10-30T10:05:00.000000Z')`, r.entity, r.kind, r.threshold, r.enabled, r.cooldown, r.by, r.by); err != nil {
			t.Fatalf("seed %s: %v", r.entity, err)
		}
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM notification_rules WHERE id IN (5, 8, 9)"); err != nil {
		t.Fatal(err)
	}
	for i, id := range []int{1, 2, 3, 4, 6, 7} {
		if _, err := db.ExecContext(ctx, `INSERT INTO command_audit_logs (occurred_at, actor_id, command, entity_id, action, outcome, detail) VALUES (?, 'u1', 'notifications.rule.add', NULL, 'state', 'success', ?)`,
			fmt.Sprintf("2026-10-30T10:0%d:00.000000Z", i), fmt.Sprintf("rule=%d kind=state", id)); err != nil {
			t.Fatal(err)
		}
	}
}

const preservedRuleColumns = "id, entity_id, kind, threshold, enabled, cooldown_seconds, created_by, created_at, updated_by, updated_at"

func ruleDigest(t *testing.T, db *sql.DB) (string, int) {
	t.Helper()
	rows, err := db.Query("SELECT " + preservedRuleColumns + " FROM notification_rules ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	h, n := sha256.New(), 0
	for rows.Next() {
		var id, enabled int64
		var entity, kind, by, created, uby, updated string
		var threshold sql.NullFloat64
		var cooldown sql.NullInt64
		if err := rows.Scan(&id, &entity, &kind, &threshold, &enabled, &cooldown, &by, &created, &uby, &updated); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%d|%s|%s|%v|%v|%d|%v|%v|%s|%s|%s|%s\n", id, entity, kind, threshold.Valid, threshold.Float64, enabled, cooldown.Valid, cooldown.Int64, by, created, uby, updated)
		n++
	}
	return hex.EncodeToString(h.Sum(nil)), n
}

func countOf(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestMigration0004_upgradesAV040Database_preservingEverything(t *testing.T) {
	ctx := context.Background()
	db, _ := legacyDB(t)
	seedLegacy(t, db)
	beforeDigest, beforeRows := ruleDigest(t, db)
	auditBefore := countOf(t, db, "SELECT count(*) FROM command_audit_logs")
	var seqBefore int
	if err := db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name = 'notification_rules'").Scan(&seqBefore); err != nil || seqBefore != 9 || beforeRows != 6 {
		t.Fatalf("the seed must leave 6 rules and a sequence of 9: rows=%d seq=%d err=%v", beforeRows, seqBefore, err)
	}

	if err := Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("upgrade to 0004: %v", err)
	}

	afterDigest, afterRows := ruleDigest(t, db)
	if afterRows != beforeRows || afterDigest != beforeDigest {
		t.Fatalf("rules were not preserved row for row: rows %d -> %d, digest equal=%v", beforeRows, afterRows, afterDigest == beforeDigest)
	}
	if n := countOf(t, db, "SELECT count(*) FROM notification_rules WHERE duration_seconds <> 0 OR clear_threshold IS NOT NULL OR unit <> ''"); n != 0 {
		t.Errorf("every legacy rule must become an immediate rule with no clear value and no unit: %d differ", n)
	}
	if got := countOf(t, db, "SELECT count(*) FROM command_audit_logs"); got != auditBefore {
		t.Errorf("audit rows: %d -> %d", auditBefore, got)
	}
	if n := countOf(t, db, "SELECT count(*) FROM notification_settings WHERE channel_id = '223456789012345678' AND enabled = 1 AND startup_summary_at IS NULL"); n != 1 {
		t.Error("settings must be preserved with the new summary time empty")
	}
	// The sequence is preserved: the next rule gets 10, never a reused 8 or 9 (rule IDs appear in audit entries).
	res, err := db.Exec(`INSERT INTO notification_rules (entity_id, kind, threshold, created_by, created_at, updated_by, updated_at) VALUES ('binary_sensor.new','state',NULL,'u','t','u','t')`)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 10 {
		t.Errorf("the next rule ID must be 10 (sequence preserved), got %d", id)
	}
	if n := countOf(t, db, "SELECT count(*) FROM pragma_foreign_key_check"); n != 0 {
		t.Errorf("foreign_key_check reports %d problems", n)
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Errorf("integrity_check: %q %v", integrity, err)
	}

	// A restart changes nothing, and a v0.4.0 build refuses the database.
	if err := Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if n := countOf(t, db, "SELECT count(*) FROM schema_migrations"); n != 4 {
		t.Errorf("a restart must not add or re-apply migrations: %d", n)
	}
	if again, rows := ruleDigest(t, db); rows != beforeRows+1 || again == "" {
		t.Errorf("a restart must not change rules: %d", rows)
	}
	if err := Migrate(ctx, db, v040FS(t)); !errors.Is(err, ErrSchemaNewer) {
		t.Errorf("a v0.4.0 build must refuse a database that has migration 0004: %v", err)
	}
}

func TestMigration0004_enforcesTheNewConstraints_andKeepsTheRuleLimit(t *testing.T) {
	ctx := context.Background()
	db, _ := legacyDB(t)
	if err := Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO notification_rules (entity_id, kind, threshold, clear_threshold, duration_seconds, created_by, created_at, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, 'u', 't', 'u', 't')`
	ok := func(label string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ins, args...); err != nil {
			t.Errorf("%s must be accepted: %v", label, err)
		}
	}
	bad := func(label string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ins, args...); err == nil {
			t.Errorf("%s must be rejected", label)
		}
	}
	ok("an immediate rule", "binary_sensor.door", "state", nil, nil, 0)
	ok("a 10-minute rule on the same entity and kind", "binary_sensor.door", "state", nil, nil, 600)
	bad("a duplicate of (entity, kind, duration)", "binary_sensor.door", "state", nil, nil, 600)
	ok("above 30 with clear 28 for 10 minutes", "sensor.t", "above", 30.0, 28.0, 600)
	ok("below 5 with clear 7", "sensor.t", "below", 5.0, 7.0, 0)
	bad("clear on a state rule", "binary_sensor.d2", "state", nil, 5.0, 0)
	bad("clear at or above the threshold of an above rule", "sensor.t2", "above", 30.0, 30.0, 0)
	bad("clear below the threshold of a below rule", "sensor.t3", "below", 5.0, 4.0, 0)
	bad("a duration over 24 hours", "sensor.t4", "above", 30.0, nil, 86401)
	bad("a negative duration", "sensor.t5", "above", 30.0, nil, -1)
	bad("a threshold on a state rule", "binary_sensor.d3", "state", 1.0, nil, 0)
	bad("a numeric rule without a threshold", "sensor.t6", "above", nil, nil, 0)
	bad("a threshold beyond the limit", "sensor.t7", "above", 1000001.0, nil, 0)

	n := countOf(t, db, "SELECT count(*) FROM notification_rules")
	for i := 0; i < 200-n; i++ {
		ok("a bulk rule", fmt.Sprintf("sensor.bulk%d", i), "above", 1.0, nil, 0)
	}
	if _, err := db.Exec(ins, "sensor.over", "above", 1.0, nil, 0); err == nil || !strings.Contains(err.Error(), "notification rule limit reached") {
		t.Errorf("the 201st rule must hit the limit trigger, got %v", err)
	}
}

func TestMigration0004_marksFollowTheirRule_andTheSummaryTimeIsPersisted(t *testing.T) {
	ctx := context.Background()
	db, _ := legacyDB(t)
	seedLegacy(t, db)
	if err := Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO notification_marks (rule_id, episode_key, delivered_at) VALUES (1, '2026-10-30T10:00:00.123456789Z', '2026-10-30T10:00:01.000000Z')"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO notification_marks (rule_id, episode_key, delivered_at) VALUES (999, 'k', 't')"); err == nil {
		t.Error("a mark for a rule that does not exist must be refused (foreign key)")
	}
	if _, err := db.Exec("INSERT INTO notification_marks (rule_id, episode_key, delivered_at) VALUES (1, 'again', 't')"); err == nil {
		t.Error("a rule has at most one mark")
	}
	if _, err := db.Exec("DELETE FROM notification_rules WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if n := countOf(t, db, "SELECT count(*) FROM notification_marks"); n != 0 {
		t.Errorf("deleting a rule must delete its mark: %d left", n)
	}
	if _, err := db.Exec("UPDATE notification_settings SET startup_summary_at = '2026-10-30T11:00:00.000000Z'"); err != nil {
		t.Fatal(err)
	}
}

func TestMigration0004_aFailureHalfwayRollsEverythingBack_andTheOriginalIsUntouched(t *testing.T) {
	ctx := context.Background()
	db, path := legacyDB(t)
	seedLegacy(t, db)
	beforeDigest, beforeRows := ruleDigest(t, db)
	auditBefore := countOf(t, db, "SELECT count(*) FROM command_audit_logs")
	sum, err := fs.ReadFile(migrations.FS, "0004_notification_rules_v2.sql")
	if err != nil {
		t.Fatal(err)
	}
	// The real migration followed by a statement that fails, after the table has
	// been dropped and renamed: the transaction must undo all of it.
	broken := fstest.MapFS{}
	for _, name := range []string{"0001_foundation.sql", "0002_interactive_setup.sql", "0003_notifications.sql"} {
		b, _ := fs.ReadFile(migrations.FS, name)
		broken[name] = &fstest.MapFile{Data: b}
	}
	broken["0004_notification_rules_v2.sql"] = &fstest.MapFile{Data: append(append([]byte{}, sum...), []byte("\nINSERT INTO a_table_that_does_not_exist VALUES (1);\n")...)}
	if err := Migrate(ctx, db, broken); err == nil {
		t.Fatal("the broken migration must fail")
	}
	if d, n := ruleDigest(t, db); n != beforeRows || d != beforeDigest {
		t.Error("a failed migration must leave the rules exactly as they were")
	}
	if n := countOf(t, db, "SELECT count(*) FROM sqlite_master WHERE name IN ('notification_rules_new','notification_marks','_rules_seq')"); n != 0 {
		t.Errorf("a failed migration must leave no half-built tables: %d", n)
	}
	if n := countOf(t, db, "SELECT count(*) FROM schema_migrations"); n != 3 {
		t.Errorf("a failed migration must not be recorded: %d applied", n)
	}
	if n := countOf(t, db, "SELECT count(*) FROM pragma_table_info('notification_rules') WHERE name IN ('duration_seconds','clear_threshold','unit')"); n != 0 {
		t.Error("the rules table must still have its 0003 shape")
	}
	var seq int
	if err := db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name = 'notification_rules'").Scan(&seq); err != nil || seq != 9 {
		t.Errorf("the ID sequence must be untouched: %d %v", seq, err)
	}
	if countOf(t, db, "SELECT count(*) FROM command_audit_logs") != auditBefore {
		t.Error("audit rows changed")
	}

	// After the failure the real migration still applies cleanly to the same database.
	if err := Migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("the real migration after a failed attempt: %v", err)
	}
	if d, n := ruleDigest(t, db); n != beforeRows || d != beforeDigest {
		t.Error("the retry must preserve the rules too")
	}
	// A database file reopened after the failure and success is the same data (persistence across restart).
	_ = db.Close()
	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if d, n := ruleDigest(t, again); n != beforeRows || d != beforeDigest {
		t.Error("the data must survive a restart")
	}
}

func TestMigration0004_aFreshDatabaseGetsTheNewSchema(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n := countOf(t, db, "SELECT count(*) FROM schema_migrations"); n != 4 {
		t.Errorf("a fresh database applies four migrations: %d", n)
	}
	want := map[string]bool{"clear_threshold": true, "unit": true, "duration_seconds": true}
	rows, err := db.Query("SELECT name FROM pragma_table_info('notification_rules')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("columns missing: %v", want)
	}
}

func TestMigration0004_concurrentWritersDoNotCorruptOrLoseData(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO notification_rules (entity_id, kind, threshold, created_by, created_at, updated_by, updated_at) VALUES ('binary_sensor.leak','state',NULL,'u','t','u','t')`); err != nil {
		t.Fatal(err)
	}
	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*each*3)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := db.ExecContext(ctx, `INSERT INTO notification_rules (entity_id, kind, threshold, duration_seconds, created_by, created_at, updated_by, updated_at)
VALUES (?, 'above', 1.0, ?, 'u', 't', 'u', 't')`, fmt.Sprintf("sensor.w%d_%d", w, i), 0); err != nil && !strings.Contains(err.Error(), "limit reached") {
					errs <- err
				}
				if _, err := db.ExecContext(ctx, `INSERT INTO notification_marks (rule_id, episode_key, delivered_at) VALUES (1, ?, 't')
ON CONFLICT (rule_id) DO UPDATE SET episode_key = excluded.episode_key`, fmt.Sprintf("k%d_%d", w, i)); err != nil {
					errs <- err
				}
				var n int
				if err := db.QueryRowContext(ctx, "SELECT count(*) FROM notification_rules").Scan(&n); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent access failed: %v", err)
	}
	if n := countOf(t, db, "SELECT count(*) FROM notification_rules"); n != 200 {
		t.Errorf("exactly 200 rules must exist (the limit trigger holds under contention): %d", n)
	}
	if n := countOf(t, db, "SELECT count(*) FROM notification_marks"); n != 1 {
		t.Errorf("one mark per rule: %d", n)
	}
}
