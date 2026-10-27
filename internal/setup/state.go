// Package setup is the Discord-free core of the interactive setup wizard: the
// session model, the explicit state machine, the SQLite store and the service
// that plans and atomically applies a confirmed configuration.
package setup

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

// Step is one stage of the wizard.
type Step string

const (
	StepHealth      Step = "health"
	StepDiscover    Step = "discover"
	StepPermissions Step = "permissions"
	StepAliases     Step = "aliases"
	StepReview      Step = "review"
	StepDone        Step = "done"
)

// Number is the 1-based position shown as "Step N of 6".
func (s Step) Number() int {
	return slices.Index([]Step{StepHealth, StepDiscover, StepPermissions, StepAliases, StepReview, StepDone}, s) + 1
}

// Status is the lifecycle state of a session.
type Status string

const (
	StatusActive    Status = "active"
	StatusCompleted Status = "completed"
	StatusCancelled Status = "cancelled"
	StatusExpired   Status = "expired"
)

// Event is a navigation request.
type Event string

const (
	EventNext    Event = "next"
	EventBack    Event = "back"
	EventConfirm Event = "confirm"
	EventCancel  Event = "cancel"
)

// transitions is the complete state machine. Cancel is handled separately
// because it is allowed from every active step.
var transitions = map[Step]map[Event]Step{
	StepHealth:      {EventNext: StepDiscover},
	StepDiscover:    {EventBack: StepHealth, EventNext: StepPermissions},
	StepPermissions: {EventBack: StepDiscover, EventNext: StepAliases},
	StepAliases:     {EventBack: StepPermissions, EventNext: StepReview},
	StepReview:      {EventBack: StepAliases, EventConfirm: StepDone},
}

const (
	// MaxSelected bounds a draft so sessions and embeds stay small.
	MaxSelected = 100
	// SessionTTL is how long an idle session stays resumable.
	SessionTTL  = 24 * time.Hour
	maxQueryLen = 64
)

var (
	// ErrNotActive means the session is completed, cancelled or expired.
	ErrNotActive = errors.New("this setup session is no longer active")
	// ErrInvalidTransition means the event is not available at the current step.
	ErrInvalidTransition = errors.New("that action is not available at this step")
	// ErrWrongStep means the operation belongs to a different step.
	ErrWrongStep = errors.New("that change is not available at this step")
	// ErrNothingSelected blocks leaving discovery with an empty selection.
	ErrNothingSelected = errors.New("select at least one entity first")
	// ErrTooManySelected means the draft would exceed MaxSelected.
	ErrTooManySelected = errors.New("too many entities selected for one session")
	// ErrNotSelected means the entity is not part of the draft.
	ErrNotSelected = errors.New("that entity is not selected in this session")
	// ErrNotOffered means a submitted value was not among the options the server offered.
	ErrNotOffered = errors.New("that entity was not offered")
	// ErrNothingToApply means the draft contains no change.
	ErrNothingToApply = errors.New("there is nothing to apply: the draft matches the current configuration")
)

// Base is an entity's configuration: its access level and alias.
type Base struct {
	Access entities.Access `json:"access"`
	Alias  string          `json:"alias"`
}

// Entry is the draft for one entity: the desired configuration and the base it
// was derived from. Comparing the base with the live configuration at apply
// time is what detects changes made outside the session.
type Entry struct {
	Desired Base `json:"desired"`
	Base    Base `json:"base"`
}

// Changed reports whether the draft differs from its base.
func (e Entry) Changed() bool { return e.Desired != e.Base }

// View is the persisted browsing state, so a resumed session lands where it stopped.
type View struct {
	Query  string `json:"query"`
	Domain string `json:"domain"`
	Shared bool   `json:"shared"`
	Page   int    `json:"page"`
	Focus  string `json:"focus"`
	// Offered are the exact entity IDs the server put in the current discovery
	// select menu. A submitted selection must be a subset of them.
	Offered []entities.ID `json:"offered"`
}

// Draft is everything a session holds. It never contains credentials.
type Draft struct {
	Entries map[entities.ID]Entry `json:"entries"`
	View    View                  `json:"view"`
}

// Session is one persisted wizard run.
type Session struct {
	ID        string
	GuildID   string
	AdminID   string
	Step      Step
	Status    Status
	Version   int
	Draft     Draft
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
}

