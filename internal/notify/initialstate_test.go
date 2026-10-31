package notify

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

const leakID = "binary_sensor.leak"

// restart is a new process: fresh runtime state, an EMPTY tracker (the monitor
// has not synchronized yet), and a start-up Reload, exactly like main.go. The
// database, the clock and the sender carry over.
func (e *ef) restart() {
	e.t.Helper()
	tr := monitor.NewTracker()
	d := e.eng.d
	d.Sensors = tr
	eng, err := NewEngine(d)
	if err != nil {
		e.t.Fatal(err)
	}
	e.fixture.tracker = tr
	e.eng = eng
	e.reload()
}

// firstSight is the first snapshot after a start: the tracker learns the sensor
// and the change feed reports a first observation.
func (e *ef) firstSight(id string, cat monitor.Category, raw string) {
	e.t.Helper()
	e.firstSightAt(id, cat, raw, e.fixture.now.Add(time.Second))
}

func (e *ef) firstSightAt(id string, cat monitor.Category, raw string, changed time.Time) {
	e.t.Helper()
	e.fixture.now = e.fixture.now.Add(time.Second)
	e.tracker.Apply(monitor.Update{ID: entities.ID(id), Category: cat, Name: "Name", Raw: raw, Unit: unitFor(cat), LastChanged: changed, LastUpdated: e.fixture.now}, e.fixture.now)
	c := e.change(id, cat, "", raw, monitor.SourceSnapshot)
	c.Cur.LastChanged = changed
	e.emit(c)
}

func (e *ef) leakRule() Rule {
	e.t.Helper()
	e.ready(leakID, monitor.CategoryLeak)
	return e.addV2(NewRule{Entity: leakID, Kind: KindState})
}

var episodeAt = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

func TestInitialState_aHazardAlreadyDetectedAtStartIsAnnouncedOnce_clearlyLabelled(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	r := e.leakRule()
	e.restart()
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt)
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 {
		t.Fatalf("one startup alert for a leak that is already wet: %d", len(got))
	}
	m := got[0]
	if !m.Initial || m.Kind != MsgAlert || m.Category != monitor.CategoryLeak || m.Meaning != monitor.MeaningWet || m.Lane() != LaneCritical || m.ReportedAt.IsZero() || !m.ChangedAt.Equal(episodeAt) || m.Late || m.Uncertain {
		t.Fatalf("it is an initial-state alert on the critical lane, never presented as a new event: %+v", m)
	}
	mark, ok, err := e.store.GetMark(ctx, r.ID)
	if err != nil || !ok || mark.EpisodeKey != episodeKey(episodeAt) {
		t.Errorf("a delivered hazard alert leaves a mark: %+v %v %v", mark, ok, err)
	}
	e.eval()
	e.flush()
	if len(e.sent()) != 1 || e.eng.Health().InitialAlerts != 1 {
		t.Fatal("exactly one, however many evaluations follow")
	}
}

func TestInitialState_aRestartDoesNotAnnounceTheSameEpisodeAgain_butAReminderComesAfterTheCooldown(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.leakRule()
	e.restart()
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt)
	e.eval()
	e.flush()

	e.clk.Advance(time.Hour) // far beyond the cooldown: only the episode identity can suppress now
	e.restart()
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt)
	e.eval()
	e.flush()
	if len(e.sent()) != 1 || e.eng.Health().MarkSuppressed != 1 {
		t.Fatalf("the same Home Assistant episode is not announced twice across a restart: %d alerts, %d suppressed", len(e.sent()), e.eng.Health().MarkSuppressed)
	}

	// Home Assistant restarted too (last_changed is new) and the alert is old: a reminder is useful, not a duplicate.
	e.restart()
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt.Add(2*time.Hour))
	e.eval()
	e.flush()
	if len(e.sent()) != 2 {
		t.Fatalf("a still-wet sensor is announced again after the cooldown when its episode identity changed: %d", len(e.sent()))
	}
	// An immediate crash loop with yet another identity stays quiet: the cooldown holds it.
	e.restart()
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt.Add(3*time.Hour))
	e.eval()
	e.flush()
	if len(e.sent()) != 2 || e.eng.Health().MarkSuppressed != 1 {
		t.Fatalf("a crash loop inside the cooldown must not repeat the alert: %d alerts", len(e.sent()))
	}
}

