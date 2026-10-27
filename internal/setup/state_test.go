package setup

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func fresh() Session { return newSession("sid", "g1", "a1", t0) }

func noBase(entities.ID) Base { return Base{} }

// selectOne offers and selects light.a on a session at the discovery step.
func selectOne(s *Session) error {
	s.SetOffered([]entities.ID{"light.a"})
	return s.SetSelection([]entities.ID{"light.a"}, noBase)
}

// walkTo moves a fresh session with one selected light to the given step.
func walkTo(t *testing.T, step Step) Session {
	t.Helper()
	s := fresh()
	order := []Step{StepHealth, StepDiscover, StepPermissions, StepAliases, StepReview}
	for _, cur := range order {
		if cur == step {
			return s
		}
		if cur == StepDiscover {
			if err := selectOne(&s); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Transition(EventNext); err != nil {
			t.Fatalf("Next from %s: %v", cur, err)
		}
	}
	t.Fatalf("unknown step %s", step)
	return s
}

func TestTransition_followsTheDocumentedPath_andBackReturnsToThePreviousStep(t *testing.T) {
	s := fresh()
	if err := s.SetPage(3); err != nil {
		t.Fatal(err)
	}
	forward := []Step{StepDiscover, StepPermissions, StepAliases, StepReview}
	for i, want := range forward {
		if i == 1 {
			if err := selectOne(&s); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Transition(EventNext); err != nil || s.Step != want {
			t.Fatalf("Next -> %s (%v), want %s", s.Step, err, want)
		}
	}
	for _, want := range []Step{StepAliases, StepPermissions, StepDiscover, StepHealth} {
		if err := s.Transition(EventBack); err != nil || s.Step != want {
			t.Fatalf("Back -> %s (%v), want %s", s.Step, err, want)
		}
	}
	if s.Draft.View.Page != 1 || len(s.Draft.Entries) != 1 {
		t.Errorf("navigation must reset the page and keep the draft: %+v", s.Draft)
	}
}

func TestTransition_rejectsEveryEventThatIsNotInTheTable(t *testing.T) {
	valid := map[Step]map[Event]bool{
		StepHealth:      {EventNext: true, EventCancel: true},
		StepDiscover:    {EventNext: true, EventBack: true, EventCancel: true},
		StepPermissions: {EventNext: true, EventBack: true, EventCancel: true},
		StepAliases:     {EventNext: true, EventBack: true, EventCancel: true},
		StepReview:      {EventBack: true, EventConfirm: true, EventCancel: true},
	}
	for _, step := range []Step{StepHealth, StepDiscover, StepPermissions, StepAliases, StepReview} {
		for _, ev := range []Event{EventNext, EventBack, EventConfirm, EventCancel, "bogus"} {
			s := walkTo(t, step)
			// Give the guards something to accept so they do not mask the table.
			switch step {
			case StepDiscover:
				_ = selectOne(&s)
			case StepReview:
				s.Draft.Entries["light.a"] = Entry{Desired: Base{Access: entities.AccessControl}}
			}

			err := s.Transition(ev)

			if valid[step][ev] && err != nil {
				t.Errorf("%s + %s: unexpected error %v", step, ev, err)
			}
			if !valid[step][ev] && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s + %s: error = %v, want ErrInvalidTransition", step, ev, err)
			}
		}
	}
}

func TestTransition_guards(t *testing.T) {
	empty := fresh()
	_ = empty.Transition(EventNext)
	noChange := walkTo(t, StepReview) // light.a selected, default read vs base none => changed; reset to unchanged
	noChange.Draft.Entries["light.a"] = Entry{Desired: Base{Access: entities.AccessRead}, Base: Base{Access: entities.AccessRead}}
	dup := walkTo(t, StepAliases)
	dup.Draft.Entries["light.b"] = Entry{}
	dup.Draft.Entries["light.a"] = Entry{Desired: Base{Access: entities.AccessRead, Alias: "Lamp"}}
	dup.Draft.Entries["light.b"] = Entry{Desired: Base{Access: entities.AccessRead, Alias: "lamp"}}

	if err := empty.Transition(EventNext); !errors.Is(err, ErrNothingSelected) {
		t.Errorf("leaving discovery with nothing selected: %v", err)
	}
	if err := noChange.Transition(EventConfirm); !errors.Is(err, ErrNothingToApply) {
		t.Errorf("confirming an unchanged draft: %v", err)
	}
	if err := dup.Transition(EventNext); !errors.Is(err, entities.ErrAliasTaken) {
		t.Errorf("duplicate aliases inside the draft: %v", err)
	}
}

func TestCancel_isAllowedFromEveryActiveStep_andFreezesTheSession(t *testing.T) {
	for _, step := range []Step{StepHealth, StepDiscover, StepPermissions, StepAliases, StepReview} {
		s := walkTo(t, step)

		if err := s.Transition(EventCancel); err != nil || s.Status != StatusCancelled {
			t.Fatalf("cancel at %s: %v status=%s", step, err, s.Status)
		}
		for _, ev := range []Event{EventNext, EventBack, EventConfirm, EventCancel} {
			if err := s.Transition(ev); !errors.Is(err, ErrNotActive) {
				t.Errorf("%s after cancel at %s: error = %v, want ErrNotActive", ev, step, err)
			}
		}
		if err := s.SetPage(2); !errors.Is(err, ErrNotActive) {
			t.Errorf("SetPage after cancel: %v", err)
		}
	}
}

func TestConfirm_completesTheSession(t *testing.T) {
	s := walkTo(t, StepReview)

	err := s.Transition(EventConfirm)

	if err != nil || s.Step != StepDone || s.Status != StatusCompleted {
		t.Errorf("confirm: %v step=%s status=%s", err, s.Step, s.Status)
	}
	if err := s.Transition(EventConfirm); !errors.Is(err, ErrNotActive) {
		t.Errorf("second confirm: %v", err)
	}
}

func TestOperations_belongToTheirStep(t *testing.T) {
	ids := []entities.ID{"light.a"}
	ops := map[string]struct {
		step Step
		run  func(s *Session) error
	}{
		"selection": {StepDiscover, func(s *Session) error { s.SetOffered(ids); return s.SetSelection(ids, noBase) }},
		"access":    {StepPermissions, func(s *Session) error { return s.SetAccess("light.a", entities.AccessRead) }},
		"bulk":      {StepPermissions, func(s *Session) error { _, err := s.SetAccessBulk(BulkReadAll); return err }},
		"alias":     {StepAliases, func(s *Session) error { return s.SetAlias("light.a", "Lamp") }},
		"query":     {StepDiscover, func(s *Session) error { return s.SetQuery("x") }},
		"domain":    {StepDiscover, func(s *Session) error { return s.SetDomain("light") }},
		"shared":    {StepDiscover, func(s *Session) error { return s.SetShared(true) }},
	}
	for name, op := range ops {
		for _, step := range []Step{StepHealth, StepDiscover, StepPermissions, StepAliases, StepReview} {
			s := walkTo(t, step)

			err := op.run(&s)

			if step != op.step && !errors.Is(err, ErrWrongStep) {
				t.Errorf("%s at %s: error = %v, want ErrWrongStep", name, step, err)
			}
			if step == op.step && errors.Is(err, ErrWrongStep) {
				t.Errorf("%s at its own step %s was refused", name, step)
			}
		}
	}
}

func TestSetSelection_defaultsToLeastPrivilege_keepsExistingAccess_andRejectsForgedValues(t *testing.T) {
	s := fresh()
	_ = s.Transition(EventNext)
	base := func(id entities.ID) Base {
		if id == "light.shared" {
			return Base{Access: entities.AccessControl, Alias: "Shared"}
		}
		return Base{}
	}
	page := []entities.ID{"light.new", "light.shared", "sensor.x"}
	s.SetOffered(page)

	if err := s.SetSelection([]entities.ID{"light.new", "light.shared"}, base); err != nil {
		t.Fatal(err)
	}
	if got := s.Draft.Entries["light.new"]; got.Desired.Access != entities.AccessRead || got.Base.Access != entities.AccessNone {
		t.Errorf("new entity = %+v, want read (never control) by default", got)
	}
	if got := s.Draft.Entries["light.shared"]; got.Desired != got.Base || got.Desired.Access != entities.AccessControl || got.Changed() {
		t.Errorf("already shared entity = %+v, want unchanged", got)
	}
	forged := s.SetSelection([]entities.ID{"lock.front_door"}, base)
	deselect := s.SetSelection(nil, base)

	if !errors.Is(forged, ErrNotOffered) {
		t.Errorf("a value outside the offered page: %v", forged)
	}
	if deselect != nil || len(s.Draft.Entries) != 0 {
		t.Errorf("deselecting the page: %v entries=%d", deselect, len(s.Draft.Entries))
	}
}

func TestSetSelection_keepsSelectionsFromOtherPages_andEnforcesTheLimit(t *testing.T) {
	s := fresh()
	_ = s.Transition(EventNext)
	s.SetOffered([]entities.ID{"light.p1"})
	_ = s.SetSelection([]entities.ID{"light.p1"}, noBase)

	s.SetOffered([]entities.ID{"light.p2"})
	if err := s.SetSelection([]entities.ID{"light.p2"}, noBase); err != nil || len(s.Draft.Entries) != 2 {
		t.Fatalf("second page replaced the first: %v %v", err, s.Draft.Entries)
	}
	big := fresh()
	_ = big.Transition(EventNext)
	var page []entities.ID
	for i := range MaxSelected + 1 {
		page = append(page, entities.ID("light.l"+string(rune('a'+i%26))+string(rune('a'+i/26))))
	}
	big.SetOffered(page)
	if err := big.SetSelection(page, noBase); !errors.Is(err, ErrTooManySelected) {
		t.Errorf("selecting %d entities: %v", len(page), err)
	}
	if len(big.Draft.Entries) != 0 {
		t.Error("a rejected selection must not be partially applied")
	}
}

func TestSetAccess_enforcesTheSharedGrantRule_andBulkRulesRespectDomains(t *testing.T) {
	s := walkTo(t, StepDiscover)
	s.SetOffered([]entities.ID{"light.a", "sensor.t", "lock.door"})
	_ = s.SetSelection([]entities.ID{"light.a", "sensor.t", "lock.door"}, noBase)
	_ = s.Transition(EventNext)

	lockControl := s.SetAccess("lock.door", entities.AccessControl)
	sensorControl := s.SetAccess("sensor.t", entities.AccessControl)
	lightControl := s.SetAccess("light.a", entities.AccessControl)
	unknown := s.SetAccess("light.zzz", entities.AccessRead)
	changed, bulkErr := s.SetAccessBulk(BulkControlSupported)

	if !errors.Is(lockControl, entities.ErrNotControllable) || !errors.Is(sensorControl, entities.ErrNotControllable) {
		t.Errorf("control on lock/sensor: %v / %v", lockControl, sensorControl)
	}
	if lightControl != nil || !errors.Is(unknown, ErrNotSelected) || bulkErr != nil {
		t.Errorf("light=%v unknown=%v bulk=%v", lightControl, unknown, bulkErr)
	}
	for id, want := range map[entities.ID]entities.Access{"light.a": entities.AccessControl, "sensor.t": entities.AccessRead, "lock.door": entities.AccessRead} {
		if got := s.Draft.Entries[id].Desired.Access; got != want {
			t.Errorf("%s = %q after bulk control rule, want %q", id, got, want)
		}
	}
	if changed != 0 {
		t.Errorf("bulk changed %d entries, want 0 (already at the rule's values)", changed)
	}
	if n, _ := s.SetAccessBulk(BulkClearAll); n != 3 || s.Draft.Entries["light.a"].Desired.Access != entities.AccessNone {
		t.Errorf("clear-all changed %d", n)
	}
}

func TestSetAlias_validatesNormalizesAndRejectsDraftDuplicates(t *testing.T) {
	s := walkTo(t, StepDiscover)
	s.SetOffered([]entities.ID{"light.a", "light.b"})
	_ = s.SetSelection([]entities.ID{"light.a", "light.b"}, noBase)
	_ = s.Transition(EventNext)
	_ = s.Transition(EventNext)

	set := s.SetAlias("light.a", "  Living   Room ")
	dup := s.SetAlias("light.b", "living room")
	bad := s.SetAlias("light.b", "<@everyone>")
	cleared := s.SetAlias("light.a", "   ")
	unknown := s.SetAlias("light.zzz", "Lamp")

	if set != nil || !errors.Is(dup, entities.ErrAliasTaken) || !errors.Is(bad, entities.ErrInvalidAlias) || cleared != nil || !errors.Is(unknown, ErrNotSelected) {
		t.Errorf("set=%v dup=%v bad=%v cleared=%v unknown=%v", set, dup, bad, cleared, unknown)
	}
	if s.Draft.Entries["light.a"].Desired.Alias != "" || s.Draft.Entries["light.b"].Desired.Alias != "" {
		t.Errorf("aliases after clear/reject: %+v", s.Draft.Entries)
	}
	_ = s.SetAlias("light.a", "  Living   Room ")
	if got := s.Draft.Entries["light.a"].Desired.Alias; got != "Living Room" {
		t.Errorf("alias stored as %q, want the normalized display form", got)
	}
}

func TestSession_expiresWhenIdle_andTouchExtendsIt(t *testing.T) {
	s := fresh()

	before := s.Expired(t0.Add(SessionTTL - time.Second))
	at := s.Expired(t0.Add(SessionTTL))
	s.Touch(t0.Add(time.Hour))
	afterTouch := s.Expired(t0.Add(SessionTTL))
	s.Status = StatusCancelled

	if before || !at || afterTouch || s.Expired(t0.Add(48*time.Hour)) {
		t.Errorf("expiry before=%v at=%v afterTouch=%v", before, at, afterTouch)
	}
}

func TestDraft_roundTripsThroughJSON_withoutCredentials(t *testing.T) {
	s := walkTo(t, StepAliases)
	_ = s.SetAlias("light.a", "Lamp")

	raw, err := json.Marshal(s.Draft)
	var back Draft
	decodeErr := json.Unmarshal(raw, &back)

	if err != nil || decodeErr != nil || back.Entries["light.a"].Desired.Alias != "Lamp" {
		t.Fatalf("round trip: %v %v %+v", err, decodeErr, back)
	}
	for _, forbidden := range []string{"token", "password", "secret"} {
		if contains(string(raw), forbidden) {
			t.Errorf("the persisted draft mentions %q: %s", forbidden, raw)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