func newSession(id, guildID, adminID string, now time.Time) Session {
	return Session{
		ID: id, GuildID: guildID, AdminID: adminID, Step: StepHealth, Status: StatusActive, Version: 1,
		Draft:     Draft{Entries: map[entities.ID]Entry{}, View: View{Page: 1}},
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(SessionTTL),
	}
}

// Expired reports whether an active session has been idle past its expiry.
func (s Session) Expired(now time.Time) bool {
	return s.Status == StatusActive && !now.Before(s.ExpiresAt)
}

// Touch records activity and extends the expiry.
func (s *Session) Touch(now time.Time) {
	s.UpdatedAt = now
	s.ExpiresAt = now.Add(SessionTTL)
}

// Selected returns the draft's entity IDs in order.
func (s Session) Selected() []entities.ID {
	ids := make([]entities.ID, 0, len(s.Draft.Entries))
	for id := range s.Draft.Entries {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b entities.ID) int { return strings.Compare(string(a), string(b)) })
	return ids
}

// HasChanges reports whether any entry differs from its base.
func (s Session) HasChanges() bool {
	for _, e := range s.Draft.Entries {
		if e.Changed() {
			return true
		}
	}
	return false
}

// Transition applies a navigation event. It is the only place steps change.
func (s *Session) Transition(ev Event) error {
	if s.Status != StatusActive {
		return ErrNotActive
	}
	if ev == EventCancel {
		s.Status = StatusCancelled
		return nil
	}
	next, ok := transitions[s.Step][ev]
	if !ok {
		return ErrInvalidTransition
	}
	if err := s.guard(ev); err != nil {
		return err
	}
	s.Step = next
	s.Draft.View.Page, s.Draft.View.Focus = 1, ""
	if ev == EventConfirm {
		s.Status = StatusCompleted
	}
	return nil
}

func (s *Session) guard(ev Event) error {
	switch {
	case s.Step == StepDiscover && ev == EventNext && len(s.Draft.Entries) == 0:
		return ErrNothingSelected
	case s.Step == StepAliases && ev == EventNext:
		return s.checkDraftAliases()
	case s.Step == StepReview && ev == EventConfirm && !s.HasChanges():
		return ErrNothingToApply
	}
	return nil
}

func (s *Session) checkDraftAliases() error {
	seen := map[string]entities.ID{}
	for _, id := range s.Selected() {
		alias := s.Draft.Entries[id].Desired.Alias
		if alias == "" {
			continue
		}
		_, key, err := entities.NormalizeAlias(alias)
		if err != nil {
			return err
		}
		if holder, dup := seen[key]; dup {
			return &entities.AliasTakenError{Alias: alias, Holder: holder}
		}
		seen[key] = id
	}
	return nil
}

func (s *Session) requireStep(step Step) error {
	if s.Status != StatusActive {
		return ErrNotActive
	}
	if s.Step != step {
		return ErrWrongStep
	}
	return nil
}

// SetSelection replaces the selection for the currently offered page of
// discovery results. chosen must be a subset of View.Offered (a forged value is
// rejected). Newly selected entities default to read access, or to their
// current access when already shared.
func (s *Session) SetSelection(chosen []entities.ID, base func(entities.ID) Base) error {
	if err := s.requireStep(StepDiscover); err != nil {
		return err
	}
	page := s.Draft.View.Offered
	for _, id := range chosen {
		if !slices.Contains(page, id) {
			return ErrNotOffered
		}
	}
	next := make(map[entities.ID]Entry, len(s.Draft.Entries))
	for id, e := range s.Draft.Entries {
		if !slices.Contains(page, id) {
			next[id] = e
		}
	}
	for _, id := range chosen {
		if e, ok := s.Draft.Entries[id]; ok {
			next[id] = e
			continue
		}
		b := base(id)
		desired := b
		if desired.Access == entities.AccessNone {
			desired.Access = entities.AccessRead
		}
		next[id] = Entry{Desired: desired, Base: b}
	}
	if len(next) > MaxSelected {
		return ErrTooManySelected
	}
	s.Draft.Entries = next
	return nil
}

// SetAccess sets the desired access of a selected entity (AccessNone revokes).
func (s *Session) SetAccess(id entities.ID, access entities.Access) error {
	if err := s.requireStep(StepPermissions); err != nil {
		return err
	}
	e, ok := s.Draft.Entries[id]
	if !ok {
		return ErrNotSelected
	}
	if err := entities.CheckGrant(id, access); err != nil {
		return err
	}
	e.Desired.Access = access
	s.Draft.Entries[id] = e
	return nil
}

