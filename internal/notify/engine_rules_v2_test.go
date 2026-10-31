package notify

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// f returns the embedded fixture.
func (e *ef) f() *fixture { return e.fixture }

// addV2 adds a rule through the service (so it is validated and bound to its
// sensor's unit) and reloads the engine.
func (e *ef) addV2(n NewRule) Rule {
	e.t.Helper()
	r, err := e.svc.AddRule(ctx, admin, n)
	if err != nil {
		e.t.Fatal(err)
	}
	e.reload()
	return r
}

// live applies one reading the way the monitor does: the tracker first (the
// state a due duration re-reads), then the change feed.
func (e *ef) live(id string, cat monitor.Category, prev, cur string) {
	e.t.Helper()
	e.monitored(id, cat, cur)
	e.emit(e.change(id, cat, prev, cur, monitor.SourceLive))
}

// step is live plus one evaluation and delivery.
func (e *ef) step(id string, cat monitor.Category, prev, cur string) {
	e.t.Helper()
	e.live(id, cat, prev, cur)
	e.eval()
	e.flush()
}

func (e *ef) tempRule(thr, clear float64, d time.Duration, start string) Rule {
	e.t.Helper()
	e.share("sensor.t", entities.AccessRead)
	e.monitored("sensor.t", monitor.CategoryTemperature, start)
	n := NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(thr), Duration: d, Cooldown: dur(0)}
	if clear != 0 {
		n.Clear = fl(clear)
	}
	return e.addV2(n)
}

func TestHysteresis_aRuleAlertsOnceUntilTheSensorClears(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.tempRule(30, 28, 0, "25")
	steps := []struct {
		val    string
		alerts int
		why    string
	}{
		{"31", 1, "crossing above 30 alerts"},
		{"29", 1, "inside the band: neither re-arms nor alerts"},
		{"31", 1, "back above 30 without having cleared: no alert"},
		{"28", 1, "exactly the clear value does not clear (strictly below)"},
		{"31", 1, "still not re-armed"},
		{"27.9", 1, "below the clear value re-arms"},
		{"31", 2, "crossing again alerts"},
		{"29.9", 2, "oscillating around the threshold ..."},
		{"30.1", 2, "... never alerts without a clear"},
		{"29.9", 2, ""},
		{"30.1", 2, ""},
		{"20", 2, "far below clears"},
		{"30", 2, "exactly the threshold is not above it"},
		{"30.0001", 3, "just above alerts"},
	}
	prev := "25"
	for _, s := range steps {
		e.step("sensor.t", monitor.CategoryTemperature, prev, s.val)
		if got := len(e.sent()); got != s.alerts {
			t.Fatalf("%s -> %s: %d alerts, want %d (%s)", prev, s.val, got, s.alerts, s.why)
		}
		prev = s.val
	}
}

func TestHysteresis_aBelowRuleIsMirrored(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.share("sensor.t", entities.AccessRead)
	e.monitored("sensor.t", monitor.CategoryTemperature, "10")
	e.addV2(NewRule{Entity: "sensor.t", Kind: KindBelow, Threshold: fl(5), Clear: fl(7), Cooldown: dur(0)})
	prev := "10"
	for _, s := range []struct {
		val    string
		alerts int
	}{{"4", 1}, {"6", 1}, {"4", 1}, {"7", 1}, {"4", 1}, {"7.1", 1}, {"4", 2}} {
		e.step("sensor.t", monitor.CategoryTemperature, prev, s.val)
		if got := len(e.sent()); got != s.alerts {
			t.Fatalf("%s -> %s: %d alerts, want %d", prev, s.val, got, s.alerts)
		}
		prev = s.val
	}
}

