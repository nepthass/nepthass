package entities_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/storage"
)

var (
	now   = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	admin = auth.Actor{UserID: "300000000000000001", Level: auth.LevelAdmin}
	user  = auth.Actor{UserID: "300000000000000002", Level: auth.LevelUser}
)

type controlCall struct {
	id     entities.ID
	action entities.Action
}

// fakeHA is an in-memory Home Assistant.
type fakeHA struct {
	states     map[entities.ID]entities.State
	calls      []controlCall
	controlErr error
	statesErr  error
}

func newFakeHA(ids ...entities.ID) *fakeHA {
	f := &fakeHA{states: map[entities.ID]entities.State{}}
	for _, id := range ids {
		f.states[id] = entities.State{ID: id, State: "off"}
	}
	return f
}

func (f *fakeHA) State(_ context.Context, id entities.ID) (entities.State, error) {
	st, ok := f.states[id]
	if !ok {
		return entities.State{}, errors.New("not found")
	}
	return st, nil
}

func (f *fakeHA) States(context.Context) ([]entities.State, error) {
	if f.statesErr != nil {
		return nil, f.statesErr
	}
	out := make([]entities.State, 0, len(f.states))
	for _, st := range f.states {
		out = append(out, st)
	}
	return out, nil
}

func (f *fakeHA) Control(_ context.Context, id entities.ID, action entities.Action) error {
	f.calls = append(f.calls, controlCall{id, action})
	if f.controlErr != nil {
		return f.controlErr
	}
	st := f.states[id]
	st.State = string(action)
	f.states[id] = st
	return nil
}

type fixture struct {
	svc   *entities.Service
	perms *entities.Permissions
	audit *audit.Log
	ha    *fakeHA
}

func newFixture(t *testing.T, ids ...entities.ID) fixture {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "svc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := fixture{perms: entities.NewPermissions(db), audit: audit.New(db), ha: newFakeHA(ids...)}
	f.svc = entities.NewService(entities.Deps{
		HA: f.ha, Permissions: f.perms, Audit: f.audit,
		Logger: slog.New(slog.DiscardHandler), Now: func() time.Time { return now },
	})
	return f
}

func (f fixture) grant(t *testing.T, id entities.ID, access entities.Access) {
	t.Helper()
	if err := f.perms.Set(context.Background(), entities.Grant{ID: id, Access: access, GrantedBy: admin.UserID, At: now}); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) lastAudit(t *testing.T) audit.Entry {
	t.Helper()
	entries, err := f.audit.Recent(context.Background(), 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit entries = %v, err = %v", entries, err)
	}
	return entries[0]
}

func TestStatus_deniesEntitiesWithoutPermission(t *testing.T) {
	f := newFixture(t, "light.kitchen")

	_, err := f.svc.Status(context.Background(), user, "light.kitchen")

	if !errors.Is(err, entities.ErrAccessDenied) {
		t.Errorf("error = %v, want ErrAccessDenied", err)
	}
}

func TestStatus_returnsState_whenReadPermissionExists(t *testing.T) {
	f := newFixture(t, "sensor.hall_temperature")
	f.grant(t, "sensor.hall_temperature", entities.AccessRead)

	st, err := f.svc.Status(context.Background(), user, "sensor.hall_temperature")

	if err != nil || st.ID != "sensor.hall_temperature" {
		t.Errorf("state=%+v err=%v", st, err)
	}
}

func TestStatus_rejectsActorsBelowUserLevel(t *testing.T) {
	f := newFixture(t, "light.kitchen")
	f.grant(t, "light.kitchen", entities.AccessControl)

	_, err := f.svc.Status(context.Background(), auth.Actor{UserID: "1"}, "light.kitchen")

	if !errors.Is(err, auth.ErrNotAuthorized) {
		t.Errorf("error = %v, want ErrNotAuthorized", err)
	}
}

func TestControl_switchesEntity_andAuditsSuccess(t *testing.T) {
	f := newFixture(t, "light.kitchen")
	f.grant(t, "light.kitchen", entities.AccessControl)

	res, err := f.svc.Control(context.Background(), user, "light.kitchen", entities.ActionOn)

	if err != nil {
		t.Fatalf("Control() error = %v", err)
	}
	if len(f.ha.calls) != 1 || f.ha.calls[0] != (controlCall{"light.kitchen", entities.ActionOn}) {
		t.Errorf("calls = %v", f.ha.calls)
	}
	if res.Before.State != "off" || res.After == nil || res.After.State != "on" {
		t.Errorf("result = %+v", res)
	}
	want := audit.Entry{Time: now, ActorID: user.UserID, Command: "control", EntityID: "light.kitchen", Action: "on", Outcome: audit.OutcomeSuccess}
	if got := f.lastAudit(t); got != want {
		t.Errorf("audit = %+v, want %+v", got, want)
	}
}

