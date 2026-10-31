package wizard

import (
	"strings"
	"testing"

	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/setup"
)

// The discovery list uses the same neutral policy as /status, /devices and /control: no green
// check mark for a reading, the registry meaning for a supported class, the raw state otherwise.
func TestDiscoverLine_isNeutral_andKeepsAvailabilityAccessAndSelection(t *testing.T) {
	item := func(id, state, class string) setup.DiscoverItem {
		return setup.DiscoverItem{Item: entities.Item{ID: entities.ID(id), Name: "X", State: state, DeviceClass: class}, Access: entities.AccessRead, Selected: true}
	}
	cases := []struct {
		name  string
		it    setup.DiscoverItem
		state string
		icon  string
	}{
		{"supported door", item("binary_sensor.d", "on", "door"), "open (on)", ui.IconInfo},
		{"unsupported smoke", item("binary_sensor.s", "on", "smoke"), "on", ui.IconInfo},
		{"unavailable", item("binary_sensor.u", "unavailable", "door"), "unavailable (not reporting)", ui.IconWarn},
		{"unknown", item("sensor.k", "unknown", ""), "unknown (not reporting)", ui.IconWarn},
	}
	for _, c := range cases {
		lines := strings.SplitN(discoverLine(c.it), "\n", 2)
		if len(lines) != 2 {
			t.Fatalf("%s: expected a two-line entry: %q", c.name, lines)
		}
		if strings.Contains(lines[0], ui.IconOK) || !strings.Contains(lines[0], c.icon) {
			t.Errorf("%s: first line %q must carry %q and never the green check mark", c.name, lines[0], c.icon)
		}
		if !strings.HasPrefix(strings.TrimLeft(lines[1], "\u2003"), c.state+" \u00b7 ") {
			t.Errorf("%s: state rendered as %q, want it to start with %q", c.name, lines[1], c.state)
		}
		if !strings.HasPrefix(lines[0], "\u2611") || !strings.Contains(lines[1], accessLabel(entities.AccessRead)) {
			t.Errorf("%s: selection and access must be preserved: %q", c.name, lines)
		}
	}
	gone := item("binary_sensor.g", "on", "door")
	gone.Gone = true
	if l := discoverLine(gone); !strings.Contains(l, "no longer in Home Assistant") || strings.Contains(strings.SplitN(l, "\n", 2)[0], ui.IconOK) || !strings.Contains(strings.SplitN(l, "\n", 2)[0], ui.IconWarn) {
		t.Errorf("a vanished entity keeps its warning: %q", l)
	}
}