func TestHysteresis_aDipAndReturnInsideOneCoalescingWindow(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.tempRule(30, 28, 0, "25")
	e.step("sensor.t", monitor.CategoryTemperature, "25", "31") // first alert, now disarmed

	// Without a clear the window yields nothing, however many crossings it holds.
	e.live("sensor.t", monitor.CategoryTemperature, "31", "29")
	e.live("sensor.t", monitor.CategoryTemperature, "29", "31")
	e.eval()
	e.flush()
	if len(e.sent()) != 1 {
		t.Fatalf("a window with no clear must not re-arm: %d alerts", len(e.sent()))
	}
	// A dip below the clear value and a return, in ONE window, is a new alert.
	e.live("sensor.t", monitor.CategoryTemperature, "31", "29")
	e.live("sensor.t", monitor.CategoryTemperature, "29", "27")
	e.live("sensor.t", monitor.CategoryTemperature, "27", "31")
	e.eval()
	e.flush()
	if len(e.sent()) != 2 {
		t.Fatalf("a dip below the clear value inside one window must re-arm the rule: %d alerts", len(e.sent()))
	}
}

func TestHysteresis_theCooldownStillApplies(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.share("sensor.t", entities.AccessRead)
	e.monitored("sensor.t", monitor.CategoryTemperature, "25")
	e.addV2(NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(30), Clear: fl(28)}) // default cooldown: 15 minutes
	e.step("sensor.t", monitor.CategoryTemperature, "25", "31")
	e.step("sensor.t", monitor.CategoryTemperature, "31", "27")
	e.step("sensor.t", monitor.CategoryTemperature, "27", "31") // re-armed, but inside the cooldown
	if len(e.sent()) != 1 || e.eng.Health().SuppressedCooldown != 1 {
		t.Fatalf("a re-armed rule inside its cooldown is suppressed and counted: %d alerts, %d suppressed", len(e.sent()), e.eng.Health().SuppressedCooldown)
	}
	e.clk.Advance(16 * time.Minute)
	e.step("sensor.t", monitor.CategoryTemperature, "31", "27")
	e.step("sensor.t", monitor.CategoryTemperature, "27", "31")
	if got := e.sent(); len(got) != 2 || got[1].Muted != 1 {
		t.Fatalf("after the cooldown the alert goes out and reports what was muted: %+v", got)
	}
}

func TestHysteresis_afterARestartTheRuleStartsDisarmedWhenAlreadyAboveAndArmedInsideTheBand(t *testing.T) {
	// Already above the threshold when the rule is first seen: no event happened, and the rule must clear first.
	e := newEF(t, nil)
	e.configure()
	e.tempRule(30, 28, 0, "31")
	e.step("sensor.t", monitor.CategoryTemperature, "31", "29")
	e.step("sensor.t", monitor.CategoryTemperature, "29", "31")
	if len(e.sent()) != 0 {
		t.Fatalf("a rule seeded above its threshold waits for the sensor to clear: %d alerts", len(e.sent()))
	}
	e.step("sensor.t", monitor.CategoryTemperature, "31", "27")
	e.step("sensor.t", monitor.CategoryTemperature, "27", "31")
	if len(e.sent()) != 1 {
		t.Fatalf("after clearing it fires: %d", len(e.sent()))
	}
	// Seeded inside the band: armed, so the first crossing alerts.
	b := newEF(t, nil)
	b.configure()
	b.tempRule(30, 28, 0, "29")
	b.step("sensor.t", monitor.CategoryTemperature, "29", "31")
	if len(b.sent()) != 1 {
		t.Fatalf("a rule seeded inside the band is armed: %d", len(b.sent()))
	}
}

func TestHysteresis_unreliableReadingsNeverFireOrChangeTheState(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.tempRule(30, 28, 0, "25")
	e.step("sensor.t", monitor.CategoryTemperature, "25", "31")
	prev := "31"
	for _, raw := range []string{"unavailable", "31", "unknown", "NaN", "Inf", "-Inf", "", "1e3", "0x20", "hot", "31"} {
		e.step("sensor.t", monitor.CategoryTemperature, prev, raw)
		prev = raw
	}
	if len(e.sent()) != 1 {
		t.Fatalf("unavailable, unknown and malformed readings must not alert or re-arm a disarmed rule: %d alerts", len(e.sent()))
	}
}

