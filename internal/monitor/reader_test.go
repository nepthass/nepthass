package monitor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/storage"
)

var (
	user    = auth.Actor{UserID: "u1", Level: auth.LevelUser}
	admin   = auth.Actor{UserID: "a1", Level: auth.LevelAdmin}
	nobody  = auth.Actor{UserID: "x", Level: auth.LevelNone}
	rootCtx = context.Background()
)

type readerEnv struct {
	m       *Monitor
	r       *Reader
	perms   *entities.Permissions
	aliases *entities.Aliases
}

func newReaderEnv(t *testing.T) *readerEnv {
	t.Helper()
	db, err := storage.Open(rootCtx, filepath.Join(t.TempDir(), "reader.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m, err := New(Config{HomeAssistant: config.HomeAssistant{URL: "http://127.0.0.1:1", Token: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	e := &readerEnv{m: m, perms: entities.NewPermissions(db), aliases: entities.NewAliases(db)}
	e.r = NewReader(m, e.perms, e.aliases)
	return e
}

func (e *readerEnv) track(id, raw string, cat Category, name string) {
	e.m.Tracker().Apply(Update{ID: entityID(id), Category: cat, Raw: raw, Name: name, LastChanged: base, LastUpdated: base}, base)
}

func (e *readerEnv) share(t *testing.T, id string, access entities.Access) {
	t.Helper()
	if err := e.perms.Set(rootCtx, entities.Grant{ID: entityID(id), Access: access, GrantedBy: "a1", At: base}); err != nil {
		t.Fatal(err)
	}
}

func TestReader_showsOnlyWhatIsShared_andNeverLeaksUnsharedSensors(t *testing.T) {
	e := newReaderEnv(t)
	e.track("sensor.temp", "21.5", CategoryTemperature, "Living Temp")
	e.track("binary_sensor.door", "off", CategoryDoor, "Front Door")
	e.track("binary_sensor.leak", "on", CategoryLeak, "Basement Leak SECRET")
	e.share(t, "sensor.temp", entities.AccessRead)
	e.share(t, "binary_sensor.door", entities.AccessRead)

	page, err := e.r.Sensors(rootCtx, user, 1)

	if err != nil || page.Total != 2 || len(page.Views) != 2 {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if dump := fmt.Sprintf("%+v", page); strings.Contains(dump, "leak") || strings.Contains(dump, "SECRET") {
		t.Errorf("an unshared sensor leaked into the listing: %s", dump)
	}
}

func TestReader_isDenyByDefault_andAnUnsharedEntityLooksLikeAnUnknownOne(t *testing.T) {
	e := newReaderEnv(t)
	e.track("binary_sensor.leak", "on", CategoryLeak, "Leak")
	e.share(t, "light.kitchen", entities.AccessControl) // shared, but not a monitored sensor

	_, tracked := e.r.Sensor(rootCtx, user, "binary_sensor.leak")
	_, unknown := e.r.Sensor(rootCtx, user, "sensor.does_not_exist")
	_, notMonitored := e.r.Sensor(rootCtx, user, "light.kitchen")

	if !errors.Is(tracked, entities.ErrAccessDenied) || !errors.Is(unknown, entities.ErrAccessDenied) {
		t.Errorf("tracked-but-unshared=%v unknown=%v: both must be the same denial", tracked, unknown)
	}
	if tracked.Error() != unknown.Error() {
		t.Error("the two denials must be indistinguishable")
	}
	if !errors.Is(notMonitored, ErrNotMonitored) {
		t.Errorf("a shared but unmonitored entity = %v, want ErrNotMonitored", notMonitored)
	}
}

func TestReader_requiresAnAllowlistedUser(t *testing.T) {
	e := newReaderEnv(t)
	e.track("sensor.temp", "21.5", CategoryTemperature, "T")
	e.share(t, "sensor.temp", entities.AccessRead)

	_, listErr := e.r.Sensors(rootCtx, nobody, 1)
	_, oneErr := e.r.Sensor(rootCtx, nobody, "sensor.temp")
	_, userOK := e.r.Sensor(rootCtx, user, "sensor.temp")
	_, adminOK := e.r.Sensor(rootCtx, admin, "sensor.temp")

	if !errors.Is(listErr, auth.ErrNotAuthorized) || !errors.Is(oneErr, auth.ErrNotAuthorized) || userOK != nil || adminOK != nil {
		t.Errorf("list=%v one=%v user=%v admin=%v", listErr, oneErr, userOK, adminOK)
	}
}

func TestReader_revocationAppliesImmediately_evenForAlreadyCachedData(t *testing.T) {
	e := newReaderEnv(t)
	e.track("binary_sensor.door", "on", CategoryDoor, "Door")
	e.share(t, "binary_sensor.door", entities.AccessRead)
	before, errBefore := e.r.Sensor(rootCtx, user, "binary_sensor.door")

	if _, err := e.perms.Revoke(rootCtx, "binary_sensor.door"); err != nil {
		t.Fatal(err)
	}
	_, errAfter := e.r.Sensor(rootCtx, user, "binary_sensor.door")
	page, listErr := e.r.Sensors(rootCtx, user, 1)

	if errBefore != nil || before.Interpretation().Meaning != MeaningOpen {
		t.Fatalf("before = %+v, %v", before, errBefore)
	}
	if !errors.Is(errAfter, entities.ErrAccessDenied) || listErr != nil || page.Total != 0 {
		t.Errorf("after revocation: one=%v list=%v total=%d", errAfter, listErr, page.Total)
	}
	if _, stillCached := e.m.Tracker().Get("binary_sensor.door"); !stillCached {
		t.Error("the cache keeps ingesting regardless of permissions; only reads are gated")
	}
}

func TestReader_showsAliasesAndMarksDataStaleWhenMonitoringIsNotHealthy(t *testing.T) {
	e := newReaderEnv(t)
	e.track("sensor.temp", "21.5", CategoryTemperature, "Home Assistant Name")
	e.share(t, "sensor.temp", entities.AccessRead)
	if err := e.aliases.Set(rootCtx, entities.AliasSet{ID: "sensor.temp", Alias: "Living Room", By: "a1", At: base}); err != nil {
		t.Fatal(err)
	}

	v, err := e.r.Sensor(rootCtx, user, "sensor.temp")

	if err != nil || v.Alias != "Living Room" || v.DisplayName() != "Living Room" {
		t.Errorf("view = %+v, %v", v, err)
	}
	if !v.Stale {
		t.Error("a monitor that never connected must mark every value stale")
	}
}

func TestReader_marksDataFreshOnlyWhileMonitoringIsHealthy(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()
	db, err := storage.Open(rootCtx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	perms, aliases := entities.NewPermissions(db), entities.NewAliases(db)
	if err := perms.Set(rootCtx, entities.Grant{ID: "sensor.temp", Access: entities.AccessRead, GrantedBy: "a1", At: base}); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(r.m, perms, aliases)

	fresh, _ := reader.Sensor(rootCtx, user, "sensor.temp")
	r.srv.Down()
	r.wait(func(s Status) bool { return s.State == StateReconnecting })
	stale, _ := reader.Sensor(rootCtx, user, "sensor.temp")

	if fresh.Stale || fresh.Raw != "21.5" {
		t.Errorf("healthy read = %+v", fresh)
	}
	if !stale.Stale || stale.Raw != "21.5" {
		t.Errorf("after the connection dropped the last value stays readable but must be marked stale: %+v", stale)
	}
}

func TestReader_pagesAndSortsByDisplayName(t *testing.T) {
	e := newReaderEnv(t)
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("sensor.s%02d", i)
		e.track(id, "1", CategoryTemperature, fmt.Sprintf("Sensor %02d", 24-i)) // reverse of the ID order
		e.share(t, id, entities.AccessRead)
	}

	p1, _ := e.r.Sensors(rootCtx, user, 1)
	p3, _ := e.r.Sensors(rootCtx, user, 3)
	far, _ := e.r.Sensors(rootCtx, user, 99)

	if p1.Total != 25 || p1.Pages != 3 || len(p1.Views) != 10 || p1.Views[0].Name != "Sensor 00" || p1.Views[9].Name != "Sensor 09" {
		t.Errorf("page 1 = %d views, first %q last %q", len(p1.Views), p1.Views[0].Name, p1.Views[len(p1.Views)-1].Name)
	}
	if len(p3.Views) != 5 || far.Page != 3 {
		t.Errorf("page 3 = %d views; out-of-range page = %d", len(p3.Views), far.Page)
	}
}

func TestReader_failsClosed_whenThePermissionStoreFails(t *testing.T) {
	e := newReaderEnv(t)
	e.track("sensor.temp", "21.5", CategoryTemperature, "T")
	e.r.perms = failingPerms{}

	page, listErr := e.r.Sensors(rootCtx, user, 1)
	v, oneErr := e.r.Sensor(rootCtx, user, "sensor.temp")

	if listErr == nil || oneErr == nil || len(page.Views) != 0 || v.ID != "" {
		t.Errorf("a failing permission store must expose nothing: list=%v one=%v", listErr, oneErr)
	}
}

type failingPerms struct{}

func (failingPerms) Get(context.Context, entities.ID) (entities.Access, error) {
	return "", errors.New("database unavailable")
}

func (failingPerms) List(context.Context) ([]entities.Permission, error) {
	return nil, errors.New("database unavailable")
}

var _ = time.Second
