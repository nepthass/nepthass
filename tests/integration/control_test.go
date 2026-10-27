//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
)

func mustID(t *testing.T, s string) entities.ID {
	t.Helper()
	id, err := entities.ParseID(s)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestControl_turnsVirtualLightAndSwitchOnAndOff_inRealHomeAssistant(t *testing.T) {
	for _, id := range []string{light, switchID} {
		t.Run(id, func(t *testing.T) {
			resetSimulation(t)
			s := newStack(t)
			s.allow(t, id, entities.AccessControl)

			if _, err := s.svc.Control(context.Background(), userActor, mustID(t, id), entities.ActionOn); err != nil {
				t.Fatalf("Control(on) error = %v", err)
			}
			waitForState(t, id, "on")
			if _, err := s.svc.Control(context.Background(), userActor, mustID(t, id), entities.ActionOff); err != nil {
				t.Fatalf("Control(off) error = %v", err)
			}
			waitForState(t, id, "off")

			entries, err := s.audit.Recent(context.Background(), 10)
			if err != nil {
				t.Fatal(err)
			}
			// Newest first: control off, control on, permissions.allow.
			if len(entries) != 3 ||
				entries[0].Command != "control" || entries[0].Action != "off" || entries[0].Outcome != audit.OutcomeSuccess || entries[0].ActorID != userActor.UserID || entries[0].EntityID != id ||
				entries[1].Command != "control" || entries[1].Action != "on" || entries[1].Outcome != audit.OutcomeSuccess ||
				entries[2].Command != "permissions.allow" || entries[2].ActorID != adminActor.UserID {
				t.Errorf("audit entries = %+v", entries)
			}
		})
	}
}

func TestControl_readOnlySensorsCanNeverBeControlled_andStayUntouched(t *testing.T) {
	resetSimulation(t)
	s := newStack(t)
	for _, id := range []string{motion, door, window, waterLeak, temperature, humidity, battery} {
		t.Run(id, func(t *testing.T) {
			grantErr := s.svc.Allow(context.Background(), adminActor, mustID(t, id), entities.AccessControl)
			// Simulate a tampered database row that bypasses Allow().
			tamper := s.perms.Set(context.Background(), entities.Grant{ID: mustID(t, id), Access: entities.AccessControl, GrantedBy: "tamper"})
			_, serviceErr := s.svc.Control(context.Background(), userActor, mustID(t, id), entities.ActionOn)
			clientErr := s.ha.Control(context.Background(), mustID(t, id), entities.ActionOn)

			if !errors.Is(grantErr, entities.ErrNotControllable) {
				t.Errorf("Allow(control) error = %v, want ErrNotControllable", grantErr)
			}
			if tamper != nil {
				t.Fatalf("tampering setup failed: %v", tamper)
			}
			if !errors.Is(serviceErr, entities.ErrNotControllable) {
				t.Errorf("Control() with a tampered row error = %v, want ErrNotControllable", serviceErr)
			}
			if !errors.Is(clientErr, rest.ErrServiceNotAllowed) {
				t.Errorf("REST client error = %v, want ErrServiceNotAllowed", clientErr)
			}
		})
	}
	for id, want := range map[string]string{motion: "off", door: "off", window: "off", waterLeak: "off", temperature: "21.5", humidity: "45", battery: "87"} {
		st, err := target.admin.state(context.Background(), id)
		if err != nil || st.State != want {
			t.Errorf("%s = %q (%v) after refused control attempts, want unchanged %q", id, st.State, err, want)
		}
	}
}

func TestControl_isDeniedWithoutPermission_andLeavesTheDeviceAlone(t *testing.T) {
	resetSimulation(t)
	s := newStack(t)
	s.allow(t, light, entities.AccessRead)

	_, noRow := s.svc.Control(context.Background(), userActor, mustID(t, switchID), entities.ActionOn)
	_, readOnly := s.svc.Control(context.Background(), userActor, mustID(t, light), entities.ActionOn)

	if !errors.Is(noRow, entities.ErrAccessDenied) || !errors.Is(readOnly, entities.ErrAccessDenied) {
		t.Errorf("errors = %v / %v, want ErrAccessDenied for both", noRow, readOnly)
	}
	for _, id := range []string{light, switchID} {
		st, err := target.admin.state(context.Background(), id)
		if err != nil || st.State != "off" {
			t.Errorf("%s = %q (%v), want it untouched", id, st.State, err)
		}
	}
	entries, err := s.audit.Recent(context.Background(), 2)
	if err != nil || len(entries) != 2 || entries[0].Outcome != audit.OutcomeDenied || entries[1].Outcome != audit.OutcomeDenied {
		t.Errorf("audit = %+v (%v), want two denied entries", entries, err)
	}
}