func TestInitialState_aDeliveryThatFailsLeavesNoMark_soTheNextStartAnnouncesIt(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	r := e.leakRule()
	e.restart()
	e.snd.script = func(int, Message) error { return Permanent(errors.New("missing access")) }
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt)
	e.eval()
	e.flush()
	if _, ok, _ := e.store.GetMark(ctx, r.ID); ok {
		t.Fatal("a mark is written only after a confirmed delivery")
	}
	e.snd.script = nil // the channel is fixed, then NeptHass restarts
	e.restart()
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt)
	e.eval()
	e.flush()
	delivered := 0
	for _, m := range e.sent() {
		if m.Initial {
			delivered++
		}
	}
	if delivered != 2 { // the failed attempt and the successful one
		t.Fatalf("the alert that never arrived is announced after the restart: %d attempts", delivered)
	}
	if _, ok, _ := e.store.GetMark(ctx, r.ID); !ok {
		t.Error("the confirmed delivery leaves the mark")
	}
}

func TestInitialState_aCrashAfterDeliveryButBeforeTheMarkIsWrittenRepeatsOnce(t *testing.T) {
	// Discord's acknowledgement and the SQLite mark are not atomic. If the process dies in between,
	// the alert is announced again after the restart: a duplicate, never a loss. This test pins that
	// residual risk so it is never silently changed into a loss.
	e := newEF(t, nil)
	e.configure()
	e.leakRule()
	e.restart()
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt)
	e.eval()
	e.flush()
	if _, err := e.db.Exec("DELETE FROM notification_marks"); err != nil { // the mark that the crash never wrote
		t.Fatal(err)
	}
	e.clk.Advance(time.Hour)
	e.restart()
	e.firstSightAt(leakID, monitor.CategoryLeak, "on", episodeAt)
	e.eval()
	e.flush()
	if len(e.sent()) != 2 {
		t.Fatalf("the residual risk is exactly one repeat: %d alerts", len(e.sent()))
	}
}

func TestInitialState_unavailableOrUnknownNeverAlertsAtStart_andAHazardRecoveringWetStaysTheV040FailLoud(t *testing.T) {
	for _, raw := range []string{"unavailable", "unknown"} {
		e := newEF(t, nil)
		e.configure()
		e.leakRule()
		e.restart()
		e.firstSight(leakID, monitor.CategoryLeak, raw)
		e.eval()
		e.flush()
		if len(e.sent()) != 0 {
			t.Fatalf("%s at start is not an alert: %d", raw, len(e.sent()))
		}
		e.step(leakID, monitor.CategoryLeak, raw, "on")
		got := e.sent()
		if len(got) != 1 || got[0].Initial || !got[0].Uncertain {
			t.Fatalf("%s then wet is the v0.4.0 uncertain fail-loud alert, once and not an initial alert: %+v", raw, got)
		}
	}
}

func TestInitialState_respectsSharingEnablementAndPausing(t *testing.T) {
	// Not shared.
	e := newEF(t, nil)
	e.configure()
	e.leakRule()
	if _, err := e.perms.Revoke(ctx, leakID); err != nil {
		t.Fatal(err)
	}
	e.restart()
	e.firstSight(leakID, monitor.CategoryLeak, "on")
	e.eval()
	e.flush()
	if len(e.sent()) != 0 || e.eng.Health().SuppressedUnshared != 1 {
		t.Fatalf("a sensor that is not shared is never announced: %d alerts", len(e.sent()))
	}
	// Paused rule.
	p := newEF(t, nil)
	p.configure()
	r := p.leakRule()
	if err := p.svc.SetRuleEnabled(ctx, admin, r.ID, false); err != nil {
		t.Fatal(err)
	}
	p.restart()
	p.firstSight(leakID, monitor.CategoryLeak, "on")
	p.eval()
	p.flush()
	if len(p.sent()) != 0 {
		t.Fatal("a paused rule is never announced")
	}
	// Notifications disabled.
	d := newEF(t, nil)
	d.configure()
	d.leakRule()
	if err := d.svc.SetEnabled(ctx, admin, false); err != nil {
		t.Fatal(err)
	}
	d.restart()
	d.firstSight(leakID, monitor.CategoryLeak, "on")
	d.eval()
	d.flush()
	d.clk.Advance(5 * time.Minute)
	d.eval()
	d.flush()
	if len(d.sent()) != 0 {
		t.Fatal("disabled notifications send nothing, not even a summary")
	}
}

func TestInitialState_aDrySensorAtStartIsSilent_andALaterLeakIsAnOrdinaryAlert(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.leakRule()
	e.restart()
	e.firstSight(leakID, monitor.CategoryLeak, "off")
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("dry at start: nothing")
	}
	e.step(leakID, monitor.CategoryLeak, "off", "on")
	if got := e.sent(); len(got) != 1 || got[0].Initial {
		t.Fatalf("a leak that starts while NeptHass watches is an ordinary alert: %+v", got)
	}
}

