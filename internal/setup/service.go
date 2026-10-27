package setup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
)

const staleSessionRetention = 30 * 24 * time.Hour

var (
	// ErrExpired means the session sat idle past its expiry and was closed.
	ErrExpired = errors.New("this setup session expired")
	// ErrStale means the panel the click came from is out of date; the caller
	// should re-render the returned (current) session instead of acting.
	ErrStale = errors.New("this setup panel is out of date")
	// ErrAlreadyApplied means a confirmation was replayed after the setup was applied.
	ErrAlreadyApplied = errors.New("this setup was already applied")
)

// HomeAssistantError means Home Assistant could not supply live data. The wizard
// never proceeds on stale or invented entity data.
type HomeAssistantError struct{ Err error }

func (e *HomeAssistantError) Error() string { return "home assistant is unavailable: " + e.Err.Error() }
func (e *HomeAssistantError) Unwrap() error { return e.Err }

// Problem blocks an entity from being applied.
type Problem struct {
	ID     entities.ID
	Reason string
}

// ValidationError lists everything that blocks applying the plan.
type ValidationError struct{ Problems []Problem }

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, string(p.ID)+": "+p.Reason)
	}
	return "setup cannot be applied: " + strings.Join(parts, "; ")
}

// StaleConfigError means entities changed outside the session after they were
// selected. Nothing was applied; the session was rebased onto the live
// configuration and its version bumped, so the old confirmation is dead and the
// administrator reviews again.
type StaleConfigError struct{ Entities []entities.ID }

func (e *StaleConfigError) Error() string {
	return fmt.Sprintf("%d entities changed outside this setup session", len(e.Entities))
}

// Caller identifies who is acting and where.
type Caller struct {
	Actor   auth.Actor
	GuildID string
}

// Deps are the collaborators of Service.
type Deps struct {
	DB          *sql.DB
	Store       *Store
	Discovery   *entities.Discovery
	Permissions *entities.Permissions
	Aliases     *entities.Aliases
	Audit       *audit.Log
	Now         func() time.Time
}

// Service runs setup sessions. Every method re-checks that the caller is an
// administrator and that the session belongs to them in this guild.
type Service struct{ d Deps }

// NewService builds a Service.
func NewService(d Deps) *Service { return &Service{d: d} }

func (s *Service) load(ctx context.Context, c Caller, id string) (Session, error) {
	if err := c.Actor.Require(auth.LevelAdmin); err != nil {
		return Session{}, err
	}
	sess, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Session{}, err
	}
	// A session of another administrator or guild is indistinguishable from a missing one.
	if sess.GuildID != c.GuildID || sess.AdminID != c.Actor.UserID {
		return Session{}, ErrNotFound
	}
	if sess.Expired(s.d.Now()) {
		return s.expire(ctx, sess)
	}
	return sess, nil
}

func (s *Service) expire(ctx context.Context, sess Session) (Session, error) {
	sess.Status = StatusExpired
	sess.UpdatedAt = s.d.Now()
	if err := s.d.Store.Save(ctx, &sess); err != nil && !errors.Is(err, ErrConflict) {
		return Session{}, err
	}
	return sess, ErrExpired
}

// Store exposes the underlying store (used by tests and maintenance).
func (s *Service) Store() *Store { return s.d.Store }

// Get returns the caller's session for display.
func (s *Service) Get(ctx context.Context, c Caller, id string) (Session, error) {
	return s.load(ctx, c, id)
}

// Begin returns the administrator's active session (resumed=true) or starts a new one.
func (s *Service) Begin(ctx context.Context, c Caller) (sess Session, resumed bool, err error) {
	if err := c.Actor.Require(auth.LevelAdmin); err != nil {
		return Session{}, false, err
	}
	cur, err := s.d.Store.Active(ctx, c.GuildID, c.Actor.UserID)
	switch {
	case err == nil && !cur.Expired(s.d.Now()):
		return cur, true, nil
	case err == nil:
		if _, expErr := s.expire(ctx, cur); expErr != nil && !errors.Is(expErr, ErrExpired) {
			return Session{}, false, expErr
		}
	case !errors.Is(err, ErrNotFound):
		return Session{}, false, err
	}
	return s.create(ctx, c)
}

