package notify

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

func TestService_addRule_durationsAndClearValues(t *testing.T) {
	f := newFixture(t)
	f.ready("binary_sensor.door", monitor.CategoryDoor)
	f.ready("binary_sensor.leak", monitor.CategoryLeak)
	f.ready("sensor.t", monitor.CategoryTemperature)
	f.ready("sensor.h", monitor.CategoryHumidity)

	cases := []struct {
		name string
		n    NewRule
		want error
	}{
		{"a door open for 10 minutes", NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute}, nil},
		{"the same door, immediate, alongside it", NewRule{Entity: "binary_sensor.door", Kind: KindState}, nil},
		{"the same door and duration again", NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute}, ErrRuleExists},
		{"the same door with another duration", NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 5 * time.Minute}, nil},
		{"a duration of 30 seconds is below the minimum", NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 30 * time.Second}, ErrInvalidDuration},
		{"a duration over 24 hours", NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 25 * time.Hour}, ErrInvalidDuration},
		{"a duration that is not whole seconds", NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: time.Minute + time.Millisecond}, ErrInvalidDuration},
		{"a negative duration", NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: -time.Minute}, ErrInvalidDuration},
		{"a water leak is immediate only: no duration", NewRule{Entity: "binary_sensor.leak", Kind: KindState, Duration: 10 * time.Minute}, ErrHazardImmediate},
		{"an immediate water leak rule", NewRule{Entity: "binary_sensor.leak", Kind: KindState}, nil},
		{"a state rule takes no clear value", NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: time.Hour, Clear: fl(1)}, ErrClearNotAllowed},
		{"above 30 clearing at 28 for 5 minutes", NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(30), Clear: fl(28), Duration: 5 * time.Minute}, nil},
		{"below 5 clearing at 7", NewRule{Entity: "sensor.t", Kind: KindBelow, Threshold: fl(5), Clear: fl(7)}, nil},
		{"clear equal to the threshold", NewRule{Entity: "sensor.h", Kind: KindAbove, Threshold: fl(60), Clear: fl(60)}, ErrInvalidClear},
		{"clear on the wrong side of an above rule", NewRule{Entity: "sensor.h", Kind: KindAbove, Threshold: fl(60), Clear: fl(61)}, ErrInvalidClear},
		{"clear on the wrong side of a below rule", NewRule{Entity: "sensor.h", Kind: KindBelow, Threshold: fl(30), Clear: fl(29)}, ErrInvalidClear},
		{"a NaN clear value", NewRule{Entity: "sensor.h", Kind: KindAbove, Threshold: fl(60), Clear: fl(math.NaN())}, ErrInvalidClear},
		{"an infinite clear value", NewRule{Entity: "sensor.h", Kind: KindBelow, Threshold: fl(30), Clear: fl(math.Inf(1))}, ErrInvalidClear},
		{"a clear value beyond the limit", NewRule{Entity: "sensor.h", Kind: KindAbove, Threshold: fl(60), Clear: fl(-2_000_000)}, ErrInvalidClear},
	}
	for _, c := range cases {
		r, err := f.svc.AddRule(ctx, admin, c.n)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
			continue
		}
		if err == nil && (r.Duration != c.n.Duration || (c.n.Clear == nil) != (r.Clear == nil)) {
			t.Errorf("%s: stored %+v", c.name, r)
		}
	}
	// A refusal is audited with its fixed sentinel text only.
	rows := f.auditRows()
	found := false
	for _, e := range rows {
		if e.Detail == ErrHazardImmediate.Error() && e.Outcome == audit.OutcomeError {
			found = true
		}
	}
	if !found {
		t.Error("the refused leak duration must be audited with the sentinel message")
	}
}

