package entities

import (
	"context"
	"slices"
	"strings"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
)

// Allow grants an access level on an entity. Only administrators may call it.
// The entity must exist in Home Assistant, and control access is refused for
// domains outside the control allowlist.
func (s *Service) Allow(ctx context.Context, actor auth.Actor, id ID, access Access) error {
	e := audit.Entry{ActorID: actor.UserID, Command: "permissions.allow", EntityID: string(id), Action: string(access)}
	return s.audited(ctx, e, func() error {
		if err := actor.Require(auth.LevelAdmin); err != nil {
			return err
		}
		if access == AccessControl && !id.Domain().Controllable() {
			return ErrNotControllable
		}
		if _, err := s.d.HA.State(ctx, id); err != nil {
			return err
		}
		return s.d.Permissions.Set(ctx, Grant{ID: id, Access: access, GrantedBy: actor.UserID, At: s.d.Now()})
	})
}

// Revoke removes an entity from the allowlist and reports whether it was listed.
func (s *Service) Revoke(ctx context.Context, actor auth.Actor, id ID) (bool, error) {
	var removed bool
	e := audit.Entry{ActorID: actor.UserID, Command: "permissions.revoke", EntityID: string(id)}
	err := s.audited(ctx, e, func() error {
		if err := actor.Require(auth.LevelAdmin); err != nil {
			return err
		}
		var err error
		removed, err = s.d.Permissions.Revoke(ctx, id)
		return err
	})
	return removed, err
}

// Permissions lists the allowlist. Only administrators may call it.
func (s *Service) Permissions(ctx context.Context, actor auth.Actor) ([]Permission, error) {
	if err := actor.Require(auth.LevelAdmin); err != nil {
		return nil, err
	}
	return s.d.Permissions.List(ctx)
}

// Discover searches every Home Assistant entity by ID or friendly name so an
// administrator can pick one to allow. Only administrators may call it.
func (s *Service) Discover(ctx context.Context, actor auth.Actor, query string) ([]State, error) {
	if err := actor.Require(auth.LevelAdmin); err != nil {
		return nil, err
	}
	states, err := s.d.HA.States(ctx)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var matches []State
	for _, st := range states {
		if strings.Contains(string(st.ID), query) || strings.Contains(strings.ToLower(st.FriendlyName()), query) {
			matches = append(matches, st)
		}
	}
	slices.SortFunc(matches, func(a, b State) int { return strings.Compare(string(a.ID), string(b.ID)) })
	if len(matches) > MaxSuggestions {
		matches = matches[:MaxSuggestions]
	}
	return matches, nil
}
