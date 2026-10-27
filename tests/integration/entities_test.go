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
