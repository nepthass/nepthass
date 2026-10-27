package setup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/storage"
)

type fakeHA struct {
	mu     sync.Mutex
	states map[entities.ID]entities.State
	err    error
	calls  int
	sent   []string
}

func (f *fakeHA) States(context.Context) ([]entities.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]entities.State, 0, len(f.states))
	for _, s := range f.states {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeHA) State(_ context.Context, id entities.ID) (entities.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.states[id]
	if !ok {
		return entities.State{}, errors.New("not found")
	}
	return s, nil
}

func (f *fakeHA) Control(_ context.Context, id entities.ID, a entities.Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, string(id)+":"+string(a))
	return nil
}

func (f *fakeHA) set(id, name, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[entities.ID(id)] = entities.State{ID: entities.ID(id), State: state, Attributes: map[string]any{"friendly_name": name}}
}

func (f *fakeHA) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.states, entities.ID(id))
}

type env struct {
	svc     *Service
	ent     *entities.Service
	perms   *entities.Permissions
	aliases *entities.Aliases
	audit   *audit.Log
	db      *sql.DB
	ha      *fakeHA
	clock   *time.Time
	path    string
}

var (
	admin1   = Caller{Actor: auth.Actor{UserID: "a1", Level: auth.LevelAdmin}, GuildID: "g1"}
	admin2   = Caller{Actor: auth.Actor{UserID: "a2", Level: auth.LevelAdmin}, GuildID: "g1"}
	otherGld = Caller{Actor: auth.Actor{UserID: "a1", Level: auth.LevelAdmin}, GuildID: "g2"}
	regular  = Caller{Actor: auth.Actor{UserID: "u1", Level: auth.LevelUser}, GuildID: "g1"}
)

func newEnv(t *testing.T) *env {
	t.Helper()
	return openEnv(t, filepath.Join(t.TempDir(), "setup.db"), &fakeHA{states: map[entities.ID]entities.State{}}, new(time.Time))
}

// openEnv wires a service on the database at path; calling it again with the
// same path and fake simulates a process restart.
func openEnv(t *testing.T, path string, ha *fakeHA, clock *time.Time) *env {
	t.Helper()
	if clock.IsZero() {
		*clock = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	}
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := func() time.Time { return *clock }
	perms, aliases, auditLog := entities.NewPermissions(db), entities.NewAliases(db), audit.New(db)
	disc := entities.NewDiscovery(entities.DiscoveryDeps{HA: ha, DB: db, Permissions: perms, Now: now})
	return &env{
		svc:   NewService(Deps{DB: db, Store: NewStore(db), Discovery: disc, Permissions: perms, Aliases: aliases, Audit: auditLog, Now: now}),
		ent:   entities.NewService(entities.Deps{HA: ha, Permissions: perms, Aliases: aliases, Audit: auditLog, Now: now}),
		perms: perms, aliases: aliases, audit: auditLog, db: db, ha: ha, clock: clock, path: path,
	}
}

func (e *env) seed(ids ...string) {
	for _, id := range ids {
		e.ha.set(id, "Name of "+id, "on")
	}
}

