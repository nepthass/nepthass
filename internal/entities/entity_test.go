package entities

import (
	"errors"
	"strings"
	"testing"
)

func TestParseID(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
	}{
		{"light.kitchen", false},
		{"switch.fan_2", false},
		{"sensor.9th_floor", false},
		{"light", true},
		{".kitchen", true},
		{"light.", true},
		{"light.kitchen.extra", true},
		{"Light.kitchen", true},
		{"light.kit chen", true},
		{"light.kitchen/../../config", true},
		{"light.kitchen?x=1", true},
		{"light.kitchen'; DROP TABLE x;--", true},
		{"", true},
		{"light." + strings.Repeat("a", 300), true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			id, err := ParseID(tt.in)

			if tt.wantErr {
				if !errors.Is(err, ErrInvalidID) {
					t.Fatalf("ParseID(%q) error = %v, want ErrInvalidID", tt.in, err)
				}
				return
			}
			if err != nil || string(id) != tt.in {
				t.Fatalf("ParseID(%q) = %q, %v", tt.in, id, err)
			}
		})
	}
}

func TestDomain_controllable_isDenyByDefault(t *testing.T) {
	allowed := map[Domain]bool{DomainLight: true, DomainSwitch: true}
	for _, d := range []Domain{
		"light", "switch", "lock", "cover", "garage_door", "alarm_control_panel",
		"climate", "camera", "siren", "valve", "water_heater", "sensor", "binary_sensor", "",
	} {
		if got := d.Controllable(); got != allowed[d] {
			t.Errorf("Domain(%q).Controllable() = %v, want %v", d, got, allowed[d])
		}
	}
}

func TestParseAction(t *testing.T) {
	for in, want := range map[string]Action{"on": ActionOn, "off": ActionOff} {
		got, err := ParseAction(in)
		if err != nil || got != want {
			t.Errorf("ParseAction(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "toggle", "ON", "turn_on", "open"} {
		if _, err := ParseAction(bad); !errors.Is(err, ErrInvalidAction) {
			t.Errorf("ParseAction(%q) error = %v, want ErrInvalidAction", bad, err)
		}
	}
}

func TestState_reliable_neverTreatsUnknownOrUnavailableAsValid(t *testing.T) {
	for state, want := range map[string]bool{"on": true, "off": true, "21.5": true, "unavailable": false, "unknown": false} {
		if got := (State{State: state}).Reliable(); got != want {
			t.Errorf("State(%q).Reliable() = %v, want %v", state, got, want)
		}
	}
}

func TestState_friendlyName_fallsBackToID(t *testing.T) {
	named := State{ID: "light.kitchen", Attributes: map[string]any{"friendly_name": "Kitchen Light"}}
	bare := State{ID: "light.kitchen"}

	if named.FriendlyName() != "Kitchen Light" || bare.FriendlyName() != "light.kitchen" {
		t.Errorf("names = %q, %q", named.FriendlyName(), bare.FriendlyName())
	}
}
