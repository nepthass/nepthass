package entities_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

type countingHA struct {
	*fakeHA
	calls int
}

func (c *countingHA) States(ctx context.Context) ([]entities.State, error) {
	c.calls++
	return c.fakeHA.States(ctx)
}

type discFixture struct {
	fixture
	disc  *entities.Discovery
	ha    *countingHA
	clock time.Time
}

func newDisc(t *testing.T, ids ...entities.ID) *discFixture {
	t.Helper()
	f := newFixture(t, ids...)
	d := &discFixture{fixture: f, clock: now}
	d.ha = &countingHA{fakeHA: f.ha}
	d.disc = entities.NewDiscovery(entities.DiscoveryDeps{HA: d.ha, DB: f.db, Permissions: f.perms, Now: func() time.Time { return d.clock }})
	return d
}

func TestSnapshot_sortsByName_handlesMissingAttributes_andFlagsUnreliableStates(t *testing.T) {
	d := newDisc(t)
	d.ha.states["light.b"] = entities.State{ID: "light.b", State: "on", Attributes: map[string]any{"friendly_name": "beta lamp"}}
	d.ha.states["light.a"] = entities.State{ID: "light.a", State: "off", Attributes: map[string]any{"friendly_name": "Alpha Lamp", "device_class": "x", "unit_of_measurement": "u"}}
	d.ha.states["sensor.no_name"] = entities.State{ID: "sensor.no_name", State: "unavailable"} // no attributes at all
	d.ha.states["sensor.odd"] = entities.State{ID: "sensor.odd", State: "unknown", Attributes: map[string]any{"friendly_name": 42}}

	snap, err := d.disc.Snapshot(context.Background(), false)

	if err != nil || snap.Total() != 4 || len(snap.Items) != 4 {
		t.Fatalf("snapshot = %+v, %v", snap, err)
	}
	var order []string
	for _, it := range snap.Items {
		order = append(order, string(it.ID))
	}
	// Alpha Lamp, beta lamp (case-insensitive), then entities named by their ID.
	if fmt.Sprint(order) != "[light.a light.b sensor.no_name sensor.odd]" {
		t.Errorf("order = %v", order)
	}
	noName, _ := snap.Find("sensor.no_name")
	odd, _ := snap.Find("sensor.odd")
	if noName.Name != "sensor.no_name" || noName.Reliable() || odd.Name != "sensor.odd" || odd.Reliable() {
		t.Errorf("fallback names / reliability wrong: %+v %+v", noName, odd)
	}
	if a, _ := snap.Find("light.a"); a.DeviceClass != "x" || a.Unit != "u" {
		t.Errorf("attributes lost: %+v", a)
	}
}

func TestSnapshot_isCachedUntilForcedOrExpired_andErrorsDoNotPoisonTheCache(t *testing.T) {
	d := newDisc(t, "light.a")
	ctx := context.Background()

	first, _ := d.disc.Snapshot(ctx, false)
	second, _ := d.disc.Snapshot(ctx, false)
	cachedCalls := d.ha.calls
	_, _ = d.disc.Snapshot(ctx, true)
	forcedCalls := d.ha.calls
	d.clock = d.clock.Add(entities.SnapshotTTL + time.Second)
	_, _ = d.disc.Snapshot(ctx, false)
	expiredCalls := d.ha.calls
	d.ha.statesErr = errors.New("home assistant down")
	_, err := d.disc.Snapshot(ctx, true)
	d.ha.statesErr = nil
	d.clock = d.clock.Add(time.Second)
	afterFailure, afterErr := d.disc.Snapshot(ctx, false)

	if first != second || cachedCalls != 1 || forcedCalls != 2 || expiredCalls != 3 {
		t.Errorf("calls cached=%d forced=%d expired=%d, same snapshot=%v", cachedCalls, forcedCalls, expiredCalls, first == second)
	}
	if err == nil {
		t.Error("a Home Assistant failure was swallowed")
	}
	if afterErr != nil || afterFailure.Total() != 1 {
		t.Errorf("cache unusable after a failed refresh: %+v, %v", afterFailure, afterErr)
	}
}

