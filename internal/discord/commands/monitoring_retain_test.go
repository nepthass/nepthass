package commands_test

import (
	"strings"
	"testing"

	"github.com/nepthass/nepthass/internal/monitor"
)

func TestSensors_aSensorThatLosesItsDeviceClassWhileUnavailableIsListedAsNotReporting(t *testing.T) {
	le := newLiveEnv(t, nil)
	le.shareBoth(t)
	le.start()
	le.wait(t, func(s monitor.Status) bool { return s.Healthy })

	le.srv.Update("binary_sensor.door", "unavailable", map[string]any{"restored": true}) // no device_class
	le.wait(t, func(monitor.Status) bool {
		s, _ := le.mon.Tracker().Get("binary_sensor.door")
		return s.Raw == "unavailable"
	})
	got := le.run(user, "sensors", "", nil)

	mustContain(t, got, "Door", "unavailable (not reporting)", "Temp")
	for _, safe := range []string{"closed", "open (on)", "dry", "clear"} {
		if strings.Contains(got, safe) {
			t.Errorf("a not-reporting sensor was shown as %q:\n%s", safe, got)
		}
	}
}