// to walks a session through the wizard to the given step with the selection.
func (e *env) to(t *testing.T, c Caller, step Step, chosen ...entities.ID) Session {
	t.Helper()
	ctx := context.Background()
	sess, _, err := e.svc.Begin(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []Step{StepDiscover, StepPermissions, StepAliases, StepReview} {
		if sess.Step == step {
			return sess
		}
		if sess.Step == StepDiscover {
			if sess, err = e.svc.Select(ctx, c, sess.ID, sess.Version, chosen); err != nil {
				t.Fatalf("select: %v", err)
			}
		}
		if sess, err = e.svc.Navigate(ctx, c, sess.ID, sess.Version, EventNext); err != nil {
			t.Fatalf("navigate to %s: %v", next, err)
		}
	}
	return sess
}

// must unwraps a (Session, error) result, failing the test on error.
func must(t *testing.T) func(Session, error) Session {
	t.Helper()
	return func(sess Session, err error) Session {
		t.Helper()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return sess
	}
}

func (e *env) auditCount(t *testing.T) (n int) {
	t.Helper()
	if err := e.db.QueryRow("SELECT count(*) FROM command_audit_logs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// prepared returns a session at Review with a light (control), a switch (read)
// and a sensor (read, with an alias) ready to apply.
func (e *env) prepared(t *testing.T, c Caller) Session {
	t.Helper()
	ctx := context.Background()
	e.seed("light.kitchen", "switch.fan", "sensor.temp")
	sess := e.to(t, c, StepPermissions, "light.kitchen", "switch.fan", "sensor.temp")
	sess = must(t)(e.svc.Focus(ctx, c, sess.ID, sess.Version, "light.kitchen"))
	sess = must(t)(e.svc.SetAccess(ctx, c, sess.ID, sess.Version, entities.AccessControl))
	sess = must(t)(e.svc.Navigate(ctx, c, sess.ID, sess.Version, EventNext))
	sess = must(t)(e.svc.Focus(ctx, c, sess.ID, sess.Version, "light.kitchen"))
	sess = must(t)(e.svc.SetAlias(ctx, c, sess.ID, sess.Version, "  Kitchen   Light "))
	return must(t)(e.svc.Navigate(ctx, c, sess.ID, sess.Version, EventNext))
}

func TestBegin_createsThenResumes_andSessionsAreOnePerAdministrator(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	first, resumed1, err1 := e.svc.Begin(ctx, admin1)
	again, resumed2, err2 := e.svc.Begin(ctx, admin1)
	other, resumed3, err3 := e.svc.Begin(ctx, admin2)
	_, _, denied := e.svc.Begin(ctx, regular)

	if err1 != nil || err2 != nil || err3 != nil || resumed1 || !resumed2 || resumed3 {
		t.Fatalf("begin: %v %v %v resumed=%v/%v/%v", err1, err2, err3, resumed1, resumed2, resumed3)
	}
	if first.ID != again.ID || other.ID == first.ID || first.Step != StepHealth {
		t.Errorf("ids first=%s again=%s other=%s step=%s", first.ID, again.ID, other.ID, first.Step)
	}
	if !errors.Is(denied, auth.ErrAdminRequired) {
		t.Errorf("a regular user started a setup: %v", denied)
	}
}

func TestSessions_surviveARestart_andCanBeResumedWhereTheyStopped(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a", "light.b")
	sess := e.to(t, admin1, StepPermissions, "light.a", "light.b")
	sess = must(t)(e.svc.Page(context.Background(), admin1, sess.ID, sess.Version, 1))
	clock := *e.clock

	restarted := openEnv(t, e.path, e.ha, &clock)
	resumed, wasResumed, err := restarted.svc.Begin(context.Background(), admin1)

	if err != nil || !wasResumed || resumed.ID != sess.ID || resumed.Step != StepPermissions || len(resumed.Draft.Entries) != 2 {
		t.Errorf("resumed = %+v resumed=%v err=%v", resumed, wasResumed, err)
	}
}

func TestSessions_areInvisibleToOtherAdministratorsGuildsAndUsers_forEveryOperation(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a")
	sess := e.to(t, admin1, StepDiscover)
	ctx := context.Background()
	ops := map[string]func(c Caller) error{
		"get":      func(c Caller) error { _, err := e.svc.Get(ctx, c, sess.ID); return err },
		"navigate": func(c Caller) error { _, err := e.svc.Navigate(ctx, c, sess.ID, sess.Version, EventNext); return err },
		"select":   func(c Caller) error { _, err := e.svc.Select(ctx, c, sess.ID, sess.Version, nil); return err },
		"search":   func(c Caller) error { _, err := e.svc.Search(ctx, c, sess.ID, sess.Version, "x"); return err },
		"refresh":  func(c Caller) error { _, err := e.svc.Refresh(ctx, c, sess.ID, sess.Version); return err },
		"cancel":   func(c Caller) error { _, err := e.svc.Navigate(ctx, c, sess.ID, sess.Version, EventCancel); return err },
		"apply":    func(c Caller) error { _, err := e.svc.Apply(ctx, c, sess.ID, sess.Version); return err },
		"plan":     func(c Caller) error { _, err := e.svc.Plan(ctx, c, sess.ID); return err },
		"restart":  func(c Caller) error { _, err := e.svc.Restart(ctx, c, sess.ID); return err },
	}
	for name, op := range ops {
		if err := op(admin2); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s by another administrator: %v, want ErrNotFound", name, err)
		}
		if err := op(otherGld); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s from another guild: %v, want ErrNotFound", name, err)
		}
		if err := op(regular); !errors.Is(err, auth.ErrAdminRequired) {
			t.Errorf("%s by a regular user: %v, want ErrAdminRequired", name, err)
		}
	}
	if got, err := e.svc.Get(ctx, admin1, sess.ID); err != nil || got.Version != sess.Version || got.Status != StatusActive {
		t.Errorf("the owner's session was affected: %+v %v", got, err)
	}
}

func TestNavigate_leavingHealthRequiresLiveHomeAssistantData(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a")
	sess, _, _ := e.svc.Begin(context.Background(), admin1)
	e.ha.err = errors.New("connection refused")

	blocked, err := e.svc.Navigate(context.Background(), admin1, sess.ID, sess.Version, EventNext)
	e.ha.err = nil
	moved, okErr := e.svc.Navigate(context.Background(), admin1, sess.ID, sess.Version, EventNext)

	var haErr *HomeAssistantError
	if !errors.As(err, &haErr) || blocked.Step != StepHealth {
		t.Errorf("blocked = %+v err = %v, want HomeAssistantError and no progress", blocked, err)
	}
	if okErr != nil || moved.Step != StepDiscover || len(moved.Draft.View.Offered) != 1 {
		t.Errorf("after recovery: %+v %v", moved, okErr)
	}
}

func TestSelect_acceptsOnlyWhatTheServerOffered_andOfferedIsPersisted(t *testing.T) {
	e := newEnv(t)
	for i := range 25 {
		e.ha.set(fmt.Sprintf("light.l%02d", i), fmt.Sprintf("Lamp %02d", i), "off")
	}
	e.ha.set("lock.front_door", "Zz Front Door", "locked") // sorts last, so it is not on page 1
	sess := e.to(t, admin1, StepDiscover)
	ctx := context.Background()

	forged, forgedErr := e.svc.Select(ctx, admin1, sess.ID, sess.Version, []entities.ID{"lock.front_door"})
	offscreen, offErr := e.svc.Select(ctx, admin1, sess.ID, sess.Version, []entities.ID{"light.l24"}) // exists, but on page 3
	ghost, ghostErr := e.svc.Select(ctx, admin1, sess.ID, sess.Version, []entities.ID{"lock.does_not_exist"})
	ok, okErr := e.svc.Select(ctx, admin1, sess.ID, sess.Version, []entities.ID{"light.l00", "light.l09"})
	paged, pageErr := e.svc.Page(ctx, admin1, ok.ID, ok.Version, 2)
	reloaded, _ := e.svc.Get(ctx, admin1, sess.ID)

	if !errors.Is(forgedErr, ErrNotOffered) || forged.Version != sess.Version || !errors.Is(offErr, ErrNotOffered) || offscreen.Version != sess.Version || !errors.Is(ghostErr, ErrNotOffered) || ghost.Version != sess.Version {
		t.Errorf("forged=%v offscreen=%v ghost=%v: values outside the offered page must be rejected without any change", forgedErr, offErr, ghostErr)
	}
	if okErr != nil || len(ok.Draft.Entries) != 2 || pageErr != nil || paged.Draft.View.Offered[0] != "light.l10" {
		t.Errorf("ok=%v entries=%d page=%v offered=%v", okErr, len(ok.Draft.Entries), pageErr, paged.Draft.View.Offered)
	}
	if len(reloaded.Draft.Entries) != 2 || len(reloaded.Draft.View.Offered) != entities.PageSize {
		t.Errorf("the offered page and selection must be persisted: %+v", reloaded.Draft.View)
	}
}

func TestFullFlow_appliesAfterConfirmation_takesEffectImmediately_andIsAudited(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	ctx := context.Background()

	plan, planErr := e.svc.Plan(ctx, admin1, sess.ID)
	beforeAccess, _ := e.perms.Get(ctx, "light.kitchen")
	_, privateBefore := e.ent.Status(ctx, auth.Actor{UserID: "u1", Level: auth.LevelUser}, "light.kitchen")
	result, err := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)
	afterAccess, _ := e.perms.Get(ctx, "light.kitchen")
	user := auth.Actor{UserID: "u1", Level: auth.LevelUser}
	status, statusErr := e.ent.Status(ctx, user, "light.kitchen")
	ctrl, ctrlErr := e.ent.Control(ctx, user, "light.kitchen", entities.ActionOn)
	final, _ := e.svc.Get(ctx, admin1, sess.ID)

	if planErr != nil || plan.New != 3 || plan.AliasChanges != 1 || plan.Unchanged != 0 || len(plan.Problems()) != 0 {
		t.Errorf("plan = %+v (%v)", plan, planErr)
	}
	if beforeAccess != entities.AccessNone || !errors.Is(privateBefore, entities.ErrAccessDenied) {
		t.Errorf("nothing may be applied before confirmation: access=%q err=%v", beforeAccess, privateBefore)
	}
	if err != nil || result.New != 3 || afterAccess != entities.AccessControl {
		t.Fatalf("apply: %+v %v access=%q", result, err, afterAccess)
	}
	if statusErr != nil || status.Alias != "Kitchen Light" || ctrlErr != nil || ctrl.Before.ID != "light.kitchen" || len(e.ha.sent) != 1 {
		t.Errorf("immediate effect: status=%+v (%v) control=%v sent=%v", status, statusErr, ctrlErr, e.ha.sent)
	}
	if sw, _ := e.perms.Get(ctx, "switch.fan"); sw != entities.AccessRead {
		t.Errorf("switch default must be read-only, got %q", sw)
	}
	if final.Status != StatusCompleted || final.Step != StepDone {
		t.Errorf("session after apply: %s/%s", final.Status, final.Step)
	}
	entries, _ := e.audit.Recent(ctx, 50)
	counts := map[string]int{}
	for _, en := range entries {
		counts[en.Command]++
		if en.Outcome != audit.OutcomeSuccess || en.ActorID != "a1" && en.Command != "control" {
			t.Errorf("audit entry %+v", en)
		}
	}
	if counts["setup.allow"] != 3 || counts["setup.alias"] != 1 || counts["setup.apply"] != 1 {
		t.Errorf("audit counts = %v", counts)
	}
}