func TestSnapshot_emptyHomeAssistant(t *testing.T) {
	d := newDisc(t)

	snap, err := d.disc.Snapshot(context.Background(), true)
	rows, pages := func() (int, int) {
		page, _, n := entities.Paginate(snap.Items, 1, entities.PageSize)
		return len(page), n
	}()

	if err != nil || snap.Total() != 0 || len(snap.Search(entities.Filter{})) != 0 || rows != 0 || pages != 1 || len(snap.Domains()) != 0 {
		t.Errorf("empty snapshot misbehaves: %+v rows=%d pages=%d", snap, rows, pages)
	}
}

func TestSearch_filtersByWordsDomainAndSet(t *testing.T) {
	d := newDisc(t)
	add := func(id, name, class string) {
		d.ha.states[entities.ID(id)] = entities.State{ID: entities.ID(id), State: "on", Attributes: map[string]any{"friendly_name": name, "device_class": class}}
	}
	add("light.living_main", "Living Room Main", "")
	add("light.living_lamp", "Living Room Lamp", "")
	add("sensor.living_temp", "Living Room Temperature", "temperature")
	add("switch.garden_pump", "Garden Pump", "")
	snap, _ := d.disc.Snapshot(context.Background(), true)
	ids := func(items []entities.Item) string {
		var out []string
		for _, it := range items {
			out = append(out, string(it.ID))
		}
		return fmt.Sprint(out)
	}

	if got := ids(snap.Search(entities.Filter{Query: "living lamp"})); got != "[light.living_lamp]" {
		t.Errorf("all words must match: %s", got)
	}
	if got := ids(snap.Search(entities.Filter{Query: "TEMPERATURE"})); got != "[sensor.living_temp]" {
		t.Errorf("device class / case-insensitive: %s", got)
	}
	if got := ids(snap.Search(entities.Filter{Query: "garden_pump"})); got != "[switch.garden_pump]" {
		t.Errorf("entity id match: %s", got)
	}
	if got := ids(snap.Search(entities.Filter{Domain: "light"})); got != "[light.living_lamp light.living_main]" {
		t.Errorf("domain filter: %s", got)
	}
	only := map[entities.ID]struct{}{"switch.garden_pump": {}}
	if got := ids(snap.Search(entities.Filter{Only: only})); got != "[switch.garden_pump]" {
		t.Errorf("only-set filter: %s", got)
	}
	if got := snap.Search(entities.Filter{Query: "nothing like this"}); len(got) != 0 {
		t.Errorf("no match expected, got %v", got)
	}
	if doms := snap.Domains(); len(doms) != 3 || doms[0].Domain != "light" || doms[0].Count != 2 {
		t.Errorf("domains = %+v", doms)
	}
}

func TestLargeCollections_arePagedAndSearchedCorrectly_andLongIDsAreSkipped(t *testing.T) {
	d := newDisc(t)
	for i := range 5000 {
		id := entities.ID(fmt.Sprintf("sensor.device_%04d", i))
		d.ha.states[id] = entities.State{ID: id, State: "1", Attributes: map[string]any{"friendly_name": fmt.Sprintf("Device %04d", i)}}
	}
	long := entities.ID("sensor." + string(make([]byte, 0)) + fmt.Sprintf("%0110d", 7)) // 117 characters
	d.ha.states[long] = entities.State{ID: long, State: "1"}
	snap, err := d.disc.Snapshot(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}

	page, current, pages := entities.Paginate(snap.Items, 3, entities.PageSize)
	last, lastCurrent, _ := entities.Paginate(snap.Items, 99999, entities.PageSize)
	hits := snap.Search(entities.Filter{Query: "device 4999"})

	if snap.Total() != 5001 || len(snap.Items) != 5000 || snap.Skipped != 1 {
		t.Errorf("total=%d items=%d skipped=%d", snap.Total(), len(snap.Items), snap.Skipped)
	}
	if _, ok := snap.Find(long); !ok {
		t.Error("an entity with a too-long ID must still be findable for validation")
	}
	if len(page) != entities.PageSize || current != 3 || pages != 500 || page[0].Name != "Device 0020" {
		t.Errorf("page 3 = %d items, current=%d pages=%d first=%q", len(page), current, pages, page[0].Name)
	}
	if lastCurrent != 500 || len(last) != entities.PageSize {
		t.Errorf("clamped page = %d with %d items", lastCurrent, len(last))
	}
	if len(hits) != 1 || hits[0].ID != "sensor.device_4999" {
		t.Errorf("search hits = %+v", hits)
	}
}

