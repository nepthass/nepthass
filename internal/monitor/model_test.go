package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

var base = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func ts(ms int) string {
	return base.Add(time.Duration(ms) * time.Millisecond).Format(time.RFC3339Nano)
}

func stateJSON(id, state string, attrs map[string]any, changedMs, updatedMs int) json.RawMessage {
	if attrs == nil {
		attrs = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"entity_id": id, "state": state, "attributes": attrs, "last_changed": ts(changedMs), "last_updated": ts(updatedMs)})
	return b
}

func eventJSON(id string, oldState, newState json.RawMessage) json.RawMessage {
	pick := func(r json.RawMessage) any {
		if r == nil {
			return nil
		}
		return r
	}
	b, _ := json.Marshal(map[string]any{"event_type": "state_changed", "data": map[string]any{"entity_id": id, "old_state": pick(oldState), "new_state": pick(newState)}})
	return b
}

func TestClassify_usesDomainAndDeviceClassOnly(t *testing.T) {
	tests := []struct {
		domain, class string
		want          Category
		ok            bool
	}{
		{"sensor", "temperature", CategoryTemperature, true},
		{"sensor", "humidity", CategoryHumidity, true},
		{"sensor", "battery", CategoryBattery, true},
		{"binary_sensor", "motion", CategoryMotion, true},
		{"binary_sensor", "door", CategoryDoor, true},
		{"binary_sensor", "window", CategoryWindow, true},
		{"binary_sensor", "moisture", CategoryLeak, true},
		{"binary_sensor", "battery", CategoryBatteryLow, true},
		{"sensor", "", "", false},                   // no device class: not classified, whatever the ID says
		{"sensor", "power", "", false},              // a class we do not monitor
		{"sensor", "door", "", false},               // right class, wrong domain
		{"binary_sensor", "temperature", "", false}, // right class, wrong domain
		{"light", "temperature", "", false},
		{"sensor", "Temperature", "", false}, // exact, case-sensitive match on untrusted input
	}
	for _, tt := range tests {
		t.Run(tt.domain+"/"+tt.class, func(t *testing.T) {
			got, ok := Classify(tt.domain, tt.class)
			if got != tt.want || ok != tt.ok {
				t.Errorf("Classify(%q, %q) = %q, %v", tt.domain, tt.class, got, ok)
			}
		})
	}
	if !Monitored("sensor") || !Monitored("binary_sensor") || Monitored("light") || Monitored("switch") {
		t.Error("Monitored() domain set is wrong")
	}
}

func TestInterpret_separatesRawStateFromMeaning(t *testing.T) {
	tests := []struct {
		cat  Category
		raw  string
		want Meaning
	}{
		{CategoryMotion, "on", MeaningDetected}, {CategoryMotion, "off", MeaningClear},
		{CategoryDoor, "on", MeaningOpen}, {CategoryDoor, "off", MeaningClosed},
		{CategoryWindow, "on", MeaningOpen}, {CategoryWindow, "off", MeaningClosed},
		{CategoryLeak, "on", MeaningWet}, {CategoryLeak, "off", MeaningDry},
		{CategoryBatteryLow, "on", MeaningLow}, {CategoryBatteryLow, "off", MeaningNormal},
		{CategoryDoor, "unavailable", MeaningUnavailable}, {CategoryMotion, "unknown", MeaningUnknown},
		{CategoryTemperature, "unavailable", MeaningUnavailable}, {CategoryHumidity, "unknown", MeaningUnknown},
		{CategoryDoor, "maybe", MeaningInvalid}, {CategoryLeak, "", MeaningInvalid}, {CategoryMotion, "ON", MeaningInvalid},
		{CategoryTemperature, "21.5", MeaningReading}, {CategoryTemperature, "-3", MeaningReading}, {CategoryTemperature, "1e1", MeaningReading},
		{CategoryHumidity, "45", MeaningReading}, {CategoryHumidity, "100", MeaningReading}, {CategoryBattery, "0", MeaningReading},
		{CategoryHumidity, "101", MeaningInvalid}, {CategoryBattery, "-1", MeaningInvalid},
		{CategoryTemperature, "abc", MeaningInvalid}, {CategoryTemperature, "", MeaningInvalid}, {CategoryTemperature, " 5", MeaningInvalid},
		{CategoryTemperature, "NaN", MeaningInvalid}, {CategoryTemperature, "Inf", MeaningInvalid}, {CategoryTemperature, "-Inf", MeaningInvalid},
		{CategoryTemperature, "0x10", MeaningInvalid}, {CategoryTemperature, "1_000", MeaningInvalid}, {CategoryTemperature, "0x1p-2", MeaningInvalid},
	}
	for _, tt := range tests {
		t.Run(string(tt.cat)+"/"+tt.raw, func(t *testing.T) {
			got := Interpret(tt.cat, tt.raw)
			if got.Meaning != tt.want {
				t.Errorf("Interpret(%s, %q) = %s, want %s", tt.cat, tt.raw, got.Meaning, tt.want)
			}
			if tt.want == MeaningReading && (!got.HasValue) {
				t.Errorf("a reading must carry its parsed value")
			}
		})
	}
	if v := Interpret(CategoryTemperature, "21.5"); v.Value != 21.5 {
		t.Errorf("value = %v", v.Value)
	}
	for _, m := range []Meaning{MeaningUnavailable, MeaningUnknown, MeaningInvalid} {
		if m.Reliable() {
			t.Errorf("%s must not count as a real reading", m)
		}
	}
	if MeaningClosed.Text(CategoryDoor) == "" || !MeaningClosed.Reliable() {
		t.Error("closed must be a reliable meaning")
	}
}

