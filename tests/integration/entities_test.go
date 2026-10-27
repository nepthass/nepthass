//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
)

func TestHomeAssistant_authenticatesAndRejectsAWrongToken(t *testing.T) {
	good := rest.New(config.HomeAssistant{URL: target.url, Token: config.Secret(target.token), Timeout: requestTimeout})
	bad := rest.New(config.HomeAssistant{URL: target.url, Token: "not-a-valid-token", Timeout: requestTimeout})

	info, goodErr := good.Info(context.Background())
	_, badErr := bad.Info(context.Background())

	if goodErr != nil || info.Version == "" {
		t.Errorf("Info() = %+v, %v", info, goodErr)
	}
	if !errors.Is(badErr, rest.ErrUnauthorized) {
		t.Errorf("wrong token error = %v, want ErrUnauthorized", badErr)
	}
}

func TestVirtualEntities_existWithPredictableIDsStatesAndClasses(t *testing.T) {
	resetSimulation(t)
	want := []struct {
		id, state, deviceClass, unit string
	}{
		{light, "off", "", ""},
		{switchID, "off", "", ""},
		{motion, "off", "motion", ""},
		{door, "off", "door", ""},
		{window, "off", "window", ""},
		{waterLeak, "off", "moisture", ""},
		{temperature, "21.5", "temperature", "\u00b0C"},
		{humidity, "45", "humidity", "%"},
		{battery, "87", "battery", "%"},
	}
	s := newStack(t)

	for _, w := range want {
		t.Run(w.id, func(t *testing.T) {
			s.allow(t, w.id, "read")

			st, err := s.svc.Status(context.Background(), userActor, mustID(t, w.id))

			if err != nil {
				t.Fatalf("Status() error = %v", err)
			}
			if st.State != w.state || st.DeviceClass() != w.deviceClass || st.Unit() != w.unit {
				t.Errorf("state=%q class=%q unit=%q, want %q %q %q", st.State, st.DeviceClass(), st.Unit(), w.state, w.deviceClass, w.unit)
			}
			if st.FriendlyName() == w.id {
				t.Errorf("entity has no friendly name")
			}
		})
	}
}

func TestStates_listsEveryVirtualEntity(t *testing.T) {
	s := newStack(t)

	states, err := s.ha.States(context.Background())

	if err != nil {
		t.Fatalf("States() error = %v", err)
	}
	present := map[string]bool{}
	for _, st := range states {
		present[string(st.ID)] = true
	}
	for _, id := range []string{light, switchID, motion, door, window, waterLeak, temperature, humidity, battery} {
		if !present[id] {
			t.Errorf("%s missing from /api/states", id)
		}
	}
}

// Regression test for the failure seen when the onboarding UI switched the
// development instance to US customary units: Home Assistant then reported
// 21.5 C as "70.7 F" and 31.2 C as "88.16 F". The reset script must re-apply
// the pinned metric system, and temperatures must be reported in degrees
// Celsius with the expected values (the expectations are not relaxed).
func TestUnits_resetRestoresMetric_afterTheInstanceWasSwitchedToUSCustomary(t *testing.T) {
	t.Cleanup(func() { resetSimulation(t) })
	if err := wsCommand(context.Background(), map[string]any{"type": "config/core/update", "unit_system": "us_customary"}); err != nil {
		t.Fatalf("switch to US customary: %v", err)
	}
	unit, err := target.admin.temperatureUnit(context.Background())
	if err != nil || unit != "\u00b0F" {
		t.Fatalf("precondition: instance temperature unit = %q (%v), want \u00b0F", unit, err)
	}

	resetSimulation(t)
	if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_high_temperature"); err != nil {
		t.Fatal(err)
	}

	unit, err = target.admin.temperatureUnit(context.Background())
	if err != nil || unit != "\u00b0C" {
		t.Errorf("after reset the instance temperature unit = %q (%v), want \u00b0C", unit, err)
	}
	waitForState(t, temperature, "31.2")
	st, err := target.admin.state(context.Background(), temperature)
	if err != nil || st.Attributes["unit_of_measurement"] != "\u00b0C" {
		t.Errorf("temperature sensor = %+v (%v), want unit \u00b0C", st, err)
	}
}