func syncOnce(t *testing.T, d *discFixture) entities.SyncReport {
	t.Helper()
	d.clock = d.clock.Add(time.Minute)
	r, err := d.disc.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	return r
}

func TestSync_recordsABaselineFirst_thenReportsNewRemovedAndReturnedEntities(t *testing.T) {
	d := newDisc(t, "light.a", "light.b", "sensor.c")

	first := syncOnce(t, d)
	d.ha.states["switch.new"] = entities.State{ID: "switch.new", State: "off"}
	delete(d.ha.states, "light.b")
	second := syncOnce(t, d)
	unchanged := syncOnce(t, d)
	d.ha.states["light.b"] = entities.State{ID: "light.b", State: "on"}
	back := syncOnce(t, d)

	if !first.FirstSync || first.Total != 3 || len(first.New) != 0 || first.Controllable != 2 {
		t.Errorf("first sync = %+v", first)
	}
	if second.FirstSync || fmt.Sprint(second.New) != "[switch.new]" || fmt.Sprint(second.Removed) != "[light.b]" || len(second.Returned) != 0 {
		t.Errorf("second sync = %+v", second)
	}
	if len(unchanged.New)+len(unchanged.Removed)+len(unchanged.Returned) != 0 {
		t.Errorf("an unchanged sync reported changes: %+v", unchanged)
	}
	if fmt.Sprint(back.Returned) != "[light.b]" || len(back.New) != 0 || len(back.Removed) != 0 {
		t.Errorf("returning entity = %+v", back)
	}
}

func TestSync_neverGrantsAccess_andKeepsPermissionsAndAliases(t *testing.T) {
	d := newDisc(t, "light.kitchen", "sensor.gone", "sensor.flaky")
	d.grant(t, "light.kitchen", entities.AccessControl)
	d.grant(t, "sensor.gone", entities.AccessRead)
	d.grant(t, "sensor.flaky", entities.AccessRead)
	if err := set(t, d.fixture, "sensor.gone", "Old Sensor"); err != nil {
		t.Fatal(err)
	}
	syncOnce(t, d)
	delete(d.ha.states, "sensor.gone")
	d.ha.states["sensor.flaky"] = entities.State{ID: "sensor.flaky", State: "unavailable"}
	d.ha.states["light.brand_new"] = entities.State{ID: "light.brand_new", State: "off"}

	report := syncOnce(t, d)

	if fmt.Sprint(report.SharedMissing) != "[sensor.gone]" || fmt.Sprint(report.SharedUnavailable) != "[sensor.flaky]" || fmt.Sprint(report.New) != "[light.brand_new]" {
		t.Errorf("report = %+v", report)
	}
	perms, _ := d.perms.List(context.Background())
	if len(perms) != 3 {
		t.Errorf("permissions changed by sync: %+v", perms)
	}
	if access, _ := d.perms.Get(context.Background(), "light.brand_new"); access != entities.AccessNone {
		t.Errorf("a new entity was shared automatically: %q", access)
	}
	if alias, _ := d.aliases.Get(context.Background(), "sensor.gone"); alias != "Old Sensor" {
		t.Errorf("alias of a missing entity was dropped: %q", alias)
	}
}

func TestSync_survivesProcessRestarts_andPropagatesHomeAssistantFailures(t *testing.T) {
	d := newDisc(t, "light.a")
	syncOnce(t, d)
	restarted := entities.NewDiscovery(entities.DiscoveryDeps{HA: d.ha, DB: d.db, Permissions: d.perms, Now: func() time.Time { return d.clock }})
	d.ha.states["light.b"] = entities.State{ID: "light.b", State: "off"}
	d.clock = d.clock.Add(time.Minute)

	report, err := restarted.Sync(context.Background())
	d.ha.statesErr = errors.New("home assistant down")
	_, failErr := restarted.Sync(context.Background())

	if err != nil || report.FirstSync || fmt.Sprint(report.New) != "[light.b]" {
		t.Errorf("report after restart = %+v (%v): the registry must persist", report, err)
	}
	if failErr == nil {
		t.Error("a Home Assistant failure must be returned")
	}
	var count int
	if err := d.db.QueryRow("SELECT count(*) FROM known_entities WHERE missing_since IS NOT NULL").Scan(&count); err != nil || count != 0 {
		t.Errorf("a failed sync marked entities as missing: count=%d err=%v", count, err)
	}
}
