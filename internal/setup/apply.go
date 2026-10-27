package setup

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/entities"
)

// Result summarizes an applied setup.
type Result struct {
	New, Modified, Revoked, AliasChanges int
	Entries                              []PlanEntry
}

// Apply writes the reviewed configuration, but only for a confirmation of the
// exact session version that was reviewed. In one SQLite transaction it
// verifies the session, re-reads the live configuration, detects changes made
// outside the session, validates, writes permissions and aliases, writes the
// audit entries and completes the session. Any failure rolls everything back
// and leaves the session active; a replayed confirmation finds the session
// completed and writes nothing. Home Assistant is consulted before the
// transaction so entities must really exist.
func (s *Service) Apply(ctx context.Context, c Caller, id string, ver int) (Result, error) {
	sess, err := s.load(ctx, c, id)
	if err != nil {
		return Result{}, err
	}
	switch {
	case sess.Status == StatusCompleted:
		return Result{}, ErrAlreadyApplied
	case sess.Status != StatusActive:
		return Result{}, ErrNotActive
	case sess.Step != StepReview:
		return Result{}, ErrWrongStep
	case sess.Version != ver:
		return Result{}, ErrStale
	}
	snap, err := s.d.Discovery.Snapshot(ctx, true)
	if err != nil {
		return Result{}, &HomeAssistantError{Err: err}
	}
	return s.applyTx(ctx, c, id, ver, snap)
}

func (s *Service) applyTx(ctx context.Context, c Caller, id string, ver int, snap *entities.Snapshot) (result Result, err error) {
	tx, err := s.d.DB.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin apply: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	store, perms, aliases, log := s.d.Store.WithTx(tx), s.d.Permissions.WithTx(tx), s.d.Aliases.WithTx(tx), s.d.Audit.WithTx(tx)

	sess, err := store.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	switch {
	case sess.Status == StatusCompleted:
		return Result{}, ErrAlreadyApplied
	case sess.Status != StatusActive:
		return Result{}, ErrNotActive
	case sess.Version != ver:
		return Result{}, ErrStale
	}
	cfg, err := s.currentConfig(ctx, perms, aliases)
	if err != nil {
		return Result{}, err
	}
	plan := buildPlan(sess, snap, cfg)

	if stale := plan.StaleIDs(); len(stale) > 0 {
		// Rebase onto the live configuration and invalidate the old confirmation.
		for _, sid := range stale {
			e := sess.Draft.Entries[sid]
			e.Base = cfg[sid]
			sess.Draft.Entries[sid] = e
		}
		sess.Touch(s.d.Now())
		if err := store.Save(ctx, &sess); err != nil {
			return Result{}, err
		}
		if err := tx.Commit(); err != nil {
			return Result{}, fmt.Errorf("commit rebase: %w", err)
		}
		committed = true
		return Result{}, &StaleConfigError{Entities: stale}
	}
	if problems := plan.Problems(); len(problems) > 0 {
		return Result{}, &ValidationError{Problems: problems}
	}
	if plan.Changes() == 0 && plan.AliasChanges == 0 {
		return Result{}, ErrNothingToApply
	}
	if err := s.write(ctx, c, plan, perms, aliases, log); err != nil {
		return Result{}, err
	}
	if err := sess.Transition(EventConfirm); err != nil {
		return Result{}, err
	}
	sess.Touch(s.d.Now())
	if err := store.Save(ctx, &sess); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit apply: %w", err)
	}
	committed = true
	return Result{New: plan.New, Modified: plan.Modified, Revoked: plan.Revoked, AliasChanges: plan.AliasChanges, Entries: slices.DeleteFunc(slices.Clone(plan.Entries), func(e PlanEntry) bool { return e.Kind == KindUnchanged && e.Alias == AliasUnchanged })}, nil
}

// write applies every changed entry inside the caller's transaction and audits it.
func (s *Service) write(ctx context.Context, c Caller, plan Plan, perms *entities.Permissions, aliases *entities.Aliases, log *audit.Log) error {
	now := s.d.Now()
	record := func(command string, id entities.ID, action, detail string) error {
		return log.Record(ctx, audit.Entry{Time: now, ActorID: c.Actor.UserID, Command: command, EntityID: string(id), Action: action, Outcome: audit.OutcomeSuccess, Detail: detail})
	}
	for _, e := range plan.Entries {
		if e.Current.Access != e.Desired.Access {
			if e.Desired.Access == entities.AccessNone {
				if _, err := perms.Revoke(ctx, e.ID); err != nil {
					return err
				}
				if err := record("setup.revoke", e.ID, "revoke", ""); err != nil {
					return err
				}
			} else {
				if err := perms.Set(ctx, entities.Grant{ID: e.ID, Access: e.Desired.Access, GrantedBy: c.Actor.UserID, At: now}); err != nil {
					return err
				}
				if err := record("setup.allow", e.ID, string(e.Desired.Access), ""); err != nil {
					return err
				}
			}
		}
		if e.Alias == AliasUnchanged {
			continue
		}
		if e.Desired.Alias == "" {
			if _, err := aliases.Remove(ctx, e.ID); err != nil {
				return err
			}
			if err := record("setup.alias", e.ID, "remove", ""); err != nil {
				return err
			}
			continue
		}
		if err := aliases.Set(ctx, entities.AliasSet{ID: e.ID, Alias: e.Desired.Alias, By: c.Actor.UserID, At: now}); err != nil {
			var taken *entities.AliasTakenError
			if errors.As(err, &taken) {
				return &ValidationError{Problems: []Problem{{ID: e.ID, Reason: err.Error()}}}
			}
			return err
		}
		if err := record("setup.alias", e.ID, "set", e.Desired.Alias); err != nil {
			return err
		}
	}
	return record("setup.apply", "", "confirm", fmt.Sprintf("%d new, %d modified, %d revoked, %d alias changes", plan.New, plan.Modified, plan.Revoked, plan.AliasChanges))
}