func TestService_addRule_bindsTheUnit_andRefusesAMissingOrUnsupportedOne(t *testing.T) {
	f := newFixture(t)
	f.share("sensor.t", entities.AccessRead)
	f.share("sensor.h", entities.AccessRead)
	apply := func(id string, cat monitor.Category, raw, unit string) {
		f.now = f.now.Add(time.Second)
		f.tracker.Apply(monitor.Update{ID: entities.ID(id), Category: cat, Name: "N", Raw: raw, Unit: unit, LastChanged: f.now, LastUpdated: f.now}, f.now)
	}
	i := 0
	for _, c := range []struct {
		unit string
		want error
	}{{"°C", nil}, {"°F", nil}, {"K", nil}, {"", ErrUnitUnsupported}, {"celsius", ErrUnitUnsupported}, {"C", ErrUnitUnsupported}, {"%", ErrUnitUnsupported}} {
		i++
		apply("sensor.t", monitor.CategoryTemperature, "21", c.unit)
		_, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(30), Duration: time.Duration(i) * time.Minute}) // a distinct duration each time, so only the unit decides
		if !errors.Is(err, c.want) {
			t.Errorf("temperature reporting %q: got %v, want %v", c.unit, err, c.want)
		}
	}
	views, _ := f.svc.Rules(ctx, admin)
	units := map[string]bool{}
	for _, v := range views {
		units[v.Unit] = true
	}
	if !units["°C"] || !units["°F"] || !units["K"] || len(views) != 3 {
		t.Errorf("each accepted rule is bound to the unit the sensor reported when it was created: %+v", units)
	}
	apply("sensor.h", monitor.CategoryHumidity, "40", "%")
	if r, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "sensor.h", Kind: KindBelow, Threshold: fl(30)}); err != nil || r.Unit != "%" {
		t.Errorf("humidity binds to %%: %+v %v", r, err)
	}
	apply("sensor.h", monitor.CategoryHumidity, "40", "°C")
	if _, err := f.svc.AddRule(ctx, admin, NewRule{Entity: "sensor.h", Kind: KindAbove, Threshold: fl(60)}); !errors.Is(err, ErrUnitUnsupported) {
		t.Errorf("a humidity sensor reporting °C is not a humidity reading: %v", err)
	}
}

func legacyRule(f *fixture, id string, kind Kind, thr float64) int64 {
	f.t.Helper()
	rid, err := f.store.InsertRule(ctx, Rule{Entity: entities.ID(id), Kind: kind, Threshold: &thr, Enabled: true, CreatedBy: "legacy", CreatedAt: f.now, UpdatedBy: "legacy", UpdatedAt: f.now})
	if err != nil {
		f.t.Fatal(err)
	}
	return rid
}

func TestRules_unitStatuses_unresolvedLegacy_andChangedUnit(t *testing.T) {
	f := newFixture(t)
	f.ready("sensor.h", monitor.CategoryHumidity)
	f.ready("sensor.t", monitor.CategoryTemperature)
	legacyRule(f, "sensor.h", KindBelow, 30)
	bound, _ := f.svc.AddRule(ctx, admin, NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(30)})
	status := func(id int64) RuleStatus {
		views, err := f.svc.Rules(ctx, admin)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range views {
			if v.ID == id {
				return v.Status
			}
		}
		t.Fatalf("rule %d not listed", id)
		return ""
	}
	if s := status(1); s != StatusUnitUnresolved {
		t.Errorf("a numeric rule with no unit is unresolved and must not evaluate: %s", s)
	}
	if s := status(bound.ID); s != StatusActive {
		t.Errorf("a freshly bound rule is active: %s", s)
	}
	f.now = f.now.Add(time.Second)
	f.tracker.Apply(monitor.Update{ID: "sensor.t", Category: monitor.CategoryTemperature, Name: "N", Raw: "70", Unit: "°F", LastChanged: f.now, LastUpdated: f.now}, f.now)
	if s := status(bound.ID); s != StatusUnitChanged {
		t.Errorf("the sensor switched from °C to °F: the threshold must not be reinterpreted: %s", s)
	}
	f.now = f.now.Add(time.Second)
	f.tracker.Apply(monitor.Update{ID: "sensor.t", Category: monitor.CategoryTemperature, Name: "N", Raw: "21", Unit: "°C", LastChanged: f.now, LastUpdated: f.now}, f.now)
	if s := status(bound.ID); s != StatusActive {
		t.Errorf("the rule resumes by itself when the unit returns: %s", s)
	}
}