func TestInitialState_nonHazardsAreSummarizedOnce_afterTheWindow_withoutEntityIDs(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	doors := []string{"binary_sensor.d1", "binary_sensor.d2", "binary_sensor.d3", "binary_sensor.d4"}
	for _, id := range doors {
		e.ready(id, monitor.CategoryDoor)
		e.addV2(NewRule{Entity: entities.ID(id), Kind: KindState})
	}
	e.share("sensor.h", entities.AccessRead)
	e.monitored("sensor.h", monitor.CategoryHumidity, "40")
	e.addV2(NewRule{Entity: "sensor.h", Kind: KindAbove, Threshold: fl(60)})
	e.restart()
	for i, id := range doors {
		raw := "on"
		if i == 3 {
			raw = "off" // one door is closed
		}
		e.firstSight(id, monitor.CategoryDoor, raw)
	}
	e.firstSight("sensor.h", monitor.CategoryHumidity, "70")
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatalf("no individual alert for a condition that was already there: %d", len(e.sent()))
	}
	e.clk.Advance(time.Minute + 59*time.Second)
	e.eval()
	e.flush()
	if len(e.sent()) != 0 {
		t.Fatal("the summary waits for the end of the window")
	}
	e.clk.Advance(time.Second)
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || got[0].Kind != MsgStartup || len(got[0].Lines) != 4 || got[0].Lane() != LaneNormal {
		t.Fatalf("one summary of the three open doors and the humid room: %+v", got)
	}
	for _, l := range got[0].Lines {
		if l.Name == "" || l.entity == "" {
			t.Errorf("a line names its sensor and keeps its entity only for the permission re-check: %+v", l)
		}
	}
	e.clk.Advance(time.Hour)
	e.eval()
	e.flush()
	if len(e.sent()) != 1 || e.eng.Health().StartupSummaries != 1 {
		t.Fatal("one summary per start")
	}
	if st, _, _ := e.store.GetSettings(ctx, testGuild); st.StartupSummaryAt.IsZero() {
		t.Error("the delivery time of the summary is persisted")
	}
}

func TestInitialState_theSummaryWindowSurvivesARestart_soACrashLoopCannotFloodTheChannel(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.d1", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.d1", Kind: KindState})
	summary := func() {
		e.restart()
		e.firstSight("binary_sensor.d1", monitor.CategoryDoor, "on")
		e.eval()
		e.clk.Advance(2 * time.Minute)
		e.eval()
		e.flush()
	}
	summary()
	if len(e.sent()) != 1 {
		t.Fatal("the first start sends its summary")
	}
	e.clk.Advance(5 * time.Minute)
	summary() // a restart 7 minutes after the last summary
	if len(e.sent()) != 1 || e.eng.Health().SummarySuppressed != 1 {
		t.Fatalf("inside the 10 minute window a restart sends no second summary: %d", len(e.sent()))
	}
	e.clk.Advance(11 * time.Minute)
	summary()
	if len(e.sent()) != 2 {
		t.Fatalf("after the window a new start sends one again: %d", len(e.sent()))
	}
}

func TestInitialState_aConditionThatAppearsAfterTheWindowIsNotListed_butAHazardStillAlerts(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready("binary_sensor.d1", monitor.CategoryDoor)
	e.addV2(NewRule{Entity: "binary_sensor.d1", Kind: KindState})
	e.leakRule()
	e.restart()
	e.firstSight("binary_sensor.d1", monitor.CategoryDoor, "off")
	e.eval()
	e.clk.Advance(5 * time.Minute)
	e.eval()
	e.flush()
	e.firstSight("binary_sensor.d1", monitor.CategoryDoor, "off") // a late first reading of a door that is already open
	e.firstSight(leakID, monitor.CategoryLeak, "on")              // a leak first seen long after the start
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || !got[0].Initial || got[0].Category != monitor.CategoryLeak {
		t.Fatalf("after the window the hazard still alerts at once and nothing else is announced: %+v", got)
	}
}