func TestUnitSafety_aChangedUnitMakesTheRuleDormantUntilItReturns(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.share("sensor.t", entities.AccessRead)
	e.monitored("sensor.t", monitor.CategoryTemperature, "25") // bound to °C
	r := e.addV2(NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(30), Cooldown: dur(0)})
	if r.Unit != "°C" {
		t.Fatalf("bound unit = %q", r.Unit)
	}
	// Home Assistant switches the sensor to °F: 90 °F is 32 °C, but the threshold 30 was written in °C.
	prevSensor, _ := e.tracker.Get("sensor.t") // reading in °C
	e.f().now = e.f().now.Add(time.Second)
	e.tracker.Apply(monitor.Update{ID: "sensor.t", Category: monitor.CategoryTemperature, Name: "Name", Raw: "90", Unit: "°F", LastChanged: e.f().now, LastUpdated: e.f().now}, e.f().now)
	cur, _ := e.tracker.Get("sensor.t")
	e.emit(monitor.Change{ID: "sensor.t", Seq: 100, Source: monitor.SourceLive, HadPrev: true, Prev: prevSensor, Cur: cur, At: e.clk.Now()})
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatalf("a reading in another unit must never be compared with the threshold: %d alerts", len(e.sent()))
	}
	views, _ := e.svc.Rules(ctx, admin)
	if views[0].Status != StatusUnitChanged {
		t.Errorf("status = %s", views[0].Status)
	}
	// Back in °C, above the threshold: the previous state was in another unit, so this is silent like any unreliable-to-violating step.
	e.step("sensor.t", monitor.CategoryTemperature, "90", "35")
	if len(e.sent()) != 0 {
		t.Fatalf("the first reading after a unit change is a new baseline: %d", len(e.sent()))
	}
	e.step("sensor.t", monitor.CategoryTemperature, "35", "20")
	e.step("sensor.t", monitor.CategoryTemperature, "20", "35")
	if len(e.sent()) != 1 {
		t.Fatalf("once the unit is back the rule works again: %d", len(e.sent()))
	}
}

func TestUnitSafety_aLegacyRuleNeverEvaluatesUntilItIsBound(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.share("sensor.h", entities.AccessRead)
	e.monitored("sensor.h", monitor.CategoryHumidity, "40")
	id := legacyRule(e.f(), "sensor.h", KindBelow, 30)
	e.reload()
	e.step("sensor.h", monitor.CategoryHumidity, "40", "20")
	e.step("sensor.h", monitor.CategoryHumidity, "20", "40")
	e.step("sensor.h", monitor.CategoryHumidity, "40", "20")
	if len(e.sent()) != 0 {
		t.Fatalf("an unresolved legacy rule must not evaluate as if its threshold were valid: %d alerts", len(e.sent()))
	}
	if n, err := e.svc.BindLegacyUnits(ctx); err != nil || n != 1 {
		t.Fatalf("bind: %d %v", n, err)
	}
	e.reload()
	e.step("sensor.h", monitor.CategoryHumidity, "20", "40")
	e.step("sensor.h", monitor.CategoryHumidity, "40", "20")
	if len(e.sent()) != 1 {
		t.Fatalf("a bound rule evaluates: %d", len(e.sent()))
	}
	_ = id
}

func TestUnitSafety_theEngineBindsALegacyRuleByItselfWhenItIsUnambiguous(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.eng.d.Binder = e.svc
	e.share("sensor.h", entities.AccessRead)
	e.monitored("sensor.h", monitor.CategoryHumidity, "40")
	id := legacyRule(e.f(), "sensor.h", KindBelow, 30)
	e.reload()
	cancel, done, _ := e.startRun()
	defer func() { cancel(); <-done }()
	e.emit(e.change("sensor.h", monitor.CategoryHumidity, "", "40", monitor.SourceSnapshot))
	deadline := time.After(testWait)
	for {
		if r, _, _ := e.store.GetRule(ctx, id); r.Unit == "%" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the engine did not bind the legacy rule")
		case <-time.After(time.Millisecond): // poll the store, bounded
		}
	}
}