func TestBindLegacyUnits_bindsOnlyWhatIsUnambiguous_once_andAudited(t *testing.T) {
	f := newFixture(t)
	for id, cat := range map[string]monitor.Category{"sensor.h": monitor.CategoryHumidity, "sensor.b": monitor.CategoryBattery, "sensor.t": monitor.CategoryTemperature} {
		f.share(id, entities.AccessRead)
		f.monitored(id, cat, "40")
	}
	f.ready("binary_sensor.door", monitor.CategoryDoor)
	hum := legacyRule(f, "sensor.h", KindBelow, 30)
	bat := legacyRule(f, "sensor.b", KindBelow, 20)
	temp := legacyRule(f, "sensor.t", KindAbove, 30)
	state, _ := f.store.InsertRule(ctx, Rule{Entity: "binary_sensor.door", Kind: KindState, Enabled: true, CreatedBy: "l", CreatedAt: f.now, UpdatedBy: "l", UpdatedAt: f.now})
	offline := legacyRule(f, "sensor.gone", KindAbove, 5) // Home Assistant has never reported this sensor
	unitOf := func(id int64) string {
		r, _, _ := f.store.GetRule(ctx, id)
		return r.Unit
	}

	n, err := f.svc.BindLegacyUnits(ctx)
	if err != nil || n != 2 {
		t.Fatalf("humidity and battery have exactly one unit and bind; temperature, a state rule and an unmonitored sensor do not: bound %d, %v", n, err)
	}
	if unitOf(hum) != "%" || unitOf(bat) != "%" || unitOf(temp) != "" || unitOf(state) != "" || unitOf(offline) != "" {
		t.Errorf("units: hum=%q bat=%q temp=%q state=%q offline=%q", unitOf(hum), unitOf(bat), unitOf(temp), unitOf(state), unitOf(offline))
	}
	if n, _ := f.svc.BindLegacyUnits(ctx); n != 0 {
		t.Errorf("a rule is bound at most once; a second pass bound %d", n)
	}
	var bind []audit.Entry
	for _, e := range f.auditRows() {
		if e.Action == "bind" {
			bind = append(bind, e)
		}
	}
	if len(bind) != 2 || bind[0].ActorID != SystemActor || bind[0].Outcome != audit.OutcomeSuccess || bind[0].Command != "notifications.rule.unit" {
		t.Fatalf("each binding is audited once, by the system actor: %+v", bind)
	}
	for _, e := range bind {
		if e.Detail != "rule=1 unit=%" && e.Detail != "rule=2 unit=%" {
			t.Errorf("the audit detail carries the rule and the unit only: %q", e.Detail)
		}
	}
}

func TestBindLegacyUnits_doesNothingWhileTheSensorIsNotReporting_orReportsAnotherUnit(t *testing.T) {
	f := newFixture(t)
	f.share("sensor.h", entities.AccessRead)
	legacyRule(f, "sensor.h", KindBelow, 30)
	apply := func(raw, unit string) {
		f.now = f.now.Add(time.Second)
		f.tracker.Apply(monitor.Update{ID: "sensor.h", Category: monitor.CategoryHumidity, Name: "N", Raw: raw, Unit: unit, LastChanged: f.now, LastUpdated: f.now}, f.now)
	}
	for _, c := range []struct{ raw, unit string }{{"unavailable", "%"}, {"unknown", "%"}, {"banana", "%"}, {"40", ""}, {"40", "°C"}, {"40", "percent"}} {
		apply(c.raw, c.unit)
		if n, err := f.svc.BindLegacyUnits(ctx); n != 0 || err != nil {
			t.Errorf("state %q unit %q must not bind: %d %v", c.raw, c.unit, n, err)
		}
	}
	apply("40", "%")
	if n, _ := f.svc.BindLegacyUnits(ctx); n != 1 {
		t.Errorf("a real reading in the documented unit binds: %d", n)
	}
}