func TestInitialState_manySimultaneouslyActiveSensorsAreBounded(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	const doors, leaks = 40, 5
	for i := 0; i < doors; i++ {
		id := fmt.Sprintf("binary_sensor.door%02d", i)
		e.ready(id, monitor.CategoryDoor)
		e.addV2(NewRule{Entity: entities.ID(id), Kind: KindState})
	}
	for i := 0; i < leaks; i++ {
		id := fmt.Sprintf("binary_sensor.leak%d", i)
		e.ready(id, monitor.CategoryLeak)
		e.addV2(NewRule{Entity: entities.ID(id), Kind: KindState})
	}
	e.restart()
	for i := 0; i < doors; i++ {
		e.firstSight(fmt.Sprintf("binary_sensor.door%02d", i), monitor.CategoryDoor, "on")
	}
	for i := 0; i < leaks; i++ {
		e.firstSight(fmt.Sprintf("binary_sensor.leak%d", i), monitor.CategoryLeak, "on")
	}
	e.eval()
	e.clk.Advance(2 * time.Minute)
	e.eval()
	e.flush()
	var initial, startup []Message
	for _, m := range e.sent() {
		switch {
		case m.Kind == MsgAlert && m.Initial:
			initial = append(initial, m)
		case m.Kind == MsgStartup:
			startup = append(startup, m)
		default:
			t.Errorf("unexpected message: %+v", m)
		}
	}
	if len(initial) != 3 {
		t.Fatalf("at most three individual hazard alerts per start: %d", len(initial))
	}
	if len(startup) != 2 {
		t.Fatalf("one critical message for the extra hazards and one summary for the doors: %d", len(startup))
	}
	var crit, norm Message
	for _, m := range startup {
		if m.Lane() == LaneCritical {
			crit = m
		} else {
			norm = m
		}
	}
	if len(crit.Lines) != 2 || len(norm.Lines) != 10 || norm.MoreLines != doors-10 {
		t.Fatalf("hazards beyond the budget are folded into one critical message; the doors fill one summary of ten lines plus a count: %d / %d+%d", len(crit.Lines), len(norm.Lines), norm.MoreLines)
	}
	if h := e.eng.Health(); h.StartupListed != doors || h.InitialAlerts != 3 {
		t.Errorf("counters: %+v", h)
	}
}

func TestInitialState_aRuleCreatedOnASensorThatIsAlreadyActive(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.ready(leakID, monitor.CategoryLeak)
	e.monitored(leakID, monitor.CategoryLeak, "on")
	e.ready("binary_sensor.door", monitor.CategoryDoor)
	e.monitored("binary_sensor.door", monitor.CategoryDoor, "on")
	e.addV2(NewRule{Entity: leakID, Kind: KindState})
	e.addV2(NewRule{Entity: "binary_sensor.door", Kind: KindState})
	e.eval()
	e.flush()
	e.clk.Advance(time.Hour)
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || !got[0].Initial || got[0].Category != monitor.CategoryLeak {
		t.Fatalf("a hazard rule on a wet sensor alerts; a door rule on an open door stays silent and never appears in a 'when NeptHass started' summary: %+v", got)
	}
}

func TestInitialState_aReconnectKeepsTheV040Behavior(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	e.leakRule()
	e.restart()
	e.firstSight(leakID, monitor.CategoryLeak, "off")
	e.eval()
	// The connection drops and, on reconnect, the snapshot reveals a leak that began meanwhile.
	e.monitored(leakID, monitor.CategoryLeak, "on")
	c := e.change(leakID, monitor.CategoryLeak, "off", "on", monitor.SourceSnapshot)
	e.emit(c)
	e.eval()
	e.flush()
	got := e.sent()
	if len(got) != 1 || !got[0].Late || got[0].Initial {
		t.Fatalf("a reconnect is not a start: the alert is late, not initial: %+v", got)
	}
}

func TestInitialState_theSummaryIsRecheckedForSharingBeforeItIsSent(t *testing.T) {
	e := newEF(t, nil)
	e.configure()
	for _, id := range []string{"binary_sensor.d1", "binary_sensor.d2"} {
		e.ready(id, monitor.CategoryDoor)
		e.addV2(NewRule{Entity: entities.ID(id), Kind: KindState})
	}
	e.restart()
	e.firstSight("binary_sensor.d1", monitor.CategoryDoor, "on")
	e.firstSight("binary_sensor.d2", monitor.CategoryDoor, "on")
	e.eval()
	e.clk.Advance(2 * time.Minute)
	e.eval()
	if _, err := e.perms.Revoke(ctx, "binary_sensor.d2"); err != nil { // revoked between production and delivery
		t.Fatal(err)
	}
	e.flush()
	got := e.sent()
	if len(got) != 1 || len(got[0].Lines) != 1 {
		t.Fatalf("a line whose sensor is no longer shared is removed before sending: %+v", got)
	}
}