func (s *Service) create(ctx context.Context, c Caller) (Session, bool, error) {
	id, err := newID()
	if err != nil {
		return Session{}, false, err
	}
	sess := newSession(id, c.GuildID, c.Actor.UserID, s.d.Now())
	if err := s.d.Store.Create(ctx, sess); err != nil {
		if errors.Is(err, ErrActiveExists) { // lost a race with a concurrent /setup
			cur, getErr := s.d.Store.Active(ctx, c.GuildID, c.Actor.UserID)
			return cur, true, getErr
		}
		return Session{}, false, err
	}
	_, _ = s.d.Store.Prune(ctx, s.d.Now().Add(-staleSessionRetention))
	return sess, false, nil
}

// Restart cancels the given session (if still active) and starts a fresh one.
func (s *Service) Restart(ctx context.Context, c Caller, id string) (Session, error) {
	sess, err := s.load(ctx, c, id)
	if err != nil && !errors.Is(err, ErrExpired) {
		return Session{}, err
	}
	if sess.Status == StatusActive {
		sess.Status = StatusCancelled
		sess.Touch(s.d.Now())
		if err := s.d.Store.Save(ctx, &sess); err != nil && !errors.Is(err, ErrConflict) {
			return Session{}, err
		}
	}
	fresh, _, err := s.create(ctx, c)
	return fresh, err
}

// mutate loads the caller's session, requires it to be active and at version
// ver, applies fn to a copy, refreshes the offered page and saves with a
// compare-and-swap. On any failure the persisted session is returned unchanged
// (ErrStale carries the current session so the caller can re-render it).
func (s *Service) mutate(ctx context.Context, c Caller, id string, ver int, fn func(*Session) error) (Session, error) {
	sess, err := s.load(ctx, c, id)
	if err != nil {
		return sess, err
	}
	if sess.Status != StatusActive {
		return sess, ErrNotActive
	}
	if sess.Version != ver {
		return sess, ErrStale
	}
	work := sess.Clone()
	if err := fn(&work); err != nil {
		return sess, err
	}
	if err := s.refreshOffered(ctx, &work); err != nil {
		return sess, err
	}
	work.Touch(s.d.Now())
	if err := s.d.Store.Save(ctx, &work); err != nil {
		if errors.Is(err, ErrConflict) {
			latest, getErr := s.d.Store.Get(ctx, id)
			if getErr != nil {
				return sess, getErr
			}
			return latest, ErrStale
		}
		return sess, err
	}
	return work, nil
}

// Navigate applies a navigation event. Leaving the health step requires a fresh
// Home Assistant fetch to succeed.
func (s *Service) Navigate(ctx context.Context, c Caller, id string, ver int, ev Event) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(w *Session) error {
		if w.Step == StepHealth && ev == EventNext {
			if _, err := s.d.Discovery.Snapshot(ctx, true); err != nil {
				return &HomeAssistantError{Err: err}
			}
		}
		return w.Transition(ev)
	})
}

// Refresh re-fetches Home Assistant's entities and re-renders the current page.
func (s *Service) Refresh(ctx context.Context, c Caller, id string, ver int) (Session, error) {
	return s.mutate(ctx, c, id, ver, func(*Session) error {
		if _, err := s.d.Discovery.Snapshot(ctx, true); err != nil {
			return &HomeAssistantError{Err: err}
		}
		return nil
	})
}

// currentConfig is the live access and alias of every entity that has either.
func (s *Service) currentConfig(ctx context.Context, perms *entities.Permissions, aliases *entities.Aliases) (map[entities.ID]Base, error) {
	list, err := perms.List(ctx)
	if err != nil {
		return nil, err
	}
	all, err := aliases.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[entities.ID]Base, len(list)+len(all))
	for _, p := range list {
		out[p.ID] = Base{Access: p.Access}
	}
	for id, alias := range all {
		b := out[id]
		b.Alias = alias
		out[id] = b
	}
	return out, nil
}
