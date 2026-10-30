package notify

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

const (
	door = "binary_sensor.front_door"
	temp = "sensor.living_temp"
)

type ef struct {
	*fixture
	clk  *fakeClock
	snd  *fakeSender
	stat *fakeStatus
	acc  *flakyAccess
	eng  *Engine
	seq  uint64
}

func newEF(t *testing.T, mod func(*EngineConfig)) *ef {
	t.Helper()
	f := newFixture(t)
	e := &ef{fixture: f, clk: newFakeClock(), snd: newFakeSender(), stat: &fakeStatus{}, acc: &flakyAccess{inner: f.perms}}
	e.stat.set(true, monitor.StateReady)
	cfg := EngineConfig{Rand: func(n int64) int64 { return n - 1 }, Rate: 1000, Burst: 1000}
	if mod != nil {
		mod(&cfg)
	}
	eng, err := NewEngine(EngineDeps{Store: f.store, GuildID: testGuild, Access: e.acc, Sensors: f.tracker, Status: e.stat, Sender: e.snd, Clock: e.clk, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	e.eng = eng
	return e
}

func (e *ef) reload() {
	e.t.Helper()
	if err := e.eng.Reload(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *ef) configure() {
	e.t.Helper()
	if err := e.svc.SetChannel(ctx, admin, testChannel); err != nil {
		e.t.Fatal(err)
	}
	e.reload()
}

func (e *ef) addRule(id string, cat monitor.Category, kind Kind, thr *float64, cd *time.Duration) Rule {
	e.t.Helper()
	e.ready(id, cat)
	return e.add(id, kind, thr, cd)
}

func (e *ef) addNumeric(id string, cat monitor.Category, kind Kind, thr float64, raw string, cd *time.Duration) Rule {
	e.t.Helper()
	e.share(id, entities.AccessRead)
	e.monitored(id, cat, raw)
	return e.add(id, kind, &thr, cd)
}

func (e *ef) add(id string, kind Kind, thr *float64, cd *time.Duration) Rule {
	e.t.Helper()
	r, err := e.svc.AddRule(ctx, admin, NewRule{Entity: entities.ID(id), Kind: kind, Threshold: thr, Cooldown: cd})
	if err != nil {
		e.t.Fatal(err)
	}
	e.reload()
	return r
}

// change builds a monitor change at the engine clock. prev "" is a first observation.
func (e *ef) change(id string, cat monitor.Category, prev, cur string, src monitor.ChangeSource) monitor.Change {
	e.seq++
	c := chg(id, cat, prev != "", prev, cur, src, e.seq)
	c.At = e.clk.Now()
	c.Cur.ObservedAt, c.Cur.LastChanged, c.Cur.LastUpdated = c.At, c.At, c.At
	return c
}

func (e *ef) emit(cs ...monitor.Change) {
	for _, c := range cs {
		e.eng.OnChange(c)
	}
}

func (e *ef) eval() { e.eng.evaluate(ctx) }

// flush delivers everything that is eligible now, in queue order.
func (e *ef) flush() {
	for {
		it, ok, _ := e.eng.q.pop(e.clk.Now())
		if !ok {
			return
		}
		e.eng.disp.process(ctx, ctx, it, false)
	}
}

// flow is one live transition, evaluated and delivered.
func (e *ef) flow(id string, cat monitor.Category, prev, cur string) {
	e.emit(e.change(id, cat, prev, cur, monitor.SourceLive))
	e.eval()
	e.flush()
}

func (e *ef) sent() []Message { return e.snd.messages() }

func exported(v any) string {
	var b strings.Builder
	rv := reflect.ValueOf(v)
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Name == "Lines" {
			for _, l := range rv.Field(i).Interface().([]DigestLine) {
				b.WriteString(exported(l))
			}
			continue
		}
		fmt.Fprintf(&b, "%s=%v|", f.Name, rv.Field(i).Interface())
	}
	return b.String()
}

func TestEngine_aFirstObservationNeverAlerts_butTheNextRealTransitionDoes(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	e.flow(door, monitor.CategoryDoor, "", "on") // baseline: the door is open when first seen
	if len(e.sent()) != 0 {
		t.Fatalf("a first observation is a baseline, never an alert: %v", e.sent())
	}
	e.flow(door, monitor.CategoryDoor, "on", "off")
	if len(e.sent()) != 0 {
		t.Fatal("closing is not an alert")
	}
	e.flow(door, monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 1 {
		t.Errorf("the next real transition alerts: %d", len(e.sent()))
	}
}

func TestEngine_everyCategoryAndTriggerAlertsOnItsQualifyingTransition(t *testing.T) {
	cases := []struct {
		name      string
		cat       monitor.Category
		kind      Kind
		thr       float64
		prev, cur string
		meaning   monitor.Meaning
		numeric   bool
	}{
		{"door opens", monitor.CategoryDoor, KindState, 0, "off", "on", monitor.MeaningOpen, false},
		{"window opens", monitor.CategoryWindow, KindState, 0, "off", "on", monitor.MeaningOpen, false},
		{"motion detected", monitor.CategoryMotion, KindState, 0, "off", "on", monitor.MeaningDetected, false},
		{"leak wet", monitor.CategoryLeak, KindState, 0, "off", "on", monitor.MeaningWet, false},
		{"battery low", monitor.CategoryBatteryLow, KindState, 0, "off", "on", monitor.MeaningLow, false},
		{"temperature above", monitor.CategoryTemperature, KindAbove, 28, "20", "29", monitor.MeaningReading, true},
		{"temperature below", monitor.CategoryTemperature, KindBelow, 10, "15", "8", monitor.MeaningReading, true},
		{"battery level below", monitor.CategoryBattery, KindBelow, 15, "20", "10", monitor.MeaningReading, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEF(t, nil)
			e.configure()
			id := "sensor.x1"
			if tc.numeric {
				e.addNumeric(id, tc.cat, tc.kind, tc.thr, tc.prev, dur(0))
			} else {
				e.addRule(id, tc.cat, tc.kind, nil, dur(0))
			}
			e.flow(id, tc.cat, tc.prev, tc.cur)
			got := e.sent()
			if len(got) != 1 {
				t.Fatalf("exactly one alert, got %d", len(got))
			}
			m := got[0]
			if m.Kind != MsgAlert || m.Category != tc.cat || m.Trigger != tc.kind || m.Meaning != tc.meaning || m.Name != "Name" || m.Late || m.Uncertain || m.Cleared {
				t.Errorf("message = %+v", m)
			}
			if tc.numeric && (!m.HasValue || m.Threshold == nil || *m.Threshold != tc.thr) {
				t.Errorf("a numeric alert carries the reading and the threshold: %+v", m)
			}
			if !tc.numeric && m.Threshold != nil {
				t.Errorf("a state alert has no threshold: %+v", m)
			}
		})
	}
}

func TestEngine_aNumericRuleAlertsOnceUntilItReArms(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addNumeric(temp, monitor.CategoryTemperature, KindAbove, 28, "20", dur(0))
	e.flow(temp, monitor.CategoryTemperature, "20", "29")
	e.flow(temp, monitor.CategoryTemperature, "29", "30") // still above: not a new crossing
	e.flow(temp, monitor.CategoryTemperature, "30", "27") // back below: re-armed, silent
	if len(e.sent()) != 1 {
		t.Fatalf("one alert so far: %d", len(e.sent()))
	}
	e.flow(temp, monitor.CategoryTemperature, "27", "29")
	if len(e.sent()) != 2 {
		t.Errorf("crossing again after re-arming alerts again: %d", len(e.sent()))
	}
}

func TestEngine_unreliableStates_failLoudOnlyForALeak(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	e.flow(door, monitor.CategoryDoor, "off", "unavailable")
	e.flow(door, monitor.CategoryDoor, "unavailable", "on") // the HA restart case: silent
	if len(e.sent()) != 0 {
		t.Fatalf("unavailable to open is silent for a door: %v", e.sent())
	}
	e.flow(door, monitor.CategoryDoor, "on", "unavailable")
	e.flow(door, monitor.CategoryDoor, "unavailable", "on") // open, blinked offline, open again: not a new alert
	e.flow(door, monitor.CategoryDoor, "on", "off")
	e.flow(door, monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 1 {
		t.Errorf("only the real closed-to-open transition alerts: %d", len(e.sent()))
	}

	l := newEF(t, nil)
	l.configure()
	l.addRule("binary_sensor.leak", monitor.CategoryLeak, KindState, nil, dur(0))
	l.flow("binary_sensor.leak", monitor.CategoryLeak, "off", "unavailable")
	l.flow("binary_sensor.leak", monitor.CategoryLeak, "unavailable", "on")
	l.flow("binary_sensor.leak", monitor.CategoryLeak, "on", "unavailable")
	l.flow("binary_sensor.leak", monitor.CategoryLeak, "unknown", "on")
	got := l.sent()
	if len(got) != 2 || !got[0].Uncertain || !got[1].Uncertain || got[0].Meaning != monitor.MeaningWet {
		t.Fatalf("a leak that reaches wet from unavailable or unknown alerts, marked uncertain: %+v", got)
	}

	n := newEF(t, nil)
	n.configure()
	n.addNumeric(temp, monitor.CategoryTemperature, KindAbove, 28, "20", dur(0))
	n.flow(temp, monitor.CategoryTemperature, "unavailable", "29")
	if len(n.sent()) != 0 {
		t.Errorf("a numeric sensor from unavailable to above is silent: %v", n.sent())
	}
}

func TestEngine_aSnapshotDifferenceAfterAReconnectIsALateAlert(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	e.emit(e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceSnapshot))
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || !got[0].Late {
		t.Fatalf("a transition found by a snapshot is reported as a late observation: %+v", got)
	}

	f := newEF(t, nil)
	f.configure()
	f.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	f.emit(f.change(door, monitor.CategoryDoor, "", "on", monitor.SourceSnapshot)) // the initial snapshot
	f.eval()
	f.flush()
	if len(f.sent()) != 0 {
		t.Error("the initial snapshot never alerts")
	}
}

func TestEngine_updatesThatChangeNothingDoNotTrigger(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	e.flow(door, monitor.CategoryDoor, "off", "on")
	e.flow(door, monitor.CategoryDoor, "on", "on") // an attribute-only update while open
	e.emit(e.change(door, monitor.CategoryDoor, "on", "on", monitor.SourceReplay))
	e.eval()
	e.flush()
	if len(e.sent()) != 1 {
		t.Errorf("repeated open states are not transitions: %d alerts", len(e.sent()))
	}
}

func TestEngine_aViolationThatEndsBeforeEvaluationStillAlerts_markedCleared(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	e.emit(e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive), e.change(door, monitor.CategoryDoor, "on", "off", monitor.SourceLive))
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || !got[0].Cleared || got[0].Meaning != monitor.MeaningOpen {
		t.Fatalf("the latch preserves the transient violation: %+v", got)
	}
	e.flow(door, monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 2 {
		t.Error("the rule re-armed because the sensor ended reliably clear")
	}
}

func TestEngine_cooldown_suppressedViolationsAreNeverDeferredAndLeaveNoReminder(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, nil) // category default: one minute
	step := func(after time.Duration, prev, cur string) {
		e.clk.Advance(after)
		e.flow(door, monitor.CategoryDoor, prev, cur)
	}
	step(0, "off", "on")
	if len(e.sent()) != 1 {
		t.Fatal("first alert")
	}
	step(10*time.Second, "on", "off")
	step(10*time.Second, "off", "on") // within the minute: suppressed
	step(10*time.Second, "on", "off")
	step(10*time.Second, "off", "on") // suppressed
	step(10*time.Second, "on", "off") // cleared before the cooldown ended
	if len(e.sent()) != 1 || e.eng.Health().SuppressedCooldown != 2 {
		t.Fatalf("two violations suppressed, none deferred: sent=%d suppressed=%d", len(e.sent()), e.eng.Health().SuppressedCooldown)
	}
	e.clk.Advance(10 * time.Minute)
	e.eval()
	e.flush()
	if len(e.sent()) != 1 {
		t.Error("a suppressed violation that cleared never produces an alert afterwards")
	}
	step(0, "off", "on")
	got := e.sent()
	if len(got) != 2 || got[1].Muted != 2 {
		t.Fatalf("the next episode alerts and reports what was muted: %+v", got)
	}

	// Still violating when the cooldown ends: no reminder.
	e.clk.Advance(time.Second)
	e.flow(door, monitor.CategoryDoor, "on", "off")
	step(10*time.Second, "off", "on") // suppressed
	e.clk.Advance(30 * time.Minute)
	e.eval()
	e.flush()
	if len(e.sent()) != 2 {
		t.Errorf("an alert is only produced by a transition, never by time passing: %d", len(e.sent()))
	}
}

func TestEngine_cooldownOverrideAndZero(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(10*time.Second))
	e.flow(door, monitor.CategoryDoor, "off", "on")
	e.clk.Advance(11 * time.Second)
	e.flow(door, monitor.CategoryDoor, "on", "off")
	e.flow(door, monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 2 {
		t.Errorf("a ten second override beats the one minute default: %d", len(e.sent()))
	}

	z := newEF(t, nil)
	z.configure()
	z.addRule("binary_sensor.motion", monitor.CategoryMotion, KindState, nil, dur(0))
	for i := 0; i < 3; i++ {
		z.flow("binary_sensor.motion", monitor.CategoryMotion, "off", "on")
		z.flow("binary_sensor.motion", monitor.CategoryMotion, "on", "off")
	}
	if len(z.sent()) != 3 {
		t.Errorf("a zero cooldown disables the cooldown: %d", len(z.sent()))
	}
}

func TestEngine_rulesRequireReadAccess_andDormantRulesNeverTrigger(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	r := e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	if _, err := e.perms.Revoke(ctx, door); err != nil {
		t.Fatal(err)
	}
	e.flow(door, monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 0 || e.eng.Health().SuppressedUnshared != 1 {
		t.Fatalf("an unshared entity must not alert: sent=%d unshared=%d", len(e.sent()), e.eng.Health().SuppressedUnshared)
	}
	e.flow(door, monitor.CategoryDoor, "on", "off")
	e.share(door, entities.AccessRead)
	e.flow(door, monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 1 {
		t.Error("shared again: the rule resumes by itself on the next real transition, and the suppressed episode is not replayed")
	}

	if err := e.svc.SetRuleEnabled(ctx, admin, r.ID, false); err != nil {
		t.Fatal(err)
	}
	e.reload()
	e.emit(e.change(door, monitor.CategoryDoor, "on", "off", monitor.SourceLive), e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive))
	if p, _ := e.eng.mb.pending(e.clk.Now()); p != 0 {
		t.Error("a paused rule is not watched at all")
	}

	i := newEF(t, nil)
	i.configure()
	i.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	i.flow(door, monitor.CategoryTemperature, "20", "29") // the sensor was reclassified as numeric
	if len(i.sent()) != 0 {
		t.Error("an incompatible rule never triggers")
	}
}

func TestEngine_removalAndReAdditionEstablishAFreshBaseline(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	removal := e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive)
	removal.Removed, removal.Cur = true, monitor.Sensor{}

	e.emit(e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive), removal)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("a pending transition dies with its entity")
	}
	e.flow(door, monitor.CategoryDoor, "", "on") // added again, already open: a baseline
	if len(e.sent()) != 0 {
		t.Fatal("re-addition is a first observation")
	}
	e.flow(door, monitor.CategoryDoor, "on", "off")
	e.flow(door, monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 1 {
		t.Errorf("after the fresh baseline the rule works normally: %d", len(e.sent()))
	}
}

