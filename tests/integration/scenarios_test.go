//go:build integration

package integration

import (
	"context"
	"testing"

	"github.com/nepthass/nepthass/internal/entities"
)

var allSensors = []string{motion, door, window, waterLeak, temperature, humidity, battery}

// These tests prove the simulation works end to end. The same scenarios drive
// the future real-time monitoring and notification milestones.
func TestScenarios_changeRealHomeAssistantState(t *testing.T) {
	tests := []struct {
		script string
		want   map[string]string
	}{
		{"nepthass_dev_scenario_motion_detected", map[string]string{motion: "on"}},
		{"nepthass_dev_scenario_door_opened", map[string]string{door: "on"}},
		{"nepthass_dev_scenario_window_opened", map[string]string{window: "on"}},
		{"nepthass_dev_scenario_high_temperature", map[string]string{temperature: "31.2"}},
		{"nepthass_dev_scenario_water_leak", map[string]string{waterLeak: "on"}},
		{"nepthass_dev_scenario_low_battery", map[string]string{battery: "12"}},
	}
	for _, tt := range tests {
		t.Run(tt.script, func(t *testing.T) {
			resetSimulation(t)
			t.Cleanup(func() { resetSimulation(t) })

			if err := target.admin.runScript(context.Background(), tt.script); err != nil {
				t.Fatal(err)
			}

			for id, state := range tt.want {
				waitForState(t, id, state)
			}
		})
	}
}

func TestScenarios_resetRestoresTheBaseline(t *testing.T) {
	resetSimulation(t)
	for _, name := range []string{"motion_detected", "door_opened", "window_opened", "high_temperature", "water_leak", "low_battery", "sensors_offline"} {
		if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_"+name); err != nil {
			t.Fatal(err)
		}
	}
	waitForState(t, motion, "unavailable")

	resetSimulation(t)

	for id, want := range map[string]string{motion: "off", door: "off", window: "off", waterLeak: "off", temperature: "21.5", humidity: "45", battery: "87"} {
		waitForState(t, id, want)
	}
}

func TestScenarios_offlineSensorsReportUnavailable_andNeptHassNeverTreatsThemAsSafe(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	s := newStack(t)
	for _, id := range allSensors {
		s.allow(t, id, entities.AccessRead)
	}

	if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_sensors_offline"); err != nil {
		t.Fatal(err)
	}

	for _, id := range allSensors {
		waitForState(t, id, entities.StateUnavailable)
		st, err := s.svc.Status(context.Background(), userActor, mustID(t, id))
		if err != nil {
			t.Fatalf("Status(%s) error = %v", id, err)
		}
		if st.Reliable() {
			t.Errorf("%s reports %q but Reliable() is true: unavailable must never look safe", id, st.State)
		}
	}
}

func TestScenarios_singleSensorCanGoOfflineAndRecover(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	const helper = "input_boolean"

	if err := target.admin.call(context.Background(), helper, "turn_on", map[string]any{"entity_id": "input_boolean.nepthass_dev_offline_motion"}); err != nil {
		t.Fatal(err)
	}
	waitForState(t, motion, "unavailable")
	waitForState(t, door, "off") // other sensors are unaffected
	if err := target.admin.call(context.Background(), helper, "turn_off", map[string]any{"entity_id": "input_boolean.nepthass_dev_offline_motion"}); err != nil {
		t.Fatal(err)
	}

	waitForState(t, motion, "off")
}
