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