func TestApply_isIdempotent_aReplayedConfirmationWritesNothing(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	ctx := context.Background()
	if _, err := e.svc.Apply(ctx, admin1, sess.ID, sess.Version); err != nil {
		t.Fatal(err)
	}
	auditBefore := e.auditCount(t)
	// The administrator then changes one permission by hand.
	if _, err := e.perms.Revoke(ctx, "switch.fan"); err != nil {
		t.Fatal(err)
	}

	replay, err := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)
	replayNewer, errNewer := e.svc.Apply(ctx, admin1, sess.ID, sess.Version+1)

	if !errors.Is(err, ErrAlreadyApplied) || !errors.Is(errNewer, ErrAlreadyApplied) || replay.New != 0 || replayNewer.New != 0 {
		t.Errorf("replay = %+v %v / %v", replay, err, errNewer)
	}
	if sw, _ := e.perms.Get(ctx, "switch.fan"); sw != entities.AccessNone {
		t.Errorf("a replayed confirmation restored a revoked permission: %q", sw)
	}
	if e.auditCount(t) != auditBefore {
		t.Error("a replayed confirmation wrote audit entries")
	}
}

func TestApply_concurrentConfirmations_applyExactlyOnce(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	var ok, rejected atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch _, err := e.svc.Apply(context.Background(), admin1, sess.ID, sess.Version); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrAlreadyApplied), errors.Is(err, ErrStale), errors.Is(err, ErrNotActive):
				rejected.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok.Load() != 1 || rejected.Load() != 7 {
		t.Errorf("ok=%d rejected=%d, want exactly one application", ok.Load(), rejected.Load())
	}
	var applies int
	if err := e.db.QueryRow("SELECT count(*) FROM command_audit_logs WHERE command = 'setup.apply'").Scan(&applies); err != nil || applies != 1 {
		t.Errorf("setup.apply audit entries = %d (%v), want 1", applies, err)
	}
}