func TestDuration_firesOnlyAfterTheDuration_andOnlyOnce(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.live("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.eval()
	if h := e.eng.Health(); h.PendingEpisodes != 1 || len(e.sent()) != 0 {
		t.Fatalf("an opened door starts counting, it does not alert: %+v", h)
	}
	e.clk.Advance(9*time.Minute + 59*time.Second)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("one second early must not alert")
	}
	e.clk.Advance(time.Second)
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || got[0].Duration != 10*time.Minute || got[0].HeldAtLeast || got[0].Meaning != monitor.MeaningOpen || got[0].Kind != MsgAlert {
		t.Fatalf("the alert at exactly the duration: %+v", got)
	}
	e.clk.Advance(time.Hour)
	e.eval()
	e.flush()
	if len(e.sent()) != 1 || e.eng.Health().PendingEpisodes != 0 {
		t.Fatal("one alert per episode")
	}
	// Closing and reopening starts a new episode after the cooldown.
	e.step("binary_sensor.door", monitor.CategoryDoor, "on", "off")
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.clk.Advance(10 * time.Minute)
	e.eval()
	e.flush()
	if len(e.sent()) != 2 {
		t.Fatalf("a second episode alerts again: %d", len(e.sent()))
	}
}

func TestDuration_aConditionThatClearsBeforeTheDurationNeverAlerts(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.clk.Advance(5 * time.Minute)
	e.step("binary_sensor.door", monitor.CategoryDoor, "on", "off")
	e.clk.Advance(30 * time.Minute)
	e.eval()
	e.flush()
	if h := e.eng.Health(); len(e.sent()) != 0 || h.PendingEpisodes != 0 || h.EpisodesCancelled != 1 || h.EpisodesFired != 0 {
		t.Fatalf("a door closed after 5 of 10 minutes sends nothing: sent=%d %+v", len(e.sent()), h)
	}
}

func TestDuration_theCountRestartsAfterAnInterruption(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.tempRule(30, 0, 5*time.Minute, "25")
	e.step("sensor.t", monitor.CategoryTemperature, "25", "31")
	e.clk.Advance(2 * time.Minute)
	e.step("sensor.t", monitor.CategoryTemperature, "31", "29")
	e.clk.Advance(time.Minute)
	e.step("sensor.t", monitor.CategoryTemperature, "29", "32")
	e.clk.Advance(4*time.Minute + 59*time.Second) // five minutes since the FIRST crossing, but not since the second
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("a duration is measured from the latest uninterrupted start, never from an earlier one")
	}
	e.clk.Advance(time.Second)
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || !got[0].HasValue || got[0].Value != 32 || got[0].Unit != "°C" || got[0].Duration != 5*time.Minute {
		t.Fatalf("the numeric duration alert carries the current value and unit: %+v", got)
	}
}

func TestDuration_anUnavailableSensorCancelsTheCount_andANewReadingStartsAgain(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.clk.Advance(8 * time.Minute)
	e.step("binary_sensor.door", monitor.CategoryDoor, "on", "unavailable")
	e.clk.Advance(time.Minute)
	e.step("binary_sensor.door", monitor.CategoryDoor, "unavailable", "on")
	e.clk.Advance(9*time.Minute + 59*time.Second) // 18 minutes after the first open, 9:59 after the new reading
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("continuity across an unavailable state is unknown, so the count restarts")
	}
	e.clk.Advance(time.Second)
	e.eval()
	e.flush()
	if got := e.sent(); len(got) != 1 {
		t.Fatalf("it alerts a full duration after the sensor is seen violating again: %d", len(got))
	}
}

func TestDuration_theCurrentStateIsReReadWhenItComesDue(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	// The door closes but its event never reaches the engine (a missed event): only the tracker knows.
	e.monitored("binary_sensor.door", monitor.CategoryDoor, "off")
	e.clk.Advance(10 * time.Minute)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("a due duration trusts the current sensor, not its own memory")
	}
}