func TestEngine_reloadSeedsNewAndResumedRulesSilently_andDisablingStopsEverything(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.share(door, entities.AccessRead)
	e.monitored(door, monitor.CategoryDoor, "on") // already open when the rule is created
	r := e.add(door, KindState, nil, dur(0))
	e.flow(door, monitor.CategoryDoor, "on", "on")
	if len(e.sent()) != 0 {
		t.Fatal("a new rule never alerts for a state that already existed")
	}

	if err := e.svc.SetRuleEnabled(ctx, admin, r.ID, false); err != nil {
		t.Fatal(err)
	}
	e.reload()
	e.monitored(door, monitor.CategoryDoor, "off")
	e.monitored(door, monitor.CategoryDoor, "on") // changed while paused
	if err := e.svc.SetRuleEnabled(ctx, admin, r.ID, true); err != nil {
		t.Fatal(err)
	}
	e.reload()
	e.flow(door, monitor.CategoryDoor, "on", "on")
	if len(e.sent()) != 0 {
		t.Fatal("a resumed rule is re-seeded from the current state")
	}

	e.flow(door, monitor.CategoryDoor, "on", "off")
	if err := e.svc.SetEnabled(ctx, admin, false); err != nil {
		t.Fatal(err)
	}
	e.reload()
	e.emit(e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive))
	if p, _ := e.eng.mb.pending(e.clk.Now()); p != 0 || e.eng.Health().Active {
		t.Errorf("with notifications disabled the callback records nothing: pending=%d active=%v", p, e.eng.Health().Active)
	}
}

