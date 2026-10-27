//go:build integration

package integration

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/setup"
)

var setupCaller = setup.Caller{Actor: adminActor, GuildID: "700000000000000001"}

func ok(t *testing.T) func(setup.Session, error) setup.Session {
	t.Helper()
	return func(s setup.Session, err error) setup.Session {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return s
	}
}

// toDiscover starts a session and moves it to discovery (a live fetch from the real Home Assistant).
func toDiscover(t *testing.T, s stack) setup.Session {
	t.Helper()
	sess, _, err := s.setup.Begin(context.Background(), setupCaller)
	if err != nil {
		t.Fatal(err)
	}
	return ok(t)(s.setup.Navigate(context.Background(), setupCaller, sess.ID, sess.Version, setup.EventNext))
}

// pick selects entities by narrowing the discovery list to each ID in turn.
func pick(t *testing.T, s stack, sess setup.Session, ids ...string) setup.Session {
	t.Helper()
	ctx := context.Background()
	m := ok(t)
	for _, id := range ids {
		sess = m(s.setup.Search(ctx, setupCaller, sess.ID, sess.Version, id))
		sess = m(s.setup.Select(ctx, setupCaller, sess.ID, sess.Version, append(selectedOn(sess), entities.ID(id))))
	}
	return m(s.setup.Search(ctx, setupCaller, sess.ID, sess.Version, ""))
}

// selectedOn returns the offered IDs that are already selected, so a new Select keeps them.
func selectedOn(sess setup.Session) []entities.ID {
	var keep []entities.ID
	for _, id := range sess.Draft.View.Offered {
		if _, ok := sess.Draft.Entries[id]; ok {
			keep = append(keep, id)
		}
	}
	return keep
}

func TestSetup_discoversTheRealVirtualEntities_withSearchFiltersAndStates(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	s := newStack(t)
	sess := toDiscover(t, s)

	all, err := s.setup.Discover(context.Background(), sess)
	lights := ok(t)(s.setup.Filter(context.Background(), setupCaller, sess.ID, sess.Version, "light", false))
	lightView, _ := s.setup.Discover(context.Background(), lights)
	searched := ok(t)(s.setup.Search(context.Background(), setupCaller, lights.ID, lights.Version, "nepthass dev light"))
	searchView, _ := s.setup.Discover(context.Background(), searched)

	if err != nil || all.Total < 33 || all.Pages < 2 || len(all.Items) != entities.PageSize {
		t.Fatalf("discovery of the real instance: %+v (%v)", all, err)
	}
	if len(lightView.Items) == 0 || lightView.Items[0].Domain() != "light" {
		t.Errorf("domain filter: %+v", lightView.Items)
	}
	if searchView.Matches != 1 || searchView.Items[0].ID != light || searchView.Items[0].Name != "NeptHass Dev Light" || !searchView.Items[0].Reliable() {
		t.Errorf("search for the virtual light: %+v", searchView)
	}
	snap, err := s.disc.Snapshot(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{temperature: "21.5", humidity: "45", battery: "87", motion: "off"} {
		item, found := snap.Find(entities.ID(id))
		if !found || item.State != want {
			t.Errorf("%s = %+v (found=%v), want state %q", id, item, found, want)
		}
	}
	if item, _ := snap.Find(temperature); item.Unit != "\u00b0C" || item.DeviceClass != "temperature" {
		t.Errorf("temperature item = %+v", item)
	}
}

func TestSetup_unavailableSensorsAreShownAsNotReporting(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	s := newStack(t)
	if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_sensors_offline"); err != nil {
		t.Fatal(err)
	}
	waitForState(t, temperature, entities.StateUnavailable)

	snap, err := s.disc.Snapshot(context.Background(), true)

	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{motion, door, window, waterLeak, temperature, humidity, battery} {
		if item, found := snap.Find(entities.ID(id)); !found || item.Reliable() {
			t.Errorf("%s = %+v: an offline sensor must be flagged unreliable", id, item)
		}
	}
}