func TestApply_anOutdatedConfirmationCannotRestoreARevokedPermission(t *testing.T) {
	e := newEnv(t)
	e.seed("light.kitchen")
	ctx := context.Background()
	if err := e.perms.Set(ctx, entities.Grant{ID: "light.kitchen", Access: entities.AccessControl, GrantedBy: "a1", At: *e.clock}); err != nil {
		t.Fatal(err)
	}
	sess := e.to(t, admin1, StepAliases, "light.kitchen") // selected while shared (control)
	sess = must(t)(e.svc.Focus(ctx, admin1, sess.ID, sess.Version, "light.kitchen"))
	sess = must(t)(e.svc.SetAlias(ctx, admin1, sess.ID, sess.Version, "Kitchen"))
	sess = must(t)(e.svc.Navigate(ctx, admin1, sess.ID, sess.Version, EventNext))
	// Meanwhile someone revokes the permission with /permissions revoke.
	if _, err := e.perms.Revoke(ctx, "light.kitchen"); err != nil {
		t.Fatal(err)
	}

	_, err := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)
	_, replay := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)
	access, _ := e.perms.Get(ctx, "light.kitchen")
	alias, _ := e.aliases.Get(ctx, "light.kitchen")
	rebased, _ := e.svc.Get(ctx, admin1, sess.ID)
	plan, _ := e.svc.Plan(ctx, admin1, sess.ID)

	var stale *StaleConfigError
	if !errors.As(err, &stale) || len(stale.Entities) != 1 || stale.Entities[0] != "light.kitchen" {
		t.Fatalf("apply error = %v, want StaleConfigError for light.kitchen", err)
	}
	if access != entities.AccessNone || alias != "" {
		t.Errorf("the outdated confirmation wrote something: access=%q alias=%q", access, alias)
	}
	if !errors.Is(replay, ErrStale) || rebased.Version != sess.Version+1 || rebased.Status != StatusActive || rebased.Step != StepReview {
		t.Errorf("the old confirmation must be dead: replay=%v version %d -> %d status=%s step=%s", replay, sess.Version, rebased.Version, rebased.Status, rebased.Step)
	}
	if len(plan.Entries) != 1 || plan.Entries[0].Kind != KindNew || plan.Entries[0].Stale {
		t.Errorf("the review must now show an explicit re-grant: %+v", plan.Entries)
	}
	if _, err := e.svc.Apply(ctx, admin1, rebased.ID, rebased.Version); err != nil {
		t.Errorf("a fresh, explicit confirmation must apply: %v", err)
	}
	if access, _ := e.perms.Get(ctx, "light.kitchen"); access != entities.AccessControl {
		t.Errorf("after re-confirmation access = %q", access)
	}
}