func TestConfirmUnit_isTheAdministratorsRecoveryPath(t *testing.T) {
	f := newFixture(t)
	f.share("sensor.t", entities.AccessRead)
	f.now = f.now.Add(time.Second)
	f.tracker.Apply(monitor.Update{ID: "sensor.t", Category: monitor.CategoryTemperature, Name: "N", Raw: "70", Unit: "°F", LastChanged: f.now, LastUpdated: f.now}, f.now)
	id := legacyRule(f, "sensor.t", KindAbove, 80)
	f.ready("binary_sensor.door", monitor.CategoryDoor)
	state, _ := f.store.InsertRule(ctx, Rule{Entity: "binary_sensor.door", Kind: KindState, Enabled: true, CreatedBy: "l", CreatedAt: f.now, UpdatedBy: "l", UpdatedAt: f.now})

	if _, err := f.svc.ConfirmUnit(ctx, user, id); !errors.Is(err, auth.ErrAdminRequired) {
		t.Errorf("only an administrator can confirm a unit: %v", err)
	}
	if u, _, _ := f.store.GetRule(ctx, id); u.Unit != "" {
		t.Error("a refused confirmation changes nothing")
	}
	if _, err := f.svc.ConfirmUnit(ctx, admin, 4242); !errors.Is(err, ErrRuleNotFound) {
		t.Errorf("unknown rule: %v", err)
	}
	if _, err := f.svc.ConfirmUnit(ctx, admin, state); !errors.Is(err, ErrIncompatible) {
		t.Errorf("a state rule has no unit: %v", err)
	}
	unit, err := f.svc.ConfirmUnit(ctx, admin, id)
	if err != nil || unit != "°F" {
		t.Fatalf("confirm: %q %v", unit, err)
	}
	if _, err := f.svc.ConfirmUnit(ctx, admin, id); !errors.Is(err, ErrUnitBound) {
		t.Errorf("a unit is never rebound: %v", err)
	}
	last := f.lastAudit()
	if last.Action != "confirm" || last.Outcome != audit.OutcomeError || last.Detail != ErrUnitBound.Error() {
		t.Errorf("the refused rebinding is audited with the sentinel: %+v", last)
	}
	var confirmed *audit.Entry
	for _, e := range f.auditRows() {
		if e.Action == "confirm" && e.Outcome == audit.OutcomeSuccess {
			e := e
			confirmed = &e
		}
	}
	if confirmed == nil || confirmed.ActorID != admin.UserID || confirmed.Detail != "rule=1 unit=°F" {
		t.Errorf("the confirmation is audited with the administrator: %+v", confirmed)
	}
	// A unit that is missing or not supported cannot be confirmed.
	f.now = f.now.Add(time.Second)
	f.tracker.Apply(monitor.Update{ID: "sensor.t", Category: monitor.CategoryTemperature, Name: "N", Raw: "21", Unit: "", LastChanged: f.now, LastUpdated: f.now}, f.now)
	other := legacyRule(f, "sensor.gone", KindAbove, 1)
	if _, err := f.svc.ConfirmUnit(ctx, admin, other); !errors.Is(err, ErrNotMonitored) {
		t.Errorf("an unmonitored sensor cannot be confirmed: %v", err)
	}
}

func TestStore_marksAndTheSummaryTime(t *testing.T) {
	f := newFixture(t)
	f.ready("binary_sensor.leak", monitor.CategoryLeak)
	id, err := f.store.InsertRule(ctx, Rule{Entity: "binary_sensor.leak", Kind: KindState, Enabled: true, CreatedBy: "l", CreatedAt: f.now, UpdatedBy: "l", UpdatedAt: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.GetMark(ctx, id); ok || err != nil {
		t.Fatalf("no mark yet: %v %v", ok, err)
	}
	at := time.Date(2026, 10, 30, 12, 0, 0, 123, time.UTC)
	if err := f.store.PutMark(ctx, Mark{RuleID: id, EpisodeKey: "2026-10-30T11:00:00.123456789Z", DeliveredAt: at}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutMark(ctx, Mark{RuleID: id, EpisodeKey: "second", DeliveredAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	m, ok, err := f.store.GetMark(ctx, id)
	if err != nil || !ok || m.EpisodeKey != "second" || !m.DeliveredAt.Equal(at.Add(time.Hour).Truncate(time.Microsecond)) {
		t.Errorf("a rule has one mark, replaced by the latest delivery: %+v %v %v", m, ok, err)
	}
	if err := f.store.PutMark(ctx, Mark{RuleID: 999, EpisodeKey: "x", DeliveredAt: at}); err == nil {
		t.Error("a mark for a missing rule is refused")
	}
	if ok, err := f.store.SetStartupSummary(ctx, testGuild, at); ok || err != nil {
		t.Errorf("there are no settings yet, so there is nothing to update: %v %v", ok, err)
	}
	if err := f.svc.SetChannel(ctx, admin, testChannel); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := f.store.GetSettings(ctx, testGuild); !st.StartupSummaryAt.IsZero() {
		t.Error("a new configuration has never sent a summary")
	}
	if ok, err := f.store.SetStartupSummary(ctx, testGuild, at); !ok || err != nil {
		t.Fatalf("set summary time: %v %v", ok, err)
	}
	if err := f.svc.SetChannel(ctx, admin, "300000000000000003"); err != nil {
		t.Fatal(err)
	}
	st, _, _ := f.store.GetSettings(ctx, testGuild)
	if !st.StartupSummaryAt.Equal(at.Truncate(time.Microsecond)) {
		t.Errorf("changing the channel must not erase the summary time: %v", st.StartupSummaryAt)
	}
}