func TestEngine_messagesNeverExposeEntityIDsOrRawAttributes(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	c := e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive)
	c.Cur.Name = "" // no alias and no friendly name: the fallback must not be the ID
	e.emit(c)
	e.eval()
	e.flush()
	m := e.sent()[0]
	if m.Name != "Door sensor" {
		t.Errorf("fallback name = %q", m.Name)
	}
	digest := Message{Kind: MsgDigest, Lines: []DigestLine{{Name: "Door sensor", entity: door}}}
	for _, msg := range []Message{m, digest} {
		text := exported(msg)
		if strings.Contains(text, door) || strings.Contains(text, "binary_sensor") {
			t.Errorf("an entity ID is visible to the transport: %s", text)
		}
	}
}

func TestEngine_overflowIsCountedNeverSilent_andARecoveredChannelGetsOneDigestOfCURRENTState(t *testing.T) {
	e := newEF(t, func(c *EngineConfig) { c.NormalCap, c.CriticalCap = 3, 3 })
	e.configure()
	var ids []string
	var rules []Rule
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("binary_sensor.door%d", i)
		ids = append(ids, id)
		rules = append(rules, e.addRule(id, monitor.CategoryDoor, KindState, nil, dur(0)))
	}
	for _, id := range ids {
		e.emit(e.change(id, monitor.CategoryDoor, "off", "on", monitor.SourceLive))
		e.monitored(id, monitor.CategoryDoor, "on") // the tracker now holds the current state
	}
	e.eval()
	h := e.eng.Health()
	if h.DroppedOverflow != 3 || !h.Degraded || h.ResyncPending != 3 {
		t.Fatalf("three alerts overflowed the lane and must be counted, degrade health and be flagged: %+v", h)
	}
	e.monitored(ids[1], monitor.CategoryDoor, "off") // one of the dropped sensors closed again meanwhile
	e.flush()
	e.eval() // the queue has drained: reconcile current state
	e.flush()
	got := e.sent()
	last := got[len(got)-1]
	if len(got) != 4 || last.Kind != MsgDigest || len(last.Lines) != 2 || last.MoreLines != 0 {
		t.Fatalf("one digest of the dropped rules that are STILL violating (the closed one is not invented): %+v", got)
	}
	e.eval()
	e.flush()
	if len(e.sent()) != 4 || e.eng.Health().Digests != 1 {
		t.Error("no second digest without new drops")
	}

	e.eng.markResync(rules[0].ID)
	e.eval()
	if e.eng.Health().Digests != 1 {
		t.Error("digests are spaced out: no storm within the minimum interval")
	}
	e.clk.Advance(time.Minute)
	e.eval()
	if e.eng.Health().Digests != 2 {
		t.Error("after the interval the next digest may go out")
	}
}

