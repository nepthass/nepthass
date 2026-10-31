package discord

import (
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

// Exact v0.5.0 alert wording for every supported class. These strings are a compatibility
// requirement: they were captured from the unchanged v0.5.0 code and must not change by accident.
func TestGolden_stateAndDurationWordingOfEverySupportedClass(t *testing.T) {
	cases := []struct {
		cat     monitor.Category
		meaning monitor.Meaning
		state   string
		held    string
	}{
		{monitor.CategoryDoor, monitor.MeaningOpen, "is now open", "has been open for 10 minutes"},
		{monitor.CategoryWindow, monitor.MeaningOpen, "is now open", "has been open for 10 minutes"},
		{monitor.CategoryGarageDoor, monitor.MeaningOpen, "is now open", "has been open for 10 minutes"},
		{monitor.CategoryOpening, monitor.MeaningOpen, "is now open", "has been open for 10 minutes"},
		{monitor.CategoryMotion, monitor.MeaningDetected, "detected motion", "has been detecting motion for 10 minutes"},
		{monitor.CategoryBatteryLow, monitor.MeaningLow, "reports a low battery", "has reported a low battery for 10 minutes"},
		{monitor.CategoryLeak, monitor.MeaningWet, "is wet (water detected)", "has been wet for 10 minutes"},
	}
	for _, c := range cases {
		spec, ok := monitor.SpecFor(c.cat)
		if !ok || spec.AlertMeaning() != c.meaning {
			t.Fatalf("%s: the registry row's alert meaning changed", c.cat)
		}
		m := notify.Message{Kind: notify.MsgAlert, Category: c.cat, Name: "Hall", Trigger: notify.KindState, Meaning: c.meaning, ObservedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
		if got := stateClause(m.Category, m.Trigger, m.Meaning, 0, false, "", nil); got != c.state {
			t.Errorf("%s state wording: %q, want %q", c.cat, got, c.state)
		}
		m.Duration = 10 * time.Minute
		if got := heldClause(m); got != c.held {
			t.Errorf("%s held wording: %q, want %q", c.cat, got, c.held)
		}
	}
	// The class wording only ever describes the class's own alert meaning: any other meaning keeps the
	// v0.5.0 generic fallback and is never worded as an alert.
	if got := stateClause(monitor.CategoryDoor, notify.KindState, monitor.MeaningClosed, 0, false, "", nil); got != "changed state" {
		t.Errorf("a door that is closed must not be worded as open: %q", got)
	}
	if got := heldClause(notify.Message{Category: monitor.CategoryDoor, Meaning: monitor.MeaningClosed, Duration: 10 * time.Minute}); got != "has been in its alert state for 10 minutes" {
		t.Errorf("held wording for a non-alert meaning: %q", got)
	}
	th := 30.0
	if got := stateClause(monitor.CategoryTemperature, notify.KindAbove, monitor.MeaningReading, 31.2, true, "\u00b0C", &th); got != "rose above 30 \u00b0C (now 31.2 \u00b0C)" {
		t.Errorf("numeric wording: %q", got)
	}
}
