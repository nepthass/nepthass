package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

func st(id, state, class, unit string) entities.State {
	a := map[string]any{}
	if class != "" {
		a["device_class"] = class
	}
	if unit != "" {
		a["unit_of_measurement"] = unit
	}
	return entities.State{ID: entities.ID(id), State: state, Attributes: a, LastChanged: time.Now(), LastUpdated: time.Now()}
}

// "on" is not healthy: it is rendered neutrally, with the registry's meaning for a supported class
// and the raw state for any other, and never with a green check mark.
func TestDescribeState_neverCallsOnHealthy_andOnlyInterpretsSupportedClasses(t *testing.T) {
	cases := []struct {
		s    entities.State
		text string
	}{
		{st("binary_sensor.d", "on", "door", ""), "open (on)"},
		{st("binary_sensor.l", "on", "moisture", ""), "water detected (on)"},
		{st("binary_sensor.m", "off", "motion", ""), "clear (off)"},
		{st("binary_sensor.smoke", "on", "smoke", ""), "on"},
		{st("binary_sensor.gas", "on", "gas", ""), "on"},
		{st("sensor.co2", "850", "carbon_dioxide", "ppm"), "850 ppm"},
		{st("binary_sensor.x", "unavailable", "door", ""), "unavailable (not reporting)"},
		{st("binary_sensor.y", "unknown", "", ""), "unknown (not reporting)"},
	}
	// The supported and unsupported readings must match exactly: a substring check would accept
	// "ALARM (on)" for a smoke sensor.
	exact := map[entities.ID]bool{"binary_sensor.d": true, "binary_sensor.l": true, "binary_sensor.m": true, "binary_sensor.smoke": true, "binary_sensor.gas": true, "sensor.co2": true}
	for _, c := range cases {
		got := DescribeState(c.s)
		if exact[c.s.ID] && got != c.text {
			t.Errorf("%s renders %q, want exactly %q", c.s.ID, got, c.text)
		}
		if !strings.Contains(got, c.text) && got != c.text {
			t.Errorf("%s %q renders %q, want %q", c.s.ID, c.s.State, got, c.text)
		}
		if icon := StateIcon(c.s); icon == IconOK {
			t.Errorf("%s: a state must never get the green check mark", c.s.ID)
		}
		if !c.s.Reliable() && StateIcon(c.s) != IconWarn {
			t.Errorf("%s: an unreliable state gets the warning mark", c.s.ID)
		}
	}
}