func TestEngine_aDigestIsBounded(t *testing.T) {
	e := newEF(t, func(c *EngineConfig) { c.DigestMaxLines = 2 })
	e.configure()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("binary_sensor.door%d", i)
		r := e.addRule(id, monitor.CategoryDoor, KindState, nil, dur(0))
		e.monitored(id, monitor.CategoryDoor, "on")
		e.eng.markResync(r.ID)
	}
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || len(got[0].Lines) != 2 || got[0].MoreLines != 1 {
		t.Errorf("two lines and 'one more': %+v", got)
	}
}

func TestEngine_aMessageThatExpiresInTheQueueIsDroppedAndReportedByCurrentStateOnly(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	e.emit(e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive))
	e.eval()
	e.monitored(door, monitor.CategoryDoor, "on")
	e.clk.Advance(11 * time.Minute)
	e.flush()
	if len(e.sent()) != 0 || e.eng.Health().DroppedTTL != 1 {
		t.Fatalf("expired: sent=%d ttl drops=%d", len(e.sent()), e.eng.Health().DroppedTTL)
	}
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || got[0].Kind != MsgDigest || len(got[0].Lines) != 1 {
		t.Errorf("the still-open door is reported by a digest: %+v", got)
	}

	c := newEF(t, nil)
	c.configure()
	c.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	c.emit(c.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive))
	c.eval()
	c.monitored(door, monitor.CategoryDoor, "off") // closed again before the digest
	c.clk.Advance(11 * time.Minute)
	c.flush()
	c.eval()
	c.flush()
	if len(c.sent()) != 0 {
		t.Errorf("a transition that ended is not recovered or invented: %+v", c.sent())
	}
}

