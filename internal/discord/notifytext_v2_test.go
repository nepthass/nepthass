package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

func TestFormatMessage_durationInitialAndStartupWording(t *testing.T) {
	thr := 30.0
	changed := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		m    notify.Message
		has  []string
		not  []string
	}{
		{"an exact duration", notify.Message{Kind: notify.MsgAlert, Category: monitor.CategoryDoor, Name: "Front door", Trigger: notify.KindState, Meaning: monitor.MeaningOpen, Duration: 10 * time.Minute},
			[]string{"**Front door** has been open for 10 minutes"}, []string{"at least", "not a new event"}},
		{"a duration that NeptHass did not see begin", notify.Message{Kind: notify.MsgAlert, Category: monitor.CategoryGarageDoor, Name: "Garage", Trigger: notify.KindState, Meaning: monitor.MeaningOpen, Duration: 90 * time.Minute, HeldAtLeast: true},
			[]string{"has been open for at least 1 hour 30 minutes"}, nil},
		{"a numeric duration", notify.Message{Kind: notify.MsgAlert, Category: monitor.CategoryTemperature, Name: "Attic", Trigger: notify.KindAbove, Threshold: &thr, Value: 31.2, HasValue: true, Unit: "°C", Duration: 5 * time.Minute},
			[]string{"has been above 30 °C for 5 minutes (now 31.2 °C)"}, nil},
		{"motion", notify.Message{Kind: notify.MsgAlert, Category: monitor.CategoryMotion, Name: "Hall", Trigger: notify.KindState, Meaning: monitor.MeaningDetected, Duration: 3 * time.Minute},
			[]string{"has been detecting motion for 3 minutes"}, nil},
		{"a hazard found at start-up", notify.Message{Kind: notify.MsgAlert, Category: monitor.CategoryLeak, Name: "Basement", Trigger: notify.KindState, Meaning: monitor.MeaningWet, Initial: true, ChangedAt: changed, ReportedAt: changed},
			[]string{"**Basement** is wet (water detected), according to Home Assistant", "found it like this when it started", "not a new event", "cannot tell when it began", "last updated it at", "may be reporting an old state", "Please check it"}, []string{" at 09", "back to normal"}},
		{"a start-up summary", notify.Message{Kind: notify.MsgStartup, MoreLines: 3, Lines: []notify.DigestLine{
			{Category: monitor.CategoryDoor, Name: "Front door", Trigger: notify.KindState, Meaning: monitor.MeaningOpen},
			{Category: monitor.CategoryHumidity, Name: "Bathroom", Trigger: notify.KindAbove, Threshold: &thr, Value: 70, HasValue: true, Unit: "%"}}},
			[]string{"NeptHass started and found these sensors already in an alert state", "cannot tell when they changed", "- **Front door**: is now open", "- **Bathroom**: rose above 30 % (now 70 %)", "- and 3 more"}, nil},
		{"hostile names stay inert in every new message", notify.Message{Kind: notify.MsgStartup, Lines: []notify.DigestLine{
			{Category: monitor.CategoryDoor, Name: "@everyone <@123456789012345678> [x](http://evil.example)", Trigger: notify.KindState, Meaning: monitor.MeaningOpen}}},
			[]string{"\\@everyone", "\\<\\@123456789012345678", "\\[x]"}, []string{"<@1", " @everyone"}},
	}
	for _, c := range cases {
		got := FormatMessage(c.m)
		for _, want := range c.has {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q must contain %q", c.name, got, want)
			}
		}
		for _, bad := range c.not {
			if strings.Contains(got, bad) {
				t.Errorf("%s: %q must not contain %q", c.name, got, bad)
			}
		}
		if len(got) > maxMessage {
			t.Errorf("%s: %d characters", c.name, len(got))
		}
	}
}

func TestDurationText(t *testing.T) {
	for in, want := range map[time.Duration]string{time.Minute: "1 minute", 10 * time.Minute: "10 minutes", time.Hour: "1 hour", 25 * time.Hour: "25 hours",
		90 * time.Minute: "1 hour 30 minutes", time.Hour + time.Second: "1 hour 1 second", 0: "0 seconds"} {
		if got := durationText(in); got != want {
			t.Errorf("durationText(%v) = %q, want %q", in, got, want)
		}
	}
}