func TestSetup_fullFlowAgainstTheRealHomeAssistant_thenExistingCommandsUseIt(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	s := newStack(t)
	ctx := context.Background()
	m := ok(t)
	sess := pick(t, s, toDiscover(t, s), light, switchID, temperature, door)
	sess = m(s.setup.Navigate(ctx, setupCaller, sess.ID, sess.Version, setup.EventNext))
	sess = m(s.setup.BulkAccess(ctx, setupCaller, sess.ID, sess.Version, setup.BulkControlSupported))
	sess = m(s.setup.Navigate(ctx, setupCaller, sess.ID, sess.Version, setup.EventNext))
	sess = m(s.setup.Focus(ctx, setupCaller, sess.ID, sess.Version, light))
	sess = m(s.setup.SetAlias(ctx, setupCaller, sess.ID, sess.Version, "Integration Light"))
	sess = m(s.setup.Navigate(ctx, setupCaller, sess.ID, sess.Version, setup.EventNext))

	plan, planErr := s.setup.Plan(ctx, setupCaller, sess.ID)
	untouched, _ := s.perms.List(ctx)
	result, err := s.setup.Apply(ctx, setupCaller, sess.ID, sess.Version)

	if planErr != nil || plan.New != 4 || plan.AliasChanges != 1 || len(plan.Problems()) != 0 {
		t.Fatalf("plan = %+v (%v)", plan, planErr)
	}
	if len(untouched) != 0 {
		t.Fatalf("nothing may be applied before confirmation: %+v", untouched)
	}
	if err != nil || result.New != 4 {
		t.Fatalf("apply: %+v %v", result, err)
	}

	// The existing v0.1.0 commands see the new configuration immediately.
	status, statusErr := s.svc.Status(ctx, userActor, mustID(t, temperature))
	resolved, resolveErr := s.svc.Resolve(ctx, userActor, "integration light", entities.AccessControl)
	_, controlErr := s.svc.Control(ctx, userActor, resolved, entities.ActionOn)
	waitForState(t, light, "on")
	_, controlOff := s.svc.Control(ctx, userActor, resolved, entities.ActionOff)
	waitForState(t, light, "off")
	_, doorControl := s.svc.Control(ctx, userActor, mustID(t, door), entities.ActionOn)
	_, hidden := s.svc.Status(ctx, userActor, mustID(t, battery))
	page, devErr := s.svc.Devices(ctx, userActor, 1)

	if statusErr != nil || status.State != "21.5" || status.Unit() != "\u00b0C" || resolveErr != nil || resolved != light {
		t.Errorf("status=%+v (%v) resolved=%q (%v)", status, statusErr, resolved, resolveErr)
	}
	if controlErr != nil || controlOff != nil {
		t.Errorf("control through the alias: %v / %v", controlErr, controlOff)
	}
	if !errors.Is(doorControl, entities.ErrAccessDenied) || !errors.Is(hidden, entities.ErrAccessDenied) {
		t.Errorf("door control=%v unshared battery=%v, want ErrAccessDenied", doorControl, hidden)
	}
	if devErr != nil || page.Total != 4 {
		t.Errorf("Devices = %+v (%v)", page, devErr)
	}
	for _, d := range page.Devices {
		if d.State == nil || (d.ID == light && d.Alias != "Integration Light") {
			t.Errorf("device %+v", d)
		}
	}
	if access, _ := s.perms.Get(ctx, mustID(t, door)); access != entities.AccessRead {
		t.Errorf("a sensor selected with the control rule must be read-only, got %q", access)
	}
	entries, _ := s.audit.Recent(ctx, 50)
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Command]++
		if e.Outcome == audit.OutcomeError {
			t.Errorf("unexpected failed audit entry %+v", e)
		}
	}
	if counts["setup.allow"] != 4 || counts["setup.alias"] != 1 || counts["setup.apply"] != 1 || counts["control"] != 3 {
		t.Errorf("audit counts = %v", counts)
	}
}

func TestSetup_readOnlyDomainsNeverReceiveControl(t *testing.T) {
	resetSimulation(t)
	s := newStack(t)
	ctx := context.Background()
	sess := pick(t, s, toDiscover(t, s), motion, light)
	sess = ok(t)(s.setup.Navigate(ctx, setupCaller, sess.ID, sess.Version, setup.EventNext))
	sess = ok(t)(s.setup.Focus(ctx, setupCaller, sess.ID, sess.Version, motion))

	_, sensorControl := s.setup.SetAccess(ctx, setupCaller, sess.ID, sess.Version, entities.AccessControl)
	bulk := ok(t)(s.setup.BulkAccess(ctx, setupCaller, sess.ID, sess.Version, setup.BulkControlSupported))

	if !errors.Is(sensorControl, entities.ErrNotControllable) {
		t.Errorf("control on a motion sensor: %v", sensorControl)
	}
	if bulk.Draft.Entries[mustID(t, motion)].Desired.Access != entities.AccessRead || bulk.Draft.Entries[mustID(t, light)].Desired.Access != entities.AccessControl {
		t.Errorf("bulk rule: %+v", bulk.Draft.Entries)
	}
}

