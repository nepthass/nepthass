//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

// Real Home Assistant, virtual devices, a recording sender. Time-based tests use an
// injected clock for NeptHass; Home Assistant itself runs in real time.

const (
	garageDoor = "binary_sensor.nepthass_dev_garage_door"
	opening    = "binary_sensor.nepthass_dev_opening"
)

// ruleV2 adds a rule with a duration and/or a clear value.
func (r *nrig) ruleV2(t *testing.T, n notify.NewRule) notify.Rule {
	t.Helper()
	r.allow(t, string(n.Entity), entities.AccessRead)
	rule, err := r.svc.AddRule(context.Background(), adminActor, n)
	if err != nil {
		t.Fatalf("AddRule(%s): %v", n.Entity, err)
	}
	if err := r.eng.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return rule
}

// settle lets the engine's first health sample see a healthy monitor, so durations may fire.
func settle(t *testing.T, clk *itClock) {
	t.Helper()
	clk.await(t, 1)
	clk.advance(time.Second)
	clk.await(t, 1)
}

func setTemperature(t *testing.T, v float64) {
	t.Helper()
	if err := target.admin.call(context.Background(), "input_number", "set_value", map[string]any{"entity_id": "input_number.nepthass_dev_sim_temperature", "value": v}); err != nil {
		t.Fatal(err)
	}
}

func (r *nrig) waitTemperature(t *testing.T, v float64) {
	t.Helper()
	until(t, fmt.Sprintf("the monitor to see %v", v), func() bool {
		s, ok := r.mon.Tracker().Get(temperature)
		return ok && s.Interpretation().HasValue && s.Interpretation().Value == v
	})
}

func TestRules_garageDoorAndOpening_areRealClasses_thatAlertWhenTheyOpen(t *testing.T) {
	resetFor(t)
	r := startRig(t, rigOpts{configure: true})
	r.rule(t, garageDoor, notify.KindState, nil, 0)
	r.rule(t, opening, notify.KindState, nil, 0)
	scenario(t, "garage_door_opened")
	scenario(t, "opening_opened")
	got := r.snd.wait(t, 2)
	seen := map[monitor.Category]bool{}
	for _, m := range got {
		seen[m.Category] = m.Meaning == monitor.MeaningOpen && m.Kind == notify.MsgAlert && m.Lane() == notify.LaneNormal && m.Name != ""
	}
	if !seen[monitor.CategoryGarageDoor] || !seen[monitor.CategoryOpening] || len(r.snd.all()) != 2 {
		t.Fatalf("Home Assistant's garage_door and opening classes (on means open) must alert: %+v", got)
	}
}

func TestRules_aDurationRuleAgainstARealDoor_neverFiresEarly_andFiresOnce(t *testing.T) {
	resetFor(t)
	clk := newItClock()
	r := startRig(t, rigOpts{configure: true, clock: clk, mod: longTTL})
	settle(t, clk)
	r.ruleV2(t, notify.NewRule{Entity: door, Kind: notify.KindState, Duration: 10 * time.Minute})
	r.rule(t, motion, notify.KindState, nil, 0) // a barrier: its alert proves the evaluator got past the door
	scenario(t, "door_opened")
	until(t, "the count to start", func() bool { return r.eng.Health().PendingEpisodes == 1 })
	clk.advance(9*time.Minute + 59*time.Second)
	scenario(t, "motion_detected")
	first := r.snd.wait(t, 1)
	if first[0].Category != monitor.CategoryMotion || len(r.snd.all()) != 1 {
		t.Fatalf("a second before the duration only the barrier may have alerted: %+v", r.snd.all())
	}
	clk.advance(time.Second)
	got := r.snd.wait(t, 2)
	m := got[1]
	if m.Category != monitor.CategoryDoor || m.Duration != 10*time.Minute || m.HeldAtLeast || m.Meaning != monitor.MeaningOpen || m.Kind != notify.MsgAlert {
		t.Fatalf("the duration alert: %+v", m)
	}
	clk.advance(time.Hour)
	if len(r.snd.all()) != 2 || r.eng.Health().PendingEpisodes != 0 {
		t.Errorf("exactly one alert per episode: %d", len(r.snd.all()))
	}
}

