package discord

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

func (a *afix) post(i int) string {
	a.t().Helper()
	posts := a.stub.postsSnapshot()
	if i >= len(posts) {
		a.t().Fatalf("post %d does not exist: %d posts", i, len(posts))
	}
	s, _ := posts[i].Body["content"].(string)
	return s
}

// apply makes the tracker know a reading, as the monitor does before it reports the change.
func (a *afix) apply(id string, cat monitor.Category, name, raw, unit string) monitor.Sensor {
	a.now = a.now.Add(time.Second)
	a.tracker.Apply(monitor.Update{ID: entities.ID(id), Category: cat, Name: name, Raw: raw, Unit: unit, LastChanged: a.now, LastUpdated: a.now}, a.now)
	s, _ := a.tracker.Get(entities.ID(id))
	return s
}

func (a *afix) live(prev, cur monitor.Sensor) {
	a.seq++
	a.eng.OnChange(monitor.Change{ID: cur.ID, Seq: a.seq, Source: monitor.SourceLive, HadPrev: true, Prev: prev, Cur: cur, At: a.clk.Now()})
}

func TestE2E_aDurationAlertReachesDiscordWithItsWording(t *testing.T) {
	a := newAFix(t, true, nil)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	a.ready("binary_sensor.garage", monitor.CategoryGarageDoor)
	closed, _ := a.tracker.Get("binary_sensor.garage")
	item, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.garage", Kind: notify.KindState, Duration: 10 * time.Minute})
	if err != nil || item.Duration != 10*time.Minute || item.AlreadyActive {
		t.Fatalf("a garage door rule for ten minutes: %+v %v", item, err)
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.clk.await(t, 1)
	open := a.apply("binary_sensor.garage", monitor.CategoryGarageDoor, "Garage", "on", "")
	a.live(closed, open)
	a.clk.await(t, 2) // the health watcher's tick and the evaluator's due timer
	a.clk.Advance(9*time.Minute + 59*time.Second)
	if n := len(a.stub.postsSnapshot()); n != 0 {
		t.Fatalf("nothing a second before the duration: %d posts", n)
	}
	a.clk.Advance(time.Second)
	a.stub.waitPosts(1)
	got := a.post(0)
	if !strings.Contains(got, "**Garage** has been open for 10 minutes") || strings.Contains(got, "binary_sensor") || strings.Contains(got, "at least") {
		t.Fatalf("the delivered alert: %q", got)
	}
	am, _ := a.stub.postsSnapshot()[0].Body["allowed_mentions"].(map[string]any)
	if parse, ok := am["parse"].([]any); !ok || len(parse) != 0 {
		t.Errorf("it still pings no one: %+v", am)
	}
}

func TestE2E_aHazardAlreadyWetIsDeliveredAsWhatHomeAssistantReports(t *testing.T) {
	a := newAFix(t, true, nil)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if err := a.perms.Set(ctx0, entities.Grant{ID: "binary_sensor.leak", Access: entities.AccessRead, GrantedBy: "admin", At: a.now}); err != nil {
		t.Fatal(err)
	}
	a.apply("binary_sensor.leak", monitor.CategoryLeak, "Basement", "off", "")
	// An administrator adds the rule while the sensor is dry, then NeptHass restarts with the sensor wet.
	if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.leak", Kind: notify.KindState}); err != nil {
		t.Fatal(err)
	}
	a.apply("binary_sensor.leak", monitor.CategoryLeak, "Basement", "on", "")
	item, err := a.admin.Rules(ctx0, adminActor)
	if err != nil || len(item) != 1 {
		t.Fatalf("%+v %v", item, err)
	}
	if err := a.eng.Reload(ctx0); err != nil { // the start-up reload of a new process
		t.Fatal(err)
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.stub.waitPosts(1)
	got := a.post(0)
	for _, want := range []string{"**Basement** is wet (water detected), according to Home Assistant", "not a new event", "cannot tell when it began", "Please check it"} {
		if !strings.Contains(got, want) {
			t.Errorf("the start-up hazard alert %q must contain %q", got, want)
		}
	}
}

func TestE2E_aLegacyNumericRuleIsDormantUntilAnAdministratorConfirmsItsUnit(t *testing.T) {
	a := newAFix(t, true, nil)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if err := a.perms.Set(ctx0, entities.Grant{ID: "sensor.attic", Access: entities.AccessRead, GrantedBy: "admin", At: a.now}); err != nil {
		t.Fatal(err)
	}
	cool := a.apply("sensor.attic", monitor.CategoryTemperature, "Attic", "20", "°F")
	a.ready("binary_sensor.door", monitor.CategoryDoor) // a barrier: an ordinary rule whose alert proves the evaluator got past the dormant one
	doorClosed, _ := a.tracker.Get("binary_sensor.door")
	if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.door", Kind: notify.KindState}); err != nil {
		t.Fatal(err)
	}
	thr := 80.0
	id, err := a.store.InsertRule(ctx0, notify.Rule{Entity: "sensor.attic", Kind: notify.KindAbove, Threshold: &thr, Cooldown: zero(), Enabled: true,
		CreatedBy: "legacy", CreatedAt: a.now, UpdatedBy: "legacy", UpdatedAt: a.now}) // a rule written before units were tracked
	if err != nil {
		t.Fatal(err)
	}
	if err := a.eng.Reload(ctx0); err != nil {
		t.Fatal(err)
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.clk.await(t, 1)
	hot := a.apply("sensor.attic", monitor.CategoryTemperature, "Attic", "90", "°F")
	a.live(cool, hot)
	views, _ := a.admin.Rules(ctx0, adminActor)
	var legacy notify.RuleStatus
	for _, v := range views {
		if v.ID == id {
			legacy = v.Status
		}
	}
	if legacy != notify.StatusUnitUnresolved {
		t.Fatalf("a legacy temperature rule is unresolved: %q", legacy)
	}
	if _, err := a.admin.ConfirmUnit(ctx0, userActor, id); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("a normal user cannot confirm a unit: %v", err)
	}
	doorOpen := a.apply("binary_sensor.door", monitor.CategoryDoor, "Door", "on", "")
	a.live(doorClosed, doorOpen)
	a.stub.waitPosts(1)
	if posts := a.stub.postsSnapshot(); len(posts) != 1 || !strings.Contains(a.post(0), "**Door**") {
		t.Fatalf("the evaluator handled the 90 °F reading BEFORE the door and sent nothing for it (a wrong alert would have been delivered first): %d posts", len(posts))
	}
	unit, err := a.admin.ConfirmUnit(ctx0, adminActor, id)
	if err != nil || unit != "°F" {
		t.Fatalf("confirm: %q %v", unit, err)
	}
	a.clk.Advance(time.Minute)
	back := a.apply("sensor.attic", monitor.CategoryTemperature, "Attic", "70", "°F")
	a.live(hot, back)
	over := a.apply("sensor.attic", monitor.CategoryTemperature, "Attic", "95", "°F")
	a.live(back, over)
	a.stub.waitPosts(2)
	if got := a.post(1); !strings.Contains(got, "rose above 80 °F") || !strings.Contains(got, "(now 95 °F)") {
		t.Fatalf("after the confirmation the rule evaluates in °F: %q", got)
	}
}