func TestControl_isDeniedAndAudited_withReadOnlyOrNoPermission(t *testing.T) {
	for name, access := range map[string]entities.Access{"read only": entities.AccessRead, "no permission": entities.AccessNone} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "light.kitchen")
			if access != entities.AccessNone {
				f.grant(t, "light.kitchen", access)
			}

			_, err := f.svc.Control(context.Background(), user, "light.kitchen", entities.ActionOn)

			if !errors.Is(err, entities.ErrAccessDenied) {
				t.Fatalf("error = %v, want ErrAccessDenied", err)
			}
			if len(f.ha.calls) != 0 {
				t.Errorf("Home Assistant was called: %v", f.ha.calls)
			}
			if got := f.lastAudit(t); got.Outcome != audit.OutcomeDenied || got.EntityID != "light.kitchen" {
				t.Errorf("audit = %+v", got)
			}
		})
	}
}

func TestControl_refusesDangerousDomains_evenWhenTheDatabaseSaysControl(t *testing.T) {
	f := newFixture(t, "lock.front_door", "cover.garage_door", "alarm_control_panel.home")
	for _, id := range []entities.ID{"lock.front_door", "cover.garage_door", "alarm_control_panel.home"} {
		f.grant(t, id, entities.AccessControl) // tampered rows bypassing Allow()

		_, err := f.svc.Control(context.Background(), admin, id, entities.ActionOff)

		if !errors.Is(err, entities.ErrNotControllable) {
			t.Errorf("Control(%s) error = %v, want ErrNotControllable", id, err)
		}
	}
	if len(f.ha.calls) != 0 {
		t.Errorf("Home Assistant was called: %v", f.ha.calls)
	}
}

func TestControl_auditsHomeAssistantFailure_asError(t *testing.T) {
	f := newFixture(t, "switch.fan")
	f.grant(t, "switch.fan", entities.AccessControl)
	f.ha.controlErr = errors.New("home assistant exploded")

	_, err := f.svc.Control(context.Background(), user, "switch.fan", entities.ActionOff)

	if err == nil {
		t.Fatal("Control() succeeded")
	}
	if got := f.lastAudit(t); got.Outcome != audit.OutcomeError || got.Detail != "home assistant exploded" {
		t.Errorf("audit = %+v", got)
	}
}

func TestAllow_requiresAdministrator(t *testing.T) {
	f := newFixture(t, "light.kitchen")

	err := f.svc.Allow(context.Background(), user, "light.kitchen", entities.AccessControl)

	if !errors.Is(err, auth.ErrAdminRequired) {
		t.Errorf("error = %v, want ErrAdminRequired", err)
	}
	if access, _ := f.perms.Get(context.Background(), "light.kitchen"); access != entities.AccessNone {
		t.Errorf("access = %q, want none", access)
	}
	if got := f.lastAudit(t); got.Command != "permissions.allow" || got.Outcome != audit.OutcomeDenied {
		t.Errorf("audit = %+v", got)
	}
}

func TestAllow_enforcesDomainAllowlistForControl_butAllowsReadOnlyAccess(t *testing.T) {
	f := newFixture(t, "lock.front_door")

	controlErr := f.svc.Allow(context.Background(), admin, "lock.front_door", entities.AccessControl)
	readErr := f.svc.Allow(context.Background(), admin, "lock.front_door", entities.AccessRead)

	if !errors.Is(controlErr, entities.ErrNotControllable) {
		t.Errorf("control error = %v, want ErrNotControllable", controlErr)
	}
	if readErr != nil {
		t.Errorf("read error = %v", readErr)
	}
	if access, _ := f.perms.Get(context.Background(), "lock.front_door"); access != entities.AccessRead {
		t.Errorf("access = %q, want read", access)
	}
}

func TestAllow_rejectsEntitiesHomeAssistantDoesNotKnow(t *testing.T) {
	f := newFixture(t)

	err := f.svc.Allow(context.Background(), admin, "light.typo", entities.AccessControl)

	if err == nil {
		t.Fatal("Allow() succeeded for an unknown entity")
	}
	if perms, _ := f.perms.List(context.Background()); len(perms) != 0 {
		t.Errorf("permissions = %+v", perms)
	}
}