func TestRules_aDoorThatClosesBeforeTheDurationNeverAlerts(t *testing.T) {
	resetFor(t)
	clk := newItClock()
	r := startRig(t, rigOpts{configure: true, clock: clk, mod: longTTL})
	settle(t, clk)
	r.ruleV2(t, notify.NewRule{Entity: door, Kind: notify.KindState, Duration: 10 * time.Minute})
	r.rule(t, motion, notify.KindState, nil, 0)
	scenario(t, "door_opened")
	until(t, "the count to start", func() bool { return r.eng.Health().PendingEpisodes == 1 })
	clk.advance(5 * time.Minute)
	if err := target.admin.call(context.Background(), "input_boolean", "turn_off", map[string]any{"entity_id": "input_boolean.nepthass_dev_sim_door_open"}); err != nil {
		t.Fatal(err)
	}
	until(t, "the count to end", func() bool { return r.eng.Health().PendingEpisodes == 0 })
	clk.advance(30 * time.Minute)
	scenario(t, "motion_detected")
	got := r.snd.wait(t, 1)
	if got[0].Category != monitor.CategoryMotion || len(r.snd.all()) != 1 || r.eng.Health().EpisodesFired != 0 || r.eng.Health().EpisodesCancelled != 1 {
		t.Fatalf("a door closed after 5 of 10 minutes sends nothing: %+v %+v", r.snd.all(), r.eng.Health())
	}
}

func TestRules_hysteresisAgainstTheRealTemperature_alertsOnlyAfterAClear(t *testing.T) {
	resetFor(t)
	r := startRig(t, rigOpts{configure: true})
	r.allow(t, temperature, entities.AccessRead)
	r.ruleV2(t, notify.NewRule{Entity: temperature, Kind: notify.KindAbove, Threshold: f64(30), Clear: f64(28), Cooldown: durPtr(0)})
	r.rule(t, door, notify.KindState, nil, 0) // the barrier
	steps := []struct {
		v     float64
		alert bool
	}{{31, true}, {29, false}, {31, false}, {28, false}, {31, false}, {27, false}, {31, true}}
	want := 0
	for _, s := range steps {
		setTemperature(t, s.v)
		r.waitTemperature(t, s.v)
		if s.alert {
			want++
			r.snd.wait(t, want)
		}
	}
	scenario(t, "door_opened") // every temperature change above has been evaluated before this one
	got := r.snd.wait(t, want+1)
	if len(got) != 3 || got[0].Category != monitor.CategoryTemperature || got[1].Category != monitor.CategoryTemperature || got[2].Category != monitor.CategoryDoor {
		t.Fatalf("31 alerts, then nothing for 29, 31, 28, 31 and 27, then 31 alerts again, then the barrier: %+v", got)
	}
	if got[0].Unit != "°C" || !got[0].HasValue {
		t.Errorf("the alert carries the bound unit: %+v", got[0])
	}
}

// longTTL is for tests that move the engine's fake clock far ahead: the monitor stamps changes with
// real time, so the default 10 minute TTL would count them as expired.
func longTTL(c *notify.EngineConfig) { c.TTL = 48 * time.Hour }

// waitMark waits until the delivery mark of a hazard alert has been written: NeptHass writes it
// only after the delivery is confirmed, which is after the sender has received the message.
func (r *nrig) waitMark(t *testing.T) {
	t.Helper()
	until(t, "the delivery mark to be written", func() bool {
		var n int
		return r.db.QueryRow("SELECT count(*) FROM notification_marks").Scan(&n) == nil && n == 1
	})
}

func durPtr(d time.Duration) *time.Duration { return &d }

func TestRules_aLeakThatIsAlreadyWetAtStartIsAnnouncedOnce_andNotAgainAfterARestart(t *testing.T) {
	resetFor(t)
	path := filepath.Join(t.TempDir(), "hazard.db")
	first := startRig(t, rigOpts{path: path, configure: true})
	first.rule(t, waterLeak, notify.KindState, nil, 0)
	scenario(t, "water_leak")
	if m := first.snd.wait(t, 1)[0]; m.Initial || m.Category != monitor.CategoryLeak {
		t.Fatalf("a leak that starts while NeptHass watches is an ordinary alert: %+v", m)
	}
	first.waitMark(t)
	// "Restart" with the leak still wet: a new engine and monitor on the same database.
	second := startRig(t, rigOpts{path: path})
	second.rule(t, door, notify.KindState, nil, 0) // a barrier on the new process
	scenario(t, "door_opened")
	got := second.snd.wait(t, 1)
	if got[0].Category != monitor.CategoryDoor || len(second.snd.all()) != 1 {
		t.Fatalf("the same wet episode must not be announced again after a restart: %+v", second.snd.all())
	}
	if h := second.eng.Health(); h.MarkSuppressed != 1 || h.InitialAlerts != 0 {
		t.Errorf("the persisted delivery mark suppressed it: %+v", h)
	}
}