func TestApply_externalChangeToTheSameValueIsNotAConflict(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	ctx := context.Background()
	if err := e.perms.Set(ctx, entities.Grant{ID: "light.kitchen", Access: entities.AccessControl, GrantedBy: "other", At: *e.clock}); err != nil {
		t.Fatal(err)
	}

	result, err := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)

	if err != nil || result.New != 2 || result.Modified != 1 || result.AliasChanges != 1 {
		t.Errorf("apply = %+v %v, want two new shares, the kitchen light's alias as a modification", result, err)
	}
}

func TestApply_rejectsTamperedDraftsAndVanishedEntities_withoutWritingAnything(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	ctx := context.Background()
	// Tamper with the persisted draft: control on a sensor (the state machine would never allow it).
	if _, err := e.db.Exec(`UPDATE setup_sessions SET state = replace(state, '"sensor.temp":{"desired":{"access":"read"', '"sensor.temp":{"desired":{"access":"control"') WHERE id = ?`, sess.ID); err != nil {
		t.Fatal(err)
	}
	e.ha.remove("switch.fan")

	_, err := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)

	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("apply error = %v, want ValidationError", err)
	}
	reasons := map[entities.ID]string{}
	for _, p := range validation.Problems {
		reasons[p.ID] = p.Reason
	}
	if reasons["sensor.temp"] == "" || reasons["switch.fan"] == "" || len(validation.Problems) != 2 {
		t.Errorf("problems = %+v", validation.Problems)
	}
	if perms, _ := e.perms.List(ctx); len(perms) != 0 {
		t.Errorf("a failed validation wrote permissions: %+v", perms)
	}
	if got, _ := e.svc.Get(ctx, admin1, sess.ID); got.Status != StatusActive || got.Version != sess.Version {
		t.Errorf("a failed apply must leave the session as it was: %+v", got)
	}
}

func TestApply_rollsBackEveryWrite_whenALaterWriteFails(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	if _, err := e.db.Exec("DROP TABLE command_audit_logs"); err != nil { // makes the first audit write fail after permissions were written
		t.Fatal(err)
	}

	_, err := e.svc.Apply(context.Background(), admin1, sess.ID, sess.Version)

	if err == nil {
		t.Fatal("apply succeeded although the audit table is gone")
	}
	perms, _ := e.perms.List(context.Background())
	aliases, _ := e.aliases.All(context.Background())
	got, _ := e.svc.Get(context.Background(), admin1, sess.ID)
	if len(perms) != 0 || len(aliases) != 0 || got.Status != StatusActive || got.Version != sess.Version {
		t.Errorf("partial write survived: perms=%+v aliases=%+v session=%s/v%d", perms, aliases, got.Status, got.Version)
	}
}

