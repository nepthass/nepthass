package setup

import (
	"context"

	"github.com/nepthass/nepthass/internal/entities"
)

// filter builds the discovery filter for the session's view.
func (s *Service) filter(ctx context.Context, v View) (entities.Filter, error) {
	f := entities.Filter{Query: v.Query, Domain: entities.Domain(v.Domain)}
	if v.Shared {
		perms, err := s.d.Permissions.List(ctx)
		if err != nil {
			return f, err
		}
		f.Only = make(map[entities.ID]struct{}, len(perms))
		for _, p := range perms {
			f.Only[p.ID] = struct{}{}
		}
	}
	return f, nil
}

// refreshOffered recomputes, from live data, the page of entities the discovery
// select menu will offer and stores their IDs in the session. A submitted
// selection is later validated against exactly these IDs.
func (s *Service) refreshOffered(ctx context.Context, sess *Session) error {
	if sess.Status != StatusActive || sess.Step != StepDiscover {
		sess.Draft.View.Offered = nil
		return nil
	}
	snap, err := s.d.Discovery.Snapshot(ctx, false)
	if err != nil {
		return &HomeAssistantError{Err: err}
	}
	f, err := s.filter(ctx, sess.Draft.View)
	if err != nil {
		return err
	}
	page, current, _ := entities.Paginate(snap.Search(f), sess.Draft.View.Page, entities.PageSize)
	sess.Draft.View.Page = current
	ids := make([]entities.ID, 0, len(page))
	for _, it := range page {
		ids = append(ids, it.ID)
	}
	sess.SetOffered(ids)
	return nil
}

// Select applies the discovery select menu submission for the offered page.
func (s *Service) Select(ctx context.Context, c Caller, id string, ver int, chosen []entities.ID) (Session, error) {
	cfg, err := s.currentConfig(ctx, s.d.Permissions, s.d.Aliases)
	if err != nil {
		return Session{}, err
	}
	return s.mutate(ctx, c, id, ver, func(w *Session) error {
		return w.SetSelection(chosen, func(id entities.ID) Base { return cfg[id] })
	})
}

// Page moves to another page of the current list.
func (s *Service) Page(ctx context.Context, c Caller, id string, ver, page int) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error { return w.SetPage(page) })
}

// Search sets the discovery search text.
func (s *Service) Search(ctx context.Context, c Caller, id string, ver int, query string) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error { return w.SetQuery(query) })
}

// Filter sets the discovery domain filter and the shared-only toggle in one step.
func (s *Service) Filter(ctx context.Context, c Caller, id string, ver int, d entities.Domain, sharedOnly bool) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error {
		if err := w.SetShared(sharedOnly); err != nil {
			return err
		}
		return w.SetDomain(d)
	})
}

// FilterDomain restricts discovery to one domain ("" for all).
func (s *Service) FilterDomain(ctx context.Context, c Caller, id string, ver int, d entities.Domain) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error { return w.SetDomain(d) })
}

// FilterShared restricts discovery to currently shared entities.
func (s *Service) FilterShared(ctx context.Context, c Caller, id string, ver int, only bool) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error { return w.SetShared(only) })
}

// Focus chooses the entity that per-entity actions apply to.
func (s *Service) Focus(ctx context.Context, c Caller, id string, ver int, entity entities.ID) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error { return w.SetFocus(entity) })
}

// SetAccess sets the access of the focused entity.
func (s *Service) SetAccess(ctx context.Context, c Caller, id string, ver int, access entities.Access) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error {
		if w.Draft.View.Focus == "" {
			return ErrNotSelected
		}
		return w.SetAccess(entities.ID(w.Draft.View.Focus), access)
	})
}

// BulkAccess applies a rule to every selected entity.
func (s *Service) BulkAccess(ctx context.Context, c Caller, id string, ver int, rule BulkRule) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error {
		_, err := w.SetAccessBulk(rule)
		return err
	})
}

// SetAlias sets (or, with "", removes) the alias of the focused entity. An alias
// already used by an entity outside the draft is rejected immediately.
func (s *Service) SetAlias(ctx context.Context, c Caller, id string, ver int, raw string) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error {
		focus := entities.ID(w.Draft.View.Focus)
		if focus == "" {
			return ErrNotSelected
		}
		if err := w.SetAlias(focus, raw); err != nil {
			return err
		}
		alias := w.Draft.Entries[focus].Desired.Alias
		if alias == "" {
			return nil
		}
		_, key, err := entities.NormalizeAlias(alias)
		if err != nil {
			return err
		}
		holder, found, err := s.d.Aliases.Lookup(ctx, key)
		if err != nil {
			return err
		}
		if _, inDraft := w.Draft.Entries[holder]; found && holder != focus && !inDraft {
			return &entities.AliasTakenError{Alias: alias, Holder: holder}
		}
		return nil
	})
}