func TestEngine_healthReportsDegradationAndItsReasons(t *testing.T) {
	e := newEF(t, func(c *EngineConfig) { c.CriticalCap = 5 })
	e.configure()
	if h := e.eng.Health(); h.Degraded || !h.Active {
		t.Fatalf("a quiet engine is healthy: %+v", h)
	}
	e.eng.m.countDrop(dropTTL, e.clk.Now())
	if h := e.eng.Health(); !h.Degraded || len(h.Reasons) != 1 {
		t.Errorf("a lost alert degrades health: %+v", h)
	}
	e.clk.Advance(11 * time.Minute)
	if h := e.eng.Health(); h.Degraded {
		t.Errorf("the degradation ends after the window: %+v", h)
	}

	for i := 0; i < 5; i++ { // more than 80% of the critical lane
		e.eng.enqueue(Message{Kind: MsgDigest, ObservedAt: e.clk.Now(), Lines: []DigestLine{{entity: door}}})
	}
	e.clk.Advance(31 * time.Second)
	if h := e.eng.Health(); !h.Degraded || h.QueueCritical != 5 {
		t.Errorf("a sustained backlog degrades health: %+v", h)
	}
	e.flush()
	e.eng.noteBacklog()

	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	e.emit(e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive))
	e.clk.Advance(6 * time.Second)
	if h := e.eng.Health(); !h.Degraded || h.MailboxPending != 1 {
		t.Errorf("an evaluator that falls behind degrades health: %+v", h)
	}
	e.eval()

	e.snd.script = func(int, Message) error { return Permanent(fmt.Errorf("missing access")) }
	for i := 0; i < 3; i++ {
		e.eng.disp.process(ctx, ctx, item{msg: Message{Kind: MsgMonitoringDown}, deadline: e.clk.Now().Add(time.Hour)}, false)
	}
	if h := e.eng.Health(); !h.BreakerOpen || !h.Degraded {
		t.Errorf("an open breaker degrades health: %+v", h)
	}
}

