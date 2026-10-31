package notify

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/storage"
)

const (
	testGuild   = "100000000000000001"
	testChannel = "200000000000000002"
)

var (
	admin  = auth.Actor{UserID: "900000000000000001", Level: auth.LevelAdmin}
	user   = auth.Actor{UserID: "900000000000000002", Level: auth.LevelUser}
	nobody = auth.Actor{UserID: "900000000000000003", Level: auth.LevelNone}
)

type fixture struct {
	t       *testing.T
	db      *sql.DB
	svc     *Service
	store   *Store
	perms   *entities.Permissions
	audit   *audit.Log
	tracker *monitor.Tracker
	now     time.Time
}

// newFixture opens a real database file (WAL, immediate transactions, the same
// pool as production) with every migration applied.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "notify.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{t: t, db: db, store: NewStore(db), perms: entities.NewPermissions(db), audit: audit.New(db),
		tracker: monitor.NewTracker(), now: time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)}
	f.svc = NewService(Deps{GuildID: testGuild, DB: db, Store: f.store, Permissions: f.perms, Sensors: f.tracker,
		Audit: f.audit, Now: func() time.Time { return f.now }})
	return f
}

func (f *fixture) share(id string, access entities.Access) {
	f.t.Helper()
	if err := f.perms.Set(context.Background(), entities.Grant{ID: entities.ID(id), Access: access, GrantedBy: "admin", At: f.now}); err != nil {
		f.t.Fatal(err)
	}
}

// monitored makes the tracker know a sensor, as the monitor would.
func (f *fixture) monitored(id string, cat monitor.Category, raw string) {
	f.t.Helper()
	f.now = f.now.Add(time.Second)
	f.tracker.Apply(monitor.Update{ID: entities.ID(id), Category: cat, Name: "Name", Raw: raw, Unit: unitFor(cat), LastChanged: f.now, LastUpdated: f.now}, f.now)
}

// ready shares a sensor and makes it monitored.
func (f *fixture) ready(id string, cat monitor.Category) {
	f.t.Helper()
	f.share(id, entities.AccessRead)
	f.monitored(id, cat, "off")
}

func (f *fixture) count(table string) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) auditRows() []audit.Entry {
	f.t.Helper()
	rows, err := f.audit.Recent(context.Background(), 500)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

func (f *fixture) lastAudit() audit.Entry {
	f.t.Helper()
	rows := f.auditRows()
	if len(rows) == 0 {
		f.t.Fatal("no audit entry was written")
	}
	return rows[0]
}

func fl(v float64) *float64              { return &v }
func dur(d time.Duration) *time.Duration { return &d }

// unitFor is the first documented unit of a numeric category ("" for a binary
// one), so a test sensor reports a unit a numeric rule can be bound to.
func unitFor(c monitor.Category) string {
	if s, ok := monitor.SpecFor(c); ok && len(s.Units) > 0 {
		return s.Units[0]
	}
	return ""
}
