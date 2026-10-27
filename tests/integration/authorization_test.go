//go:build integration

package integration

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nepthass/nepthass/internal/entities"
)

func TestPrivateEntities_stayInvisibleUntilShared(t *testing.T) {
	s := newStack(t)

	_, status := s.svc.Status(context.Background(), userActor, mustID(t, temperature))
	page, devicesErr := s.svc.Devices(context.Background(), userActor, 1)
	suggestions, suggestErr := s.svc.Suggest(context.Background(), userActor, entities.AccessRead, "nepthass_dev")

	if !errors.Is(status, entities.ErrAccessDenied) {
		t.Errorf("Status() error = %v, want ErrAccessDenied", status)
	}
	if devicesErr != nil || page.Total != 0 || len(page.Devices) != 0 {
		t.Errorf("Devices() = %+v, %v, want nothing", page, devicesErr)
	}
	if suggestErr != nil || len(suggestions) != 0 {
		t.Errorf("Suggest() = %v, %v, want nothing", suggestions, suggestErr)
	}
}

func TestSharing_exposesOnlyWhatWasGranted(t *testing.T) {
	resetSimulation(t)
	s := newStack(t)
	s.allow(t, temperature, entities.AccessRead)
	s.allow(t, light, entities.AccessControl)

	page, err := s.svc.Devices(context.Background(), userActor, 1)
	_, hiddenErr := s.svc.Status(context.Background(), userActor, mustID(t, battery))

	if err != nil || page.Total != 2 || len(page.Devices) != 2 {
		t.Fatalf("Devices() = %+v, %v", page, err)
	}
	for _, d := range page.Devices {
		if d.State == nil {
			t.Errorf("%s has no live state", d.ID)
		}
	}
	if !errors.Is(hiddenErr, entities.ErrAccessDenied) {
		t.Errorf("unshared battery sensor error = %v, want ErrAccessDenied", hiddenErr)
	}
}

func TestPermissions_surviveAProcessRestart(t *testing.T) {
	resetSimulation(t)
	path := filepath.Join(t.TempDir(), "restart.db")
	first := openStack(t, path)
	first.allow(t, light, entities.AccessControl)
	first.allow(t, temperature, entities.AccessRead)
	if err := first.db.Close(); err != nil {
		t.Fatal(err)
	}

	second := openStack(t, path)
	perms, listErr := second.svc.Permissions(context.Background(), adminActor)
	_, controlErr := second.svc.Control(context.Background(), userActor, mustID(t, light), entities.ActionOn)
	entries, auditErr := second.audit.Recent(context.Background(), 10)

	if listErr != nil || len(perms) != 2 {
		t.Fatalf("Permissions() = %+v, %v", perms, listErr)
	}
	if controlErr != nil {
		t.Errorf("Control() after restart error = %v", controlErr)
	}
	waitForState(t, light, "on")
	if auditErr != nil || len(entries) != 3 {
		t.Errorf("audit after restart = %+v (%v), want the two grants and the control", entries, auditErr)
	}
}