func TestSetup_anOutdatedConfirmationDoesNotRestoreAPermissionRevokedMeanwhile(t *testing.T) {
	resetSimulation(t)
	s := newStack(t)
	ctx := context.Background()
	m := ok(t)
	if err := s.perms.Set(ctx, entities.Grant{ID: mustID(t, light), Access: entities.AccessControl, GrantedBy: adminActor.UserID}); err != nil {
		t.Fatal(err)
	}
	sess := pick(t, s, toDiscover(t, s), light) // selected while shared with control
	sess = m(s.setup.Navigate(ctx, setupCaller, sess.ID, sess.Version, setup.EventNext))
	sess = m(s.setup.Navigate(ctx, setupCaller, sess.ID, sess.Version, setup.EventNext))
	sess = m(s.setup.Focus(ctx, setupCaller, sess.ID, sess.Version, light))
	sess = m(s.setup.SetAlias(ctx, setupCaller, sess.ID, sess.Version, "Light"))
	sess = m(s.setup.Navigate(ctx, setupCaller, sess.ID, sess.Version, setup.EventNext))
	if _, err := s.svc.Revoke(ctx, adminActor, mustID(t, light)); err != nil { // /permissions revoke, meanwhile
		t.Fatal(err)
	}

	_, err := s.setup.Apply(ctx, setupCaller, sess.ID, sess.Version)
	_, replay := s.setup.Apply(ctx, setupCaller, sess.ID, sess.Version)

	access, _ := s.perms.Get(ctx, mustID(t, light))
	alias, _ := s.aliases.Get(ctx, mustID(t, light))
	var stale *setup.StaleConfigError
	if !errors.As(err, &stale) || access != entities.AccessNone || alias != "" {
		t.Errorf("the outdated confirmation must change nothing: err=%v access=%q alias=%q", err, access, alias)
	}
	if !errors.Is(replay, setup.ErrStale) {
		t.Errorf("the old confirmation must be dead, got %v", replay)
	}
}

func TestSetup_sessionsSurviveARestartOfTheProcess(t *testing.T) {
	resetSimulation(t)
	path := filepath.Join(t.TempDir(), "restart-setup.db")
	first := openStack(t, path)
	sess := pick(t, first, toDiscover(t, first), light, switchID)
	sess = ok(t)(first.setup.Navigate(context.Background(), setupCaller, sess.ID, sess.Version, setup.EventNext))
	if err := first.db.Close(); err != nil {
		t.Fatal(err)
	}

	second := openStack(t, path)
	resumed, wasResumed, err := second.setup.Begin(context.Background(), setupCaller)

	if err != nil || !wasResumed || resumed.ID != sess.ID || resumed.Step != setup.StepPermissions || len(resumed.Draft.Entries) != 2 {
		t.Errorf("resumed = %+v resumed=%v err=%v", resumed, wasResumed, err)
	}
}

func TestSync_againstTheRealHomeAssistant(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	s := newStack(t)
	ctx := context.Background()
	if err := s.perms.Set(ctx, entities.Grant{ID: "light.does_not_exist_anymore", Access: entities.AccessRead, GrantedBy: adminActor.UserID}); err != nil {
		t.Fatal(err)
	}
	if err := s.perms.Set(ctx, entities.Grant{ID: mustID(t, temperature), Access: entities.AccessRead, GrantedBy: adminActor.UserID}); err != nil {
		t.Fatal(err)
	}

	first, err := s.disc.Sync(ctx)
	if err != nil || !first.FirstSync || first.Total < 33 || first.Controllable < 2 || len(first.New) != 0 {
		t.Fatalf("first sync = %+v (%v)", first, err)
	}
	// An entity Home Assistant reports that NeptHass had not recorded yet is "new".
	if _, err := s.db.Exec("DELETE FROM known_entities WHERE entity_id = ?", battery); err != nil {
		t.Fatal(err)
	}
	if err := target.admin.runScript(ctx, "nepthass_dev_scenario_sensors_offline"); err != nil {
		t.Fatal(err)
	}
	waitForState(t, temperature, entities.StateUnavailable)
	second, err := s.disc.Sync(ctx)

	if err != nil || second.FirstSync || len(second.New) != 1 || second.New[0] != battery || len(second.Removed) != 0 {
		t.Errorf("second sync = %+v (%v)", second, err)
	}
	if len(second.SharedMissing) != 1 || second.SharedMissing[0] != "light.does_not_exist_anymore" {
		t.Errorf("shared-but-missing = %v", second.SharedMissing)
	}
	if len(second.SharedUnavailable) != 1 || second.SharedUnavailable[0] != temperature {
		t.Errorf("shared-but-unavailable = %v", second.SharedUnavailable)
	}
	if access, _ := s.perms.Get(ctx, battery); access != entities.AccessNone {
		t.Errorf("sync shared a new entity: %q", access)
	}
}