func TestEngine_theMonitorCallbackNeverBlocksOnDeliveryOrTouchesTheDatabase(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	gate := make(chan struct{})
	e.snd.gate = gate // every send blocks and ignores cancellation
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.eng.Run(runCtx); close(done) }()

	finished := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ {
			e.emit(e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive), e.change(door, monitor.CategoryDoor, "on", "off", monitor.SourceLive))
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(testWait):
		t.Fatal("OnChange blocked while a send was stuck")
	}
	e.clk.AwaitWaiters(t, 1)
	cancel()
	e.clk.AwaitWaiters(t, 2) // the shutdown drain budget is running
	e.clk.Advance(3 * time.Second)
	<-done
	close(gate)
	awaitNoGoroutines(t, engineGoroutines...)

	d := newEF(t, nil)
	d.configure()
	d.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	if err := d.db.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		d.emit(d.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive)) // would fail if it queried the database
	}
	if p, _ := d.eng.mb.pending(d.clk.Now()); p != 1 {
		t.Errorf("the callback only records into the mailbox: %d", p)
	}
}

func TestEngine_runStopsEveryGoroutineOnCancellation(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	ticks := make(chan struct{}, 100)
	e.eng.onTick = func() { ticks <- struct{}{} }
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.eng.Run(runCtx); close(done) }()
	<-ticks
	// A goroutine that has been created but has not run yet already shows its Run closure.
	if got := goroutinesIn("notify.(*Engine).Run.func"); len(got) < 3 {
		t.Fatalf("Run must have started three goroutines (evaluator, dispatcher, watcher): %v", got)
	}
	cancel()
	<-done
	awaitNoGoroutines(t, engineGoroutines...)
}

func TestEngine_concurrentConfigurationUpdatesHooksAndEvaluation(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	var rules []Rule
	var changes []monitor.Change
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("binary_sensor.door%d", i)
		rules = append(rules, e.addRule(id, monitor.CategoryDoor, KindState, nil, dur(0)))
		changes = append(changes, e.change(id, monitor.CategoryDoor, "off", "on", monitor.SourceLive), e.change(id, monitor.CategoryDoor, "on", "off", monitor.SourceLive))
	}
	var wg, evaluator sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			if err := e.svc.SetRuleEnabled(ctx, admin, rules[i%5].ID, i%2 == 0); err != nil {
				t.Error(err)
				return
			}
			if err := e.eng.Reload(ctx); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			e.eng.OnChange(changes[i%len(changes)])
		}
	}()
	evaluator.Add(1)
	go func() {
		defer evaluator.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.eval()
			}
		}
	}()
	wg.Wait()
	close(stop)
	evaluator.Wait()
	e.reload()
	e.eval()
	enabled := 0
	all, _ := e.store.ListRules(ctx)
	for _, r := range all {
		if r.Enabled {
			enabled++
		}
	}
	if got := len(e.eng.rules.Load().byID); got != enabled {
		t.Errorf("the rule cache (%d) must match the stored enabled rules (%d)", got, enabled)
	}
}

func (e *ef) startRun() (cancel func(), done <-chan struct{}, ticks chan struct{}) {
	ticks = make(chan struct{}, 1000)
	e.eng.onTick = func() { ticks <- struct{}{} }
	runCtx, c := context.WithCancel(context.Background())
	d := make(chan struct{})
	go func() { e.eng.Run(runCtx); close(d) }()
	return c, d, ticks
}