// BulkRule is a one-click access rule for every selected entity.
type BulkRule string

const (
	// BulkReadAll gives read access to every selected entity.
	BulkReadAll BulkRule = "read"
	// BulkControlSupported gives control to lights and switches and read to the rest.
	BulkControlSupported BulkRule = "control"
	// BulkClearAll removes access from every selected entity.
	BulkClearAll BulkRule = "none"
)

// SetAccessBulk applies a rule to every selected entity and returns how many entries changed.
func (s *Session) SetAccessBulk(rule BulkRule) (int, error) {
	if err := s.requireStep(StepPermissions); err != nil {
		return 0, err
	}
	changed := 0
	for id, e := range s.Draft.Entries {
		want := entities.AccessRead
		switch rule {
		case BulkControlSupported:
			if id.Domain().Controllable() {
				want = entities.AccessControl
			}
		case BulkClearAll:
			want = entities.AccessNone
		case BulkReadAll:
		}
		if e.Desired.Access != want {
			e.Desired.Access = want
			s.Draft.Entries[id] = e
			changed++
		}
	}
	return changed, nil
}

// SetAlias sets the desired alias of a selected entity; "" removes it. A
// duplicate inside the draft is rejected, naming the other entity.
func (s *Session) SetAlias(id entities.ID, raw string) error {
	if err := s.requireStep(StepAliases); err != nil {
		return err
	}
	e, ok := s.Draft.Entries[id]
	if !ok {
		return ErrNotSelected
	}
	display := ""
	if strings.TrimSpace(raw) != "" {
		var key string
		var err error
		if display, key, err = entities.NormalizeAlias(raw); err != nil {
			return err
		}
		for other, oe := range s.Draft.Entries {
			if _, otherKey, err := entities.NormalizeAlias(oe.Desired.Alias); other != id && oe.Desired.Alias != "" && err == nil && otherKey == key {
				return &entities.AliasTakenError{Alias: display, Holder: other}
			}
		}
	}
	e.Desired.Alias = display
	s.Draft.Entries[id] = e
	return nil
}

// SetFocus chooses the entity the next per-entity action applies to.
func (s *Session) SetFocus(id entities.ID) error {
	if s.Status != StatusActive {
		return ErrNotActive
	}
	if s.Step != StepPermissions && s.Step != StepAliases {
		return ErrWrongStep
	}
	if _, ok := s.Draft.Entries[id]; !ok {
		return ErrNotSelected
	}
	s.Draft.View.Focus = string(id)
	return nil
}

// SetQuery sets the discovery search text ("" clears it) and returns to page 1.
func (s *Session) SetQuery(q string) error {
	if err := s.requireStep(StepDiscover); err != nil {
		return err
	}
	q = strings.Join(strings.Fields(q), " ")
	if r := []rune(q); len(r) > maxQueryLen {
		q = string(r[:maxQueryLen])
	}
	s.Draft.View.Query, s.Draft.View.Page = q, 1
	return nil
}

// SetDomain filters discovery by domain ("" for all) and returns to page 1.
func (s *Session) SetDomain(d entities.Domain) error {
	if err := s.requireStep(StepDiscover); err != nil {
		return err
	}
	s.Draft.View.Domain, s.Draft.View.Page = string(d), 1
	return nil
}

// SetShared restricts discovery to currently shared entities and returns to page 1.
func (s *Session) SetShared(only bool) error {
	if err := s.requireStep(StepDiscover); err != nil {
		return err
	}
	s.Draft.View.Shared, s.Draft.View.Page = only, 1
	return nil
}

// SetPage moves to a page (clamped by the caller when rendering).
func (s *Session) SetPage(page int) error {
	if s.Status != StatusActive {
		return ErrNotActive
	}
	s.Draft.View.Page = max(page, 1)
	return nil
}

// SetOffered records the IDs offered in the current discovery select menu.
func (s *Session) SetOffered(ids []entities.ID) { s.Draft.View.Offered = slices.Clone(ids) }

// Clone returns a deep copy, so a failed operation can be discarded.
func (s Session) Clone() Session {
	c := s
	c.Draft.Entries = make(map[entities.ID]Entry, len(s.Draft.Entries))
	for id, e := range s.Draft.Entries {
		c.Draft.Entries[id] = e
	}
	c.Draft.View.Offered = slices.Clone(s.Draft.View.Offered)
	return c
}
