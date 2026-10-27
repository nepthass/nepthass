package setup

import (
	"slices"
	"strings"

	"github.com/nepthass/nepthass/internal/entities"
)

// Draft editing operations. Each one belongs to a single step and is rejected
// elsewhere, so the UI cannot reach a change through the wrong screen.

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