func TestEngine_monitoringDown_oneAlertAfterFiveMinutes_recoveryOnlyAfterDelivery_thenANewEpisode(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.stat.set(false, monitor.StateReady) // liveness lost: no state transition at all, Healthy is simply false
	cancel, done, ticks := e.startRun()
	<-ticks
	for i := 0; i < 30; i++ { // 30 ticks of 10 seconds: five minutes
		e.clk.Advance(10 * time.Second)
		<-ticks
	}
	got := e.snd.waitCalls(t, 1)
	if got[0].Kind != MsgMonitoringDown || got[0].DownFor < 5*time.Minute || got[0].Episode != 1 {
		t.Fatalf("the down alert after five continuous minutes: %+v", got[0])
	}
	for i := 0; i < 6; i++ {
		e.clk.Advance(10 * time.Second)
		<-ticks
	}
	if len(e.snd.messages()) != 1 {
		t.Fatalf("one alert per outage, none while it keeps reconnecting: %d", len(e.snd.messages()))
	}
	e.stat.set(true, monitor.StateReady)
	e.clk.Advance(10 * time.Second)
	<-ticks
	got = e.snd.waitCalls(t, 2)
	if got[1].Kind != MsgMonitoringRecovered || got[1].Episode != 1 {
		t.Fatalf("recovery after a DELIVERED alert: %+v", got[1])
	}

	e.stat.set(false, monitor.StateReady)
	e.clk.Advance(10 * time.Second)
	<-ticks
	for i := 0; i < 30; i++ {
		e.clk.Advance(10 * time.Second)
		<-ticks
	}
	got = e.snd.waitCalls(t, 3)
	if got[2].Kind != MsgMonitoringDown || got[2].Episode != 2 {
		t.Errorf("the next outage is a new episode with its own alert: %+v", got[2])
	}
	cancel()
	<-done
}

func TestEngine_monitoringDown_neverAlertsDuringShutdownOrWhenDisabledOrWhenTheMonitorWasStopped(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.stat.set(false, monitor.StateReconnecting)
	cancel, done, ticks := e.startRun()
	<-ticks
	for i := 0; i < 24; i++ { // four minutes
		e.clk.Advance(10 * time.Second)
		<-ticks
	}
	cancel() // an intentional shutdown during the outage
	<-done
	if len(e.snd.messages()) != 0 || e.eng.down.current() != downStopped {
		t.Errorf("shutting down is not an outage: %v", e.snd.messages())
	}

	d := newEF(t, nil) // never configured: notifications are disabled
	d.stat.set(false, monitor.StateReady)
	c2, done2, ticks2 := d.startRun()
	<-ticks2
	for i := 0; i < 40; i++ {
		d.clk.Advance(10 * time.Second)
		<-ticks2
	}
	c2()
	<-done2
	if len(d.snd.messages()) != 0 {
		t.Errorf("disabled notifications never alert: %v", d.snd.messages())
	}

	s := newEF(t, nil)
	s.configure()
	s.stat.set(false, monitor.StateStopped) // Run on the monitor returned: a deliberate stop
	c3, done3, _ := s.startRun()
	c3()
	<-done3
	if len(s.snd.messages()) != 0 || s.eng.down.current() != downStopped {
		t.Errorf("a stopped monitor is not an outage: %v", s.snd.messages())
	}
}

func TestEngine_anUndeliveredDownAlertIsWithdrawnOnRecovery_andNeverFollowedByARecoveryNotice(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.snd.script = func(int, Message) error { return Retryable(fmt.Errorf("network")) }
	st := monitor.Status{State: monitor.StateReady}
	now := e.clk.Now()
	e.eng.handleDown(e.eng.down.step(now, false, true), st, now)
	now = now.Add(5 * time.Minute)
	e.eng.handleDown(e.eng.down.step(now, false, true), st, now) // the alert is queued
	e.clk.Advance(5 * time.Minute)
	e.flush() // the attempt fails and the alert waits to retry: queued, NOT delivered
	if len(e.sent()) != 1 {
		t.Fatalf("one failed attempt: %d", len(e.sent()))
	}
	now = now.Add(10 * time.Second)
	st.Healthy = true
	e.eng.handleDown(e.eng.down.step(now, true, true), st, now)
	if h := e.eng.Health(); h.DroppedCancelled != 1 || h.QueueCritical != 0 {
		t.Errorf("recovery withdraws the stale alert: %+v", h)
	}
	e.clk.Advance(time.Hour)
	e.flush()
	for _, m := range e.sent() {
		if m.Kind == MsgMonitoringRecovered {
			t.Error("no recovery notice for an alert that was never delivered")
		}
	}
}