func TestDuration_duplicateEventsDoNotRestartOrDoubleTheCount(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	for i := 0; i < 5; i++ {
		e.clk.Advance(time.Minute)
		e.step("binary_sensor.door", monitor.CategoryDoor, "on", "on") // the same state again (an attribute-only update)
	}
	e.clk.Advance(5 * time.Minute)
	e.eval()
	e.flush()
	if got := e.sent(); len(got) != 1 {
		t.Fatalf("ten minutes after the first open, exactly one alert: %d", len(got))
	}
}

func TestDuration_manyRulesOnOneEntity_eachFiresOnItsOwnSchedule(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState})
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 5 * time.Minute})
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 15 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 1 {
		t.Fatalf("the immediate rule alerts at once: %d", len(e.sent()))
	}
	for i, d := range []time.Duration{5, 5, 5} {
		e.clk.Advance(d * time.Minute)
		e.eval()
		e.flush()
		if got := len(e.sent()); got != i+2 {
			t.Fatalf("after %d minutes: %d alerts, want %d", 5*(i+1), got, i+2)
		}
	}
	rules := map[int64]bool{}
	for _, m := range e.sent() {
		rules[m.RuleID] = true
	}
	if len(rules) != 4 {
		t.Fatalf("four distinct rules fired once each: %v", rules)
	}
}

func TestDuration_aTemperatureHasFourRulesOnOneSensor(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.share("sensor.t", entities.AccessRead)
	e.monitored("sensor.t", monitor.CategoryTemperature, "20")
	e.addV2(NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(30), Cooldown: dur(0)})
	e.addV2(NewRule{Entity: "sensor.t", Kind: KindAbove, Threshold: fl(30), Duration: 5 * time.Minute, Cooldown: dur(0)})
	e.addV2(NewRule{Entity: "sensor.t", Kind: KindBelow, Threshold: fl(5), Cooldown: dur(0)})
	e.addV2(NewRule{Entity: "sensor.t", Kind: KindBelow, Threshold: fl(5), Duration: 10 * time.Minute, Cooldown: dur(0)})
	e.step("sensor.t", monitor.CategoryTemperature, "20", "31")
	e.clk.Advance(5 * time.Minute)
	e.eval()
	e.flush()
	e.step("sensor.t", monitor.CategoryTemperature, "31", "4")
	e.clk.Advance(10 * time.Minute)
	e.eval()
	e.flush()
	if got := len(e.sent()); got != 4 {
		t.Fatalf("an entity with more than two rules: every one fires (the old two-latch limit is gone): %d", got)
	}
}

func TestDuration_removingPausingOrRevokingWhilePendingNeverAlerts(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	for _, id := range []string{"binary_sensor.a", "binary_sensor.b", "binary_sensor.c"} {
		e.ready(id, monitor.CategoryDoor)
		e.addV2(NewRule{Entity: entities.ID(id), Kind: KindState, Duration: 10 * time.Minute})
		e.step(id, monitor.CategoryDoor, "off", "on")
	}
	e.clk.Advance(5 * time.Minute)
	if err := e.svc.RemoveRule(ctx, admin, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetRuleEnabled(ctx, admin, 2, false); err != nil {
		t.Fatal(err)
	}
	e.reload()
	if _, err := e.perms.Revoke(ctx, "binary_sensor.c"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(5 * time.Minute)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatalf("a removed rule, a paused rule and a revoked share must never alert: %d", len(e.sent()))
	}
	if h := e.eng.Health(); h.SuppressedUnshared != 1 || h.PendingEpisodes != 0 {
		t.Fatalf("the revoked share is counted: %+v", h)
	}
	// Resuming starts a NEW count: never early.
	if err := e.svc.SetRuleEnabled(ctx, admin, 2, true); err != nil {
		t.Fatal(err)
	}
	e.reload()
	e.eval() // the evaluator picks the resumed rule up at once
	e.clk.Advance(9*time.Minute + 59*time.Second)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("a resumed rule counts from the resume, not from the original open")
	}
	e.clk.Advance(time.Second)
	e.eval()
	e.flush()
	if len(e.sent()) != 1 {
		t.Fatalf("a resumed rule alerts a full duration after it resumed: %d", len(e.sent()))
	}
}