func TestRules_aLeakWetAtStartWithNoMarkIsAnnouncedAsWhatHomeAssistantReports(t *testing.T) {
	resetFor(t)
	path := filepath.Join(t.TempDir(), "fresh-hazard.db")
	first := startRig(t, rigOpts{path: path, configure: true})
	first.rule(t, waterLeak, notify.KindState, nil, 0)
	scenario(t, "water_leak")
	first.snd.wait(t, 1)
	first.waitMark(t)
	// A database that never delivered an alert for it (the mark is gone): the next start announces it.
	if _, err := first.db.Exec("DELETE FROM notification_marks"); err != nil {
		t.Fatal(err)
	}
	second := startRig(t, rigOpts{path: path})
	defer func() {
		if t.Failed() {
			t.Logf("health: %+v", second.eng.Health())
		}
	}()
	m := second.snd.wait(t, 1)[0]
	if !m.Initial || m.Category != monitor.CategoryLeak || m.Meaning != monitor.MeaningWet || m.Lane() != notify.LaneCritical || m.ReportedAt.IsZero() || m.Late || m.Uncertain {
		t.Fatalf("a wet leak found at start is announced once, as an initial-state alert on the critical lane: %+v", m)
	}
}

func TestRules_openingsAlreadyOpenAtStartAreListedInOneSummary(t *testing.T) {
	resetFor(t)
	path := filepath.Join(t.TempDir(), "summary.db")
	first := startRig(t, rigOpts{path: path, configure: true})
	for _, id := range []string{door, window, garageDoor} {
		first.rule(t, id, notify.KindState, nil, 0)
	}
	first.rule(t, motion, notify.KindState, nil, 0)
	for _, s := range []string{"door_opened", "window_opened", "garage_door_opened"} {
		scenario(t, s)
	}
	first.snd.wait(t, 3) // the first process saw them open, as events

	clk := newItClock()
	second := startRig(t, rigOpts{path: path, clock: clk, mod: longTTL})
	defer func() {
		if t.Failed() {
			t.Logf("health: %+v", second.eng.Health())
		}
	}()
	settle(t, clk)
	// The snapshot is evaluated in batches: wait until all three are listed before moving the clock past the window.
	until(t, "the three open sensors to be listed", func() bool { return second.eng.Health().StartupListed == 3 })
	if n := len(second.snd.all()); n != 0 {
		t.Fatalf("no individual alert for what was already open: %d", n)
	}
	clk.advance(2 * time.Minute)
	got := second.snd.wait(t, 1)
	if got[0].Kind != notify.MsgStartup || len(got[0].Lines) != 3 || got[0].Lane() != notify.LaneNormal {
		t.Fatalf("one summary listing the three open sensors: %+v", got[0])
	}
	scenario(t, "motion_detected") // a barrier: nothing else was queued before it
	second.snd.wait(t, 2)
	if all := second.snd.all(); len(all) != 2 || all[1].Category != monitor.CategoryMotion {
		t.Fatalf("only the summary and the barrier: %+v", all)
	}
}

func (r *nrig) ruleStatus(t *testing.T, id int64) notify.RuleStatus {
	t.Helper()
	views, err := r.svc.Rules(context.Background(), adminActor)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.ID == id {
			return v.Status
		}
	}
	t.Fatalf("rule %d is not listed", id)
	return ""
}