func TestApply_requiresHomeAssistant_andRejectsStaleVersionsAndWrongSteps(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	ctx := context.Background()
	early := e.to(t, admin2, StepPermissions, "light.kitchen")

	_, wrongStep := e.svc.Apply(ctx, admin2, early.ID, early.Version)
	_, oldVersion := e.svc.Apply(ctx, admin1, sess.ID, sess.Version-1)
	e.ha.err = errors.New("home assistant down")
	_, down := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)
	e.ha.err = nil

	var haErr *HomeAssistantError
	if !errors.Is(wrongStep, ErrWrongStep) || !errors.Is(oldVersion, ErrStale) || !errors.As(down, &haErr) {
		t.Errorf("wrongStep=%v oldVersion=%v down=%v", wrongStep, oldVersion, down)
	}
	if perms, _ := e.perms.List(ctx); len(perms) != 0 {
		t.Errorf("nothing may be written: %+v", perms)
	}
}

func TestCancel_appliesNothing_andEndsTheSession(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	ctx := context.Background()

	cancelled, err := e.svc.Navigate(ctx, admin1, sess.ID, sess.Version, EventCancel)
	_, afterApply := e.svc.Apply(ctx, admin1, sess.ID, cancelled.Version)
	_, afterNavigate := e.svc.Navigate(ctx, admin1, sess.ID, cancelled.Version, EventBack)
	fresh, resumed, beginErr := e.svc.Begin(ctx, admin1)

	if err != nil || cancelled.Status != StatusCancelled {
		t.Fatalf("cancel: %+v %v", cancelled, err)
	}
	if !errors.Is(afterApply, ErrNotActive) || !errors.Is(afterNavigate, ErrNotActive) {
		t.Errorf("after cancel: apply=%v navigate=%v", afterApply, afterNavigate)
	}
	if perms, _ := e.perms.List(ctx); len(perms) != 0 {
		t.Errorf("cancel applied pending changes: %+v", perms)
	}
	if beginErr != nil || resumed || fresh.ID == sess.ID || fresh.Step != StepHealth {
		t.Errorf("begin after cancel: %+v resumed=%v err=%v", fresh, resumed, beginErr)
	}
}

func TestRestart_cancelsTheOldSession_andStartsFresh(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)

	fresh, err := e.svc.Restart(context.Background(), admin1, sess.ID)
	old, _ := e.svc.Get(context.Background(), admin1, sess.ID)

	if err != nil || fresh.ID == sess.ID || fresh.Step != StepHealth || len(fresh.Draft.Entries) != 0 || old.Status != StatusCancelled {
		t.Errorf("fresh=%+v err=%v old=%s", fresh, err, old.Status)
	}
}

func TestSessions_expireWhenIdle_andAnExpiredSessionNeverApplies(t *testing.T) {
	e := newEnv(t)
	sess := e.prepared(t, admin1)
	ctx := context.Background()
	*e.clock = e.clock.Add(SessionTTL + time.Minute)

	_, getErr := e.svc.Get(ctx, admin1, sess.ID)
	_, applyErr := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)
	stored, _ := e.svc.Store().Get(ctx, sess.ID)
	fresh, resumed, err := e.svc.Begin(ctx, admin1)

	if !errors.Is(getErr, ErrExpired) || !errors.Is(applyErr, ErrNotActive) && !errors.Is(applyErr, ErrExpired) {
		t.Errorf("get=%v apply=%v", getErr, applyErr)
	}
	if stored.Status != StatusExpired || perms(e) != 0 {
		t.Errorf("status=%s perms=%d", stored.Status, perms(e))
	}
	if err != nil || resumed || fresh.ID == sess.ID {
		t.Errorf("begin after expiry: %+v resumed=%v err=%v", fresh, resumed, err)
	}
}

func perms(e *env) int {
	list, _ := e.perms.List(context.Background())
	return len(list)
}