func TestRevoke_removesPermission_andReportsMissing(t *testing.T) {
	f := newFixture(t, "light.kitchen")
	f.grant(t, "light.kitchen", entities.AccessControl)

	first, err1 := f.svc.Revoke(context.Background(), admin, "light.kitchen")
	second, err2 := f.svc.Revoke(context.Background(), admin, "light.kitchen")

	if err1 != nil || err2 != nil || !first || second {
		t.Errorf("revoke results = %v/%v errors %v/%v", first, second, err1, err2)
	}
	if _, err := f.svc.Revoke(context.Background(), user, "light.kitchen"); !errors.Is(err, auth.ErrAdminRequired) {
		t.Errorf("non-admin revoke error = %v", err)
	}
}

func TestDevices_paginatesAndFlagsEntitiesMissingFromHomeAssistant(t *testing.T) {
	f := newFixture(t)
	for i := range 12 {
		id := entities.ID(fmt.Sprintf("light.l%02d", i))
		f.grant(t, id, entities.AccessControl)
		if i != 11 { // l11 was removed from Home Assistant
			f.ha.states[id] = entities.State{ID: id, State: "on"}
		}
	}

	page1, err1 := f.svc.Devices(context.Background(), user, 1)
	page2, err2 := f.svc.Devices(context.Background(), user, 2)
	clamped, err3 := f.svc.Devices(context.Background(), user, 99)

	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("errors = %v %v %v", err1, err2, err3)
	}
	if len(page1.Devices) != 10 || page1.Pages != 2 || page1.Total != 12 || page1.Devices[0].ID != "light.l00" {
		t.Errorf("page1 = %+v", page1)
	}
	if len(page2.Devices) != 2 || page2.Devices[1].ID != "light.l11" || page2.Devices[1].State != nil || page2.Devices[0].State == nil {
		t.Errorf("page2 = %+v", page2)
	}
	if clamped.Page != 2 {
		t.Errorf("clamped page = %d, want 2", clamped.Page)
	}
}

func TestDevices_doesNotCallHomeAssistant_whenNothingIsPermitted(t *testing.T) {
	f := newFixture(t)
	f.ha.statesErr = errors.New("must not be called")

	page, err := f.svc.Devices(context.Background(), user, 1)

	if err != nil || page.Total != 0 || page.Pages != 1 {
		t.Errorf("page=%+v err=%v", page, err)
	}
}

func TestSuggest_filtersByAccessAndQuery(t *testing.T) {
	f := newFixture(t)
	f.grant(t, "light.kitchen", entities.AccessControl)
	f.grant(t, "sensor.kitchen_temp", entities.AccessRead)
	f.grant(t, "light.bedroom", entities.AccessControl)

	control, err1 := f.svc.Suggest(context.Background(), user, entities.AccessControl, "KITCHEN")
	read, err2 := f.svc.Suggest(context.Background(), user, entities.AccessRead, "kitchen")

	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if len(control) != 1 || control[0] != "light.kitchen" || len(read) != 2 {
		t.Errorf("control=%v read=%v", control, read)
	}
}

func TestDiscover_isAdminOnly_andMatchesFriendlyNames(t *testing.T) {
	f := newFixture(t)
	f.ha.states["light.l1"] = entities.State{ID: "light.l1", State: "on", Attributes: map[string]any{"friendly_name": "Reading Lamp"}}
	f.ha.states["switch.s1"] = entities.State{ID: "switch.s1", State: "off"}

	found, err := f.svc.Discover(context.Background(), admin, "lamp")
	_, denied := f.svc.Discover(context.Background(), user, "lamp")

	if err != nil || len(found) != 1 || found[0].ID != "light.l1" {
		t.Errorf("found=%+v err=%v", found, err)
	}
	if !errors.Is(denied, auth.ErrAdminRequired) {
		t.Errorf("non-admin error = %v", denied)
	}
}

func TestPermissions_setUpdatesExistingRow(t *testing.T) {
	f := newFixture(t)
	f.grant(t, "light.kitchen", entities.AccessRead)
	f.grant(t, "light.kitchen", entities.AccessControl)

	perms, err := f.svc.Permissions(context.Background(), admin)

	if err != nil || len(perms) != 1 || perms[0].Access != entities.AccessControl || !perms[0].UpdatedAt.Equal(now) {
		t.Errorf("perms=%+v err=%v", perms, err)
	}
}

func TestParseAccess(t *testing.T) {
	for _, in := range []string{"read", "control"} {
		if got, err := entities.ParseAccess(in); err != nil || string(got) != in {
			t.Errorf("ParseAccess(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "write", "admin", "CONTROL"} {
		if _, err := entities.ParseAccess(in); !errors.Is(err, entities.ErrInvalidAccess) {
			t.Errorf("ParseAccess(%q) error = %v", in, err)
		}
	}
}