// A real unit change: the instance is switched to US customary units, so Home Assistant itself
// reports the (still template-defined, Celsius) sensor in Fahrenheit. A threshold written in
// Celsius must never be compared with 95, and the rule must resume when the unit returns.
func TestRules_aRealUnitChangeMakesABoundRuleDormant_andItResumesWhenTheUnitReturns(t *testing.T) {
	resetFor(t) // its cleanup also puts the instance back on metric units
	r := startRig(t, rigOpts{configure: true})
	rule := r.rule(t, temperature, notify.KindAbove, f64(30), 0)
	if rule.Unit != "°C" {
		t.Fatalf("the rule is bound to the unit the sensor reported when it was created: %q", rule.Unit)
	}
	r.rule(t, door, notify.KindState, nil, 0) // a barrier: its alert proves the evaluator got past the temperature

	if err := wsCommand(context.Background(), map[string]any{"type": "config/core/update", "unit_system": "us_customary"}); err != nil {
		t.Fatalf("switch to US customary: %v", err)
	}
	setTemperature(t, 35) // 35 °C, which Home Assistant now reports as 95 °F
	until(t, "Home Assistant to report the temperature in °F", func() bool {
		s, ok := r.mon.Tracker().Get(temperature)
		return ok && s.Unit == "°F" && s.Interpretation().HasValue && s.Interpretation().Value > 90
	})
	if got := r.ruleStatus(t, rule.ID); got != notify.StatusUnitChanged {
		t.Fatalf("the rule must say its sensor changed unit: %q", got)
	}
	scenario(t, "door_opened")
	if got := r.snd.wait(t, 1); got[0].Category != monitor.CategoryDoor || len(r.snd.all()) != 1 {
		t.Fatalf("95 °F must never be compared with a threshold of 30 °C: %+v", r.snd.all())
	}

	if err := wsCommand(context.Background(), map[string]any{"type": "config/core/update", "unit_system": "metric"}); err != nil {
		t.Fatalf("switch back to metric: %v", err)
	}
	setTemperature(t, 20) // a fresh reading in °C, below the threshold
	r.waitTemperature(t, 20)
	until(t, "the rule to resume", func() bool { return r.ruleStatus(t, rule.ID) == notify.StatusActive })
	setTemperature(t, 36)
	r.waitTemperature(t, 36)
	got := r.snd.wait(t, 2)
	if m := got[1]; m.Category != monitor.CategoryTemperature || m.Unit != "°C" || !m.HasValue || m.Value != 36 {
		t.Fatalf("after the unit returned the rule evaluates again, in °C: %+v", m)
	}
}

// A real outage (the WebSocket connection is cut) while a duration is pending. NeptHass cannot prove
// the door stayed open while it could not look, so nothing fires during the outage, the count starts
// again when monitoring is back, and the alert says "at least".
func TestRules_aRealOutageNeverFiresAPendingDuration_andTheCountRestartsAfterRecovery(t *testing.T) {
	resetFor(t)
	clk := newItClock()
	proxy := newTCPProxy(t, target.url)
	r := startRig(t, rigOpts{configure: true, monURL: proxy.URL(), clock: clk, mod: func(c *notify.EngineConfig) {
		c.HealthTick = 10 * time.Second
		longTTL(c)
	}})
	clk.await(t, 1)
	clk.advance(10 * time.Second) // the first tick that sees a healthy monitor
	clk.await(t, 1)
	r.ruleV2(t, notify.NewRule{Entity: door, Kind: notify.KindState, Duration: 10 * time.Minute})
	r.rule(t, motion, notify.KindState, nil, 0) // barriers: their alerts are ordered after anything the evaluator fired earlier
	r.rule(t, window, notify.KindState, nil, 0)
	scenario(t, "door_opened")
	until(t, "the count to start", func() bool { return r.eng.Health().PendingEpisodes == 1 })
	clk.await(t, 2) // the health watcher's tick and the evaluator's due timer

	proxy.SetDown(true)
	waitMonitor(t, r.mon, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting })
	clk.advance(10 * time.Second) // one tick that samples an unhealthy monitor
	clk.await(t, 2)
	clk.advance(12 * time.Minute) // far past the ten minutes the door has been open
	if down := r.snd.wait(t, 1)[0]; down.Kind != notify.MsgMonitoringDown {
		t.Fatalf("the outage is announced once monitoring has been down five minutes: %+v", down)
	}

	proxy.SetDown(false)
	waitMonitor(t, r.mon, func(s monitor.Status) bool { return healthy(s) && s.Reconnects >= 1 })
	clk.await(t, 1)
	clk.advance(10 * time.Second) // the tick that sees monitoring is back and restarts the count
	r.snd.wait(t, 2)
	scenario(t, "motion_detected")
	got := r.snd.wait(t, 3)
	if got[1].Kind != notify.MsgMonitoringRecovered || got[2].Category != monitor.CategoryMotion || len(r.snd.all()) != 3 {
		t.Fatalf("down, recovered, then only the barrier: no duration alert fired during or right after the outage: %+v", r.snd.all())
	}

	clk.await(t, 2)
	clk.advance(9*time.Minute + 59*time.Second)
	scenario(t, "window_opened")
	if got = r.snd.wait(t, 4); got[3].Category != monitor.CategoryWindow || len(r.snd.all()) != 4 {
		t.Fatalf("a second before the restarted duration only the barrier may have alerted: %+v", r.snd.all())
	}
	clk.advance(time.Second)
	got = r.snd.wait(t, 5)
	if m := got[4]; m.Category != monitor.CategoryDoor || m.Duration != 10*time.Minute || !m.HeldAtLeast || m.Kind != notify.MsgAlert {
		t.Fatalf("the restarted count fires once and says \"at least\": %+v", m)
	}
}