func TestParseState_validatesUntrustedInput(t *testing.T) {
	long := strings.Repeat("x", 300)
	tests := []struct {
		name    string
		raw     json.RawMessage
		wantErr error
		check   func(t *testing.T, u Update)
	}{
		{"motion sensor", stateJSON("binary_sensor.hall", "on", map[string]any{"device_class": "motion", "friendly_name": "Hall"}, 1, 2), nil, func(t *testing.T, u Update) {
			if u.Category != CategoryMotion || u.Raw != "on" || u.Name != "Hall" || !u.LastUpdated.Equal(base.Add(2*time.Millisecond)) {
				t.Errorf("update = %+v", u)
			}
		}},
		{"temperature with unit", stateJSON("sensor.t", "21.5", map[string]any{"device_class": "temperature", "unit_of_measurement": "\u00b0C"}, 1, 1), nil, func(t *testing.T, u Update) {
			if u.Unit != "\u00b0C" || u.Category != CategoryTemperature {
				t.Errorf("update = %+v", u)
			}
		}},
		{"missing attributes", stateJSON("sensor.bare", "5", nil, 1, 1), nil, func(t *testing.T, u Update) {
			if u.Category != "" || u.Name != "" {
				t.Errorf("an entity without a device class must not be classified: %+v", u)
			}
		}},
		{"attribute of the wrong type is ignored", stateJSON("sensor.odd", "5", map[string]any{"device_class": 42, "friendly_name": []int{1}, "unit_of_measurement": true}, 1, 1), nil, func(t *testing.T, u Update) {
			if u.Category != "" || u.DeviceClass != "" || u.Unit != "" || u.Name != "" {
				t.Errorf("update = %+v", u)
			}
		}},
		{"unavailable is kept raw", stateJSON("binary_sensor.d", "unavailable", map[string]any{"device_class": "door"}, 1, 1), nil, func(t *testing.T, u Update) {
			if u.Raw != "unavailable" || Interpret(u.Category, u.Raw).Meaning != MeaningUnavailable {
				t.Errorf("update = %+v", u)
			}
		}},
		{"name truncated", stateJSON("sensor.n", "1", map[string]any{"device_class": "humidity", "friendly_name": strings.Repeat("\u063a", 500)}, 1, 1), nil, func(t *testing.T, u Update) {
			if n := len([]rune(u.Name)); n != maxNameRunes {
				t.Errorf("name length = %d runes, want %d", n, maxNameRunes)
			}
		}},
		{"huge unrelated attributes are skipped", stateJSON("sensor.big", "1", map[string]any{"device_class": "battery", "forecast": strings.Repeat("y", 100000)}, 1, 1), nil, nil},
		{"other domain ignored", stateJSON("light.kitchen", "on", nil, 1, 1), errIgnored, nil},
		{"invalid entity id", stateJSON("Sensor.Upper", "1", nil, 1, 1), ErrInvalid, nil},
		{"entity id without a dot", stateJSON("sensor", "1", nil, 1, 1), ErrInvalid, nil},
		{"state is not a string", json.RawMessage(`{"entity_id":"sensor.x","state":5,"attributes":{},"last_changed":"` + ts(0) + `","last_updated":"` + ts(0) + `"}`), ErrInvalid, nil},
		{"state too long", stateJSON("sensor.x", long, nil, 1, 1), ErrInvalid, nil},
		{"unit too long", stateJSON("sensor.x", "1", map[string]any{"unit_of_measurement": strings.Repeat("u", 40)}, 1, 1), ErrInvalid, nil},
		{"bad timestamp", json.RawMessage(`{"entity_id":"sensor.x","state":"1","attributes":{},"last_changed":"yesterday","last_updated":"` + ts(0) + `"}`), ErrInvalid, nil},
		{"missing timestamp", json.RawMessage(`{"entity_id":"sensor.x","state":"1","attributes":{}}`), ErrInvalid, nil},
		{"not an object", json.RawMessage(`[1,2,3]`), ErrInvalid, nil},
		{"attributes is a list", json.RawMessage(`{"entity_id":"sensor.x","state":"1","attributes":[1],"last_changed":"` + ts(0) + `","last_updated":"` + ts(0) + `"}`), ErrInvalid, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := ParseState(tt.raw)

			if !errors.Is(err, tt.wantErr) && !(tt.wantErr == nil && err == nil) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.check != nil && err == nil {
				tt.check(t, u)
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	oldS := stateJSON("binary_sensor.d", "off", map[string]any{"device_class": "door"}, 1, 1)
	newS := stateJSON("binary_sensor.d", "on", map[string]any{"device_class": "door"}, 5, 5)
	t.Run("state change", func(t *testing.T) {
		u, err := ParseEvent(eventJSON("binary_sensor.d", oldS, newS))
		if err != nil || u.Removed || u.Raw != "on" || u.Category != CategoryDoor {
			t.Errorf("update = %+v, %v", u, err)
		}
	})
	t.Run("new entity (no old state)", func(t *testing.T) {
		u, err := ParseEvent(eventJSON("binary_sensor.d", nil, newS))
		if err != nil || u.Raw != "on" {
			t.Errorf("update = %+v, %v", u, err)
		}
	})
	t.Run("removal carries the removed state's timestamp", func(t *testing.T) {
		u, err := ParseEvent(eventJSON("binary_sensor.d", oldS, nil))
		if err != nil || !u.Removed || !u.PrevUpdated.Equal(base.Add(time.Millisecond)) {
			t.Errorf("update = %+v, %v", u, err)
		}
	})
	t.Run("removal without any state is invalid", func(t *testing.T) {
		if _, err := ParseEvent(eventJSON("binary_sensor.d", nil, nil)); !errors.Is(err, ErrInvalid) {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("entity id mismatch is invalid", func(t *testing.T) {
		if _, err := ParseEvent(eventJSON("binary_sensor.other", oldS, newS)); !errors.Is(err, ErrInvalid) {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("other event types are ignored", func(t *testing.T) {
		if _, err := ParseEvent(json.RawMessage(`{"event_type":"call_service","data":{}}`)); !errors.Is(err, errIgnored) {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("other domains are ignored without decoding their states", func(t *testing.T) {
		if _, err := ParseEvent(eventJSON("light.kitchen", nil, stateJSON("light.kitchen", "on", nil, 1, 1))); !errors.Is(err, errIgnored) {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("unexpected entity id", func(t *testing.T) {
		for _, id := range []string{"", "no_dot", "SENSOR.x", "sensor.has space", "sensor.x'; DROP TABLE x;--", "sensor." + strings.Repeat("a", 300)} {
			if _, err := ParseEvent(eventJSON(id, nil, newS)); !errors.Is(err, ErrInvalid) {
				t.Errorf("id %q: error = %v, want ErrInvalid", id, err)
			}
		}
	})
	t.Run("malformed", func(t *testing.T) {
		for _, raw := range []string{``, `null`, `{`, `"text"`, `{"event_type":"state_changed","data":5}`} {
			if _, err := ParseEvent(json.RawMessage(raw)); err == nil {
				t.Errorf("%q was accepted", raw)
			}
		}
	})
}

func upd(id, raw string, changedMs, updatedMs int) Update {
	c := entities.ID(id)
	cat, _ := Classify(string(c.Domain()), "temperature")
	if c.Domain() == "binary_sensor" {
		cat = CategoryDoor
	}
	return Update{ID: c, Category: cat, Raw: raw, Unit: "u", LastChanged: base.Add(time.Duration(changedMs) * time.Millisecond), LastUpdated: base.Add(time.Duration(updatedMs) * time.Millisecond)}
}

func TestTracker_appliesUpdatesInOrder_andNeverAssumesExactlyOnceDelivery(t *testing.T) {
	tr := NewTracker()
	now := base.Add(time.Hour)

	added := tr.Apply(upd("sensor.t", "20", 10, 10), now)
	dup := tr.Apply(upd("sensor.t", "20", 10, 10), now)
	newer := tr.Apply(upd("sensor.t", "21", 20, 20), now)
	stale := tr.Apply(upd("sensor.t", "19", 5, 5), now)
	attrOnly := tr.Apply(Update{ID: "sensor.t", Category: CategoryTemperature, Raw: "21", Unit: "v", LastChanged: base.Add(20 * time.Millisecond), LastUpdated: base.Add(30 * time.Millisecond)}, now)
	got, _ := tr.Get("sensor.t")

	if added.Outcome != OutcomeApplied || !added.ValueChanged {
		t.Errorf("first update = %+v", added)
	}
	if dup.Outcome != OutcomeDuplicate || stale.Outcome != OutcomeStale {
		t.Errorf("duplicate=%+v stale=%+v", dup, stale)
	}
	if newer.Outcome != OutcomeApplied || !newer.ValueChanged {
		t.Errorf("newer update = %+v", newer)
	}
	if attrOnly.Outcome != OutcomeApplied || attrOnly.ValueChanged {
		t.Errorf("an attribute-only update must apply without a value change: %+v", attrOnly)
	}
	if got.Raw != "21" || got.Unit != "v" || !got.LastChanged.Equal(base.Add(20*time.Millisecond)) || !got.LastUpdated.Equal(base.Add(30*time.Millisecond)) || got.Revision != 3 || !got.ObservedAt.Equal(now) {
		t.Errorf("stored = %+v", got)
	}
	if c := tr.Counters(); c.Duplicates != 1 || c.Stale != 1 || c.Applied != 3 {
		t.Errorf("counters = %+v", c)
	}
}

func TestTracker_sameTimestampButDifferentContent_isNotADuplicate(t *testing.T) {
	tr := NewTracker()
	tr.Apply(upd("sensor.t", "20", 10, 10), base)

	res := tr.Apply(upd("sensor.t", "25", 10, 10), base)

	if res.Outcome != OutcomeApplied || !res.ValueChanged {
		t.Errorf("result = %+v: a conflicting update at the same instant must not be silently dropped", res)
	}
}

func TestTracker_handlesRemovalAndReappearance_withoutHonoringStaleRemovals(t *testing.T) {
	tr := NewTracker()
	tr.Apply(upd("sensor.t", "20", 10, 50), base)

	stale := tr.Apply(Update{ID: "sensor.t", Removed: true, PrevUpdated: base.Add(10 * time.Millisecond)}, base)
	_, stillThere := tr.Get("sensor.t")
	removed := tr.Apply(Update{ID: "sensor.t", Removed: true, PrevUpdated: base.Add(50 * time.Millisecond)}, base)
	_, gone := tr.Get("sensor.t")
	unknown := tr.Apply(Update{ID: "sensor.never", Removed: true}, base)
	back := tr.Apply(upd("sensor.t", "22", 60, 60), base)
	again, _ := tr.Get("sensor.t")

	if stale.Outcome != OutcomeStale || !stillThere {
		t.Errorf("a removal older than the stored state must be ignored: %+v present=%v", stale, stillThere)
	}
	if removed.Outcome != OutcomeRemoved || gone {
		t.Errorf("removal = %+v present=%v", removed, gone)
	}
	if unknown.Outcome != OutcomeUntracked {
		t.Errorf("removing an unknown sensor = %+v", unknown)
	}
	if back.Outcome != OutcomeApplied || again.Revision != 1 || again.Raw != "22" {
		t.Errorf("reappearance = %+v %+v", back, again)
	}
}

func TestTracker_stopsTrackingASensorWhoseClassNoLongerClassifies(t *testing.T) {
	tr := NewTracker()
	tr.Apply(upd("sensor.t", "20", 1, 1), base)

	res := tr.Apply(Update{ID: "sensor.t", Category: "", Raw: "20", LastUpdated: base.Add(time.Second)}, base)

	if _, ok := tr.Get("sensor.t"); ok || res.Outcome != OutcomeRemoved {
		t.Errorf("result = %+v", res)
	}
	if res := tr.Apply(Update{ID: "sensor.other", Category: ""}, base); res.Outcome != OutcomeUntracked {
		t.Errorf("an unclassified sensor must stay untracked: %+v", res)
	}
}

func TestTracker_isBounded(t *testing.T) {
	tr := NewTracker()
	for i := range MaxTracked + 5 {
		tr.Apply(upd(fmt.Sprintf("sensor.s%d", i), "1", 1, 1), base)
	}

	if tr.Len() != MaxTracked || tr.Counters().Dropped != 5 {
		t.Errorf("len=%d dropped=%d, want %d and 5", tr.Len(), tr.Counters().Dropped, MaxTracked)
	}
	if res := tr.Apply(upd("sensor.s0", "2", 5, 5), base); res.Outcome != OutcomeApplied {
		t.Errorf("a full tracker must still update the sensors it already holds: %+v", res)
	}
}

func TestReconcile_isAuthoritative_removesVanishedSensors_andOverridesNewerCachedValues(t *testing.T) {
	tr := NewTracker()
	tr.Apply(upd("sensor.kept", "1", 100, 100), base)
	tr.Apply(upd("sensor.changed", "1", 100, 900), base) // cached value is "newer" than the snapshot's (clock step)
	tr.Apply(upd("sensor.gone", "1", 100, 100), base)
	now := base.Add(time.Hour)

	r := tr.Reconcile([]Update{
		upd("sensor.kept", "1", 100, 100),
		upd("sensor.changed", "2", 200, 200),
		upd("sensor.fresh", "3", 300, 300),
		{ID: "sensor.skipped", Category: ""},
		upd("sensor.kept", "9", 999, 999), // a duplicate ID in one snapshot: first one wins
	}, now)

	if r.Added != 1 || r.Updated != 1 || r.Unchanged != 1 || r.Removed != 1 {
		t.Errorf("reconciled = %+v", r)
	}
	changed, _ := tr.Get("sensor.changed")
	kept, _ := tr.Get("sensor.kept")
	if changed.Raw != "2" || changed.Revision != 2 || kept.Raw != "1" || !kept.ObservedAt.Equal(now) {
		t.Errorf("changed=%+v kept=%+v: the snapshot must win regardless of timestamps", changed, kept)
	}
	if _, ok := tr.Get("sensor.gone"); ok || tr.Len() != 3 {
		t.Errorf("len=%d: sensors absent from the snapshot must be removed", tr.Len())
	}
}

func TestTracker_isSafeForConcurrentUse(t *testing.T) {
	tr := NewTracker()
	done := make(chan struct{})
	for w := range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 500 {
				tr.Apply(upd(fmt.Sprintf("sensor.s%d", i%20), fmt.Sprint(i), i, i+w), base)
				tr.Get("sensor.s1")
				_ = tr.All()
				_ = tr.Counters()
			}
		}()
	}
	go func() {
		defer func() { done <- struct{}{} }()
		for range 50 {
			tr.Reconcile([]Update{upd("sensor.s1", "x", 1, 1)}, base)
		}
	}()
	for range 5 {
		<-done
	}
}