func TestDuration_aMonitoringOutageNeverFiresEarly_andReseedsTheCount(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.clk.Advance(3 * time.Minute)
	e.stat.set(false, monitor.StateReconnecting)
	e.eng.sampleHealth()
	e.clk.Advance(30 * time.Minute) // far past the original due time, while Home Assistant cannot be watched
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("a duration is never claimed while monitoring is down")
	}
	e.stat.set(true, monitor.StateReady)
	e.eng.sampleHealth()
	e.eval()
	e.clk.Advance(9*time.Minute + 59*time.Second)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("after an outage the count restarts from the current sensors: continuity cannot be proven")
	}
	e.clk.Advance(time.Second)
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || !got[0].HeldAtLeast {
		t.Fatalf("a duration after an outage is late and says 'at least': %+v", got)
	}
}

func TestDuration_afterARestartTheCountStartsFromTheFirstSightingNeverEarlier(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.clk.Advance(9 * time.Minute) // nine of ten minutes are over when NeptHass restarts
	// A new process: fresh runtime state, the same database, the same Home Assistant.
	eng2, err := NewEngine(e.eng.d)
	if err != nil {
		t.Fatal(err)
	}
	e.eng = eng2
	e.reload()
	e.eval() // the new process evaluates at start-up, which is its first sighting
	e.clk.Advance(9*time.Minute + 59*time.Second)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("elapsed time before a restart is not claimed: it would be a guess")
	}
	e.clk.Advance(time.Second)
	e.eval()
	e.flush()
	if got := e.sent(); len(got) != 1 || !got[0].HeldAtLeast {
		t.Fatalf("the alert comes a full duration after NeptHass first saw the sensor, labelled 'at least': %+v", got)
	}
}

func TestDuration_clockChangesNeverFireEarly(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.clk.Advance(-time.Hour) // the clock is set back
	e.eval()
	e.flush()
	e.clk.Advance(time.Hour + 9*time.Minute + 59*time.Second)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("a clock moved backwards must not make a duration fire early")
	}
	e.clk.Advance(time.Second)
	e.eval()
	e.flush()
	if len(e.sent()) != 1 {
		t.Fatalf("the alert comes when the clock says the duration has elapsed: %d", len(e.sent()))
	}
	// A forward jump fires late (at the next evaluation), never early.
	e2 := newEF(t, nil)
	e2.configure()
	e2.ready("binary_sensor.door", monitor.CategoryDoor)
	e2.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e2.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e2.clk.Advance(24 * time.Hour)
	e2.eval()
	e2.flush()
	if len(e2.sent()) != 1 {
		t.Fatalf("a forward jump alerts late: %d", len(e2.sent()))
	}
}

func TestDuration_aTrailOverflowCancelsInsteadOfGuessing(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	e.step("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.clk.Advance(time.Minute)
	prev := "on"
	for i := 0; i < maxTrail+4; i++ { // more flips than the trail keeps, ending open
		cur := "off"
		if prev == "off" {
			cur = "on"
		}
		e.live("binary_sensor.door", monitor.CategoryDoor, prev, cur)
		prev = cur
	}
	if prev != "on" {
		e.live("binary_sensor.door", monitor.CategoryDoor, prev, "on")
	}
	e.eval()
	e.clk.Advance(9 * time.Minute) // ten minutes after the ORIGINAL open
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("when continuity cannot be proven the old count is dropped")
	}
	e.clk.Advance(time.Minute) // ten minutes after the new first sighting
	e.eval()
	e.flush()
	if got := e.sent(); len(got) != 1 {
		t.Fatalf("the count restarted at the window: %+v", got)
	}
}

