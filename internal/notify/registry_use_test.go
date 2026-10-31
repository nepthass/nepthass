package notify

import (
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/monitor"
)

func TestRegistryDrivesRuleBehavior(t *testing.T) {
	for _, c := range []monitor.Category{monitor.CategoryDoor, monitor.CategoryWindow, monitor.CategoryGarageDoor, monitor.CategoryOpening} {
		if m, ok := alertMeaning(c); !ok || m != monitor.MeaningOpen {
			t.Errorf("%s: a state rule fires on open, got %q,%v", c, m, ok)
		}
		if !compatible(KindState, c) || compatible(KindAbove, c) || compatible(KindBelow, c) {
			t.Errorf("%s: a binary class takes state rules only", c)
		}
		if defaultCooldown(c) != time.Minute {
			t.Errorf("%s: default cooldown is one minute, got %v", c, defaultCooldown(c))
		}
	}
	if categoryNoun(monitor.CategoryGarageDoor) != "Garage door sensor" || categoryNoun(monitor.CategoryOpening) != "Opening sensor" || categoryNoun("") != "Sensor" {
		t.Error("nouns come from the registry; an unknown category is just a sensor")
	}
	for _, c := range []monitor.Category{monitor.CategoryTemperature, monitor.CategoryHumidity, monitor.CategoryBattery} {
		if !compatible(KindAbove, c) || !compatible(KindBelow, c) || compatible(KindState, c) {
			t.Errorf("%s: a numeric class takes thresholds only", c)
		}
	}
	if compatible(KindState, "") || compatible(KindAbove, "") {
		t.Error("an unknown category fits no rule kind")
	}
}

func TestOnlyHazardsTravelTheCriticalLane(t *testing.T) {
	for _, s := range monitor.Specs() {
		got := Message{Kind: MsgAlert, Category: s.Category}.Lane()
		want := LaneNormal
		if s.Hazard {
			want = LaneCritical
		}
		if got != want {
			t.Errorf("%s: lane %d, want %d", s.Category, got, want)
		}
	}
	if !monitor.CategoryLeak.Hazard() || monitor.CategoryDoor.Hazard() {
		t.Error("the leak is the only hazard in v0.5.0")
	}
}