func TestEngine_aDownAlertDeliveredAfterRecoveryStillGetsItsRecoveryNotice(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	st := monitor.Status{State: monitor.StateReady}
	now := e.clk.Now()
	e.eng.handleDown(e.eng.down.step(now, false, true), st, now)
	now = now.Add(5 * time.Minute)
	e.eng.handleDown(e.eng.down.step(now, false, true), st, now)
	inFlight, ok, _ := e.eng.q.pop(e.clk.Now()) // the dispatcher has taken it and is sending
	if !ok {
		t.Fatal("the alert should be queued")
	}
	now = now.Add(10 * time.Second)
	e.eng.handleDown(e.eng.down.step(now, true, true), st, now) // recovery: nothing left to withdraw
	e.eng.disp.process(ctx, ctx, inFlight, false)               // ...and the send completes
	e.flush()
	var kinds []MessageKind
	for _, m := range e.sent() {
		kinds = append(kinds, m.Kind)
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]MessageKind{MsgMonitoringDown, MsgMonitoringRecovered}) {
		t.Errorf("the channel must not be left saying monitoring is down: %v", kinds)
	}
}

func TestEngine_aRecoveryNoticeIsQueuedAheadOfTheLateSnapshotAlertsItExplains(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule("binary_sensor.leak", monitor.CategoryLeak, KindState, nil, dur(0))
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))

	// An outage whose down alert was DELIVERED.
	st := monitor.Status{State: monitor.StateReady}
	now := e.clk.Now()
	e.eng.handleDown(e.eng.down.step(now, false, true), st, now)
	now = now.Add(5 * time.Minute)
	e.clk.Advance(5 * time.Minute)
	e.eng.handleDown(e.eng.down.step(now, false, true), st, now)
	e.flush()
	if len(e.sent()) != 1 || e.sent()[0].Kind != MsgMonitoringDown {
		t.Fatalf("the down alert was delivered: %+v", e.sent())
	}

	// Monitoring recovers and the reconnect snapshot reveals two late transitions.
	e.stat.set(true, monitor.StateReady)
	e.emit(e.change("binary_sensor.leak", monitor.CategoryLeak, "off", "on", monitor.SourceSnapshot),
		e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceSnapshot))
	e.eval() // one pass: it must sample health before it produces the late alerts
	e.flush()

	var kinds []string
	for _, m := range e.sent()[1:] {
		label := map[MessageKind]string{MsgMonitoringRecovered: "recovered", MsgAlert: "alert"}[m.Kind]
		if m.Kind == MsgAlert && m.Late {
			label = "late-" + string(m.Category)
		}
		kinds = append(kinds, label)
	}
	if fmt.Sprint(kinds) != "[recovered late-leak late-door]" {
		t.Errorf("recovery first, then the late alerts (critical lane first, then normal): %v", kinds)
	}
}

func TestEngine_aFailedReloadKeepsThePreviousConfiguration_isDegraded_andTheWatcherRetriesUntilItWorks(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	if _, err := e.db.Exec("ALTER TABLE notification_rules RENAME TO notification_rules_broken"); err != nil {
		t.Fatal(err)
	}
	if err := e.eng.Reload(ctx); err == nil {
		t.Fatal("the reload must fail while the rules cannot be read")
	}
	h := e.eng.Health()
	if !h.ConfigStale || !h.Degraded || !h.Active {
		t.Fatalf("the previous configuration stays in force and the staleness is reported: %+v", h)
	}
	e.flow(door, monitor.CategoryDoor, "off", "on")
	if len(e.sent()) != 1 {
		t.Fatalf("the engine never half-loads: the previous rules still work: %d", len(e.sent()))
	}

	cancel, done, ticks := e.startRun()
	defer func() { cancel(); <-done }()
	<-ticks // the watcher retried and the store is still broken
	if !e.eng.Health().ConfigStale {
		t.Fatal("still broken, still stale")
	}
	if _, err := e.db.Exec("ALTER TABLE notification_rules_broken RENAME TO notification_rules"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(10 * time.Second)
	<-ticks
	if h := e.eng.Health(); h.ConfigStale || h.Degraded {
		t.Errorf("the watcher must retry until the saved configuration is active again: %+v", h)
	}
}

func TestEngine_theSameTransitionDeliveredTwiceBeforeEvaluationAlertsOnce(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.addRule(door, monitor.CategoryDoor, KindState, nil, dur(0))
	c := e.change(door, monitor.CategoryDoor, "off", "on", monitor.SourceLive)
	e.emit(c, c) // a duplicate of the very same change
	e.eval()
	e.flush()
	if len(e.sent()) != 1 {
		t.Errorf("a duplicate delivery must not alert twice: %d", len(e.sent()))
	}
}