func TestDuration_andHysteresisTogether(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.tempRule(30, 28, 5*time.Minute, "25")
	e.step("sensor.t", monitor.CategoryTemperature, "25", "31")
	e.clk.Advance(5 * time.Minute)
	e.eval()
	e.flush()
	if len(e.sent()) != 1 {
		t.Fatal("first episode alerts after five minutes")
	}
	e.step("sensor.t", monitor.CategoryTemperature, "31", "29") // inside the band: no re-arm, so no new count
	e.step("sensor.t", monitor.CategoryTemperature, "29", "31")
	e.clk.Advance(time.Hour)
	e.eval()
	e.flush()
	if len(e.sent()) != 1 || e.eng.Health().PendingEpisodes != 0 {
		t.Fatalf("a disarmed rule does not start counting again until it clears: %d", len(e.sent()))
	}
	e.step("sensor.t", monitor.CategoryTemperature, "31", "27")
	e.step("sensor.t", monitor.CategoryTemperature, "27", "31")
	e.clk.Advance(5 * time.Minute)
	e.eval()
	e.flush()
	if len(e.sent()) != 2 {
		t.Fatalf("after clearing, a new episode alerts: %d", len(e.sent()))
	}
}

func TestDuration_aHazardRuleWithADurationInTheDatabaseIsDormant(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.leak", monitor.CategoryLeak)
	// Validation refuses this; the engine must not honor it even if it got into the database another way.
	if _, err := e.store.InsertRule(ctx, Rule{Entity: "binary_sensor.leak", Kind: KindState, Duration: 10 * time.Minute, Enabled: true, CreatedBy: "x", CreatedAt: e.f().now, UpdatedBy: "x", UpdatedAt: e.f().now}); err != nil {
		t.Fatal(err)
	}
	e.reload()
	e.step("binary_sensor.leak", monitor.CategoryLeak, "off", "on")
	e.clk.Advance(time.Hour)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatalf("a delayed leak alert is never produced: %d", len(e.sent()))
	}
	views, _ := e.svc.Rules(ctx, admin)
	if views[0].Status != StatusIncompatible {
		t.Errorf("the dormant rule says why: %s", views[0].Status)
	}
	// The immediate leak rule is untouched.
	e.addV2(NewRule{Entity: "binary_sensor.leak", Kind: KindState})
	e.step("binary_sensor.leak", monitor.CategoryLeak, "on", "off")
	e.step("binary_sensor.leak", monitor.CategoryLeak, "off", "on")
	if got := e.sent(); len(got) != 1 || got[0].Lane() != LaneCritical {
		t.Fatalf("the immediate leak rule still alerts on the critical lane: %+v", got)
	}
}

func TestDuration_manySimultaneousTimersUseNoExtraGoroutines(t *testing.T) {
	e := newEF(t, func(c *EngineConfig) { c.NormalCap = 512 })
	e.configure()
	const n = MaxRules
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("binary_sensor.d%03d", i)
		e.ready(id, monitor.CategoryDoor)
		e.addV2(NewRule{Entity: entities.ID(id), Kind: KindState, Duration: 10 * time.Minute})
	}
	before := runtime.NumGoroutine()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("binary_sensor.d%03d", i)
		e.live(id, monitor.CategoryDoor, "off", "on")
	}
	e.eval()
	if h := e.eng.Health(); h.PendingEpisodes != n {
		t.Fatalf("200 pending episodes: %d", h.PendingEpisodes)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("pending episodes are data, not goroutines: %d -> %d", before, after)
	}
	e.clk.Advance(10 * time.Minute)
	e.eval()
	e.flush()
	if got := len(e.sent()); got != n {
		t.Fatalf("every one of %d timers fires once in a burst: %d", n, got)
	}
}

func TestDuration_theRunningEngineFiresFromItsOwnTimer(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState, Duration: 10 * time.Minute})
	cancel, done, ticks := e.startRun()
	defer func() { cancel(); <-done }()
	<-ticks
	e.live("binary_sensor.door", monitor.CategoryDoor, "off", "on")
	e.clk.AwaitWaiters(t, 2) // the health watcher's tick and the evaluator's due timer
	e.clk.Advance(10 * time.Minute)
	got := e.snd.waitCalls(t, 1)
	if got[0].Kind != MsgAlert || got[0].Duration != 10*time.Minute {
		t.Fatalf("the evaluator's own timer fired the duration alert: %+v", got[0])
	}
}