func TestAliases_duplicatesAreCaughtImmediately_andAtApplyTime(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a", "light.b")
	ctx := context.Background()
	if err := e.aliases.Set(ctx, entities.AliasSet{ID: "light.taken", Alias: "Lounge", By: "a1", At: *e.clock}); err != nil {
		t.Fatal(err)
	}
	sess := e.to(t, admin1, StepAliases, "light.a", "light.b")
	sess = must(t)(e.svc.Focus(ctx, admin1, sess.ID, sess.Version, "light.a"))

	_, immediate := e.svc.SetAlias(ctx, admin1, sess.ID, sess.Version, "lounge")
	sess = must(t)(e.svc.SetAlias(ctx, admin1, sess.ID, sess.Version, "Reading Lamp"))
	sess = must(t)(e.svc.Navigate(ctx, admin1, sess.ID, sess.Version, EventNext))
	// Someone else takes that alias before the confirmation.
	if err := e.aliases.Set(ctx, entities.AliasSet{ID: "light.sneaky", Alias: "READING lamp", By: "a2", At: *e.clock}); err != nil {
		t.Fatal(err)
	}
	_, atApply := e.svc.Apply(ctx, admin1, sess.ID, sess.Version)

	var taken *entities.AliasTakenError
	var validation *ValidationError
	if !errors.As(immediate, &taken) || taken.Holder != "light.taken" {
		t.Errorf("immediate duplicate = %v", immediate)
	}
	if !errors.As(atApply, &validation) || perms(e) != 0 {
		t.Errorf("apply-time duplicate = %v perms=%d", atApply, perms(e))
	}
}

func TestDiscover_annotatesSharingAndSelection_filtersSharedAndClampsPages(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a", "light.b", "sensor.t")
	ctx := context.Background()
	_ = e.perms.Set(ctx, entities.Grant{ID: "light.a", Access: entities.AccessControl, GrantedBy: "x", At: *e.clock})
	_ = e.aliases.Set(ctx, entities.AliasSet{ID: "light.a", Alias: "Alpha", By: "x", At: *e.clock})
	sess := e.to(t, admin1, StepDiscover)
	sess = must(t)(e.svc.Select(ctx, admin1, sess.ID, sess.Version, []entities.ID{"light.b"}))

	all, err := e.svc.Discover(ctx, sess)
	sess = must(t)(e.svc.FilterShared(ctx, admin1, sess.ID, sess.Version, true))
	shared, _ := e.svc.Discover(ctx, sess)
	sess = must(t)(e.svc.Page(ctx, admin1, sess.ID, sess.Version, 99))
	clamped, _ := e.svc.Discover(ctx, sess)

	if err != nil || all.Matches != 3 || all.Selected != 1 || len(all.Items) != 3 {
		t.Fatalf("all = %+v %v", all, err)
	}
	byID := map[entities.ID]DiscoverItem{}
	for _, it := range all.Items {
		byID[it.ID] = it
	}
	if byID["light.a"].Access != entities.AccessControl || byID["light.a"].Alias != "Alpha" || byID["light.b"].Access != entities.AccessNone || !byID["light.b"].Selected {
		t.Errorf("annotations = %+v", byID)
	}
	if shared.Matches != 1 || shared.Items[0].ID != "light.a" || clamped.Page != 1 {
		t.Errorf("shared=%+v clamped page=%d", shared, clamped.Page)
	}
}

func TestRefresh_forcesAFreshFetch(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a")
	sess := e.to(t, admin1, StepDiscover)
	before := e.ha.calls
	e.ha.set("light.b", "Brand New", "off")

	refreshed, err := e.svc.Refresh(context.Background(), admin1, sess.ID, sess.Version)
	view, _ := e.svc.Discover(context.Background(), refreshed)

	if err != nil || e.ha.calls <= before || view.Matches != 2 {
		t.Errorf("refresh: err=%v calls %d -> %d matches=%d", err, before, e.ha.calls, view.Matches)
	}
}

func TestStaleVersions_arePassedBackSoTheCallerCanRerender(t *testing.T) {
	e := newEnv(t)
	e.seed("light.a")
	sess := e.to(t, admin1, StepDiscover)
	ctx := context.Background()
	newer := must(t)(e.svc.Search(ctx, admin1, sess.ID, sess.Version, "light"))

	current, err := e.svc.Search(ctx, admin1, sess.ID, sess.Version, "other")

	if !errors.Is(err, ErrStale) || current.Version != newer.Version || current.Draft.View.Query != "light" {
		t.Errorf("stale click: %v current=%+v", err, current.Draft.View)
	}
}
