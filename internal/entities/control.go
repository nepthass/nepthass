package entities

import (
	"context"
	"errors"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
)

// ControlResult is the outcome of a successful command.
type ControlResult struct {
	Before State
	// After is the state read back after the command; nil when the read-back failed.
	// Devices often report their new state a moment later, so it may equal Before.
	After *State
}

// Control turns an entity on or off. The actor needs control access on the
// entity, and the entity's domain must be on the control allowlist even if the
// database says otherwise. Every attempt, allowed or not, is audited.
func (s *Service) Control(ctx context.Context, actor auth.Actor, id ID, action Action) (ControlResult, error) {
	var res ControlResult
	err := s.audited(ctx, audit.Entry{ActorID: actor.UserID, Command: "control", EntityID: string(id), Action: string(action)}, func() error {
		if err := actor.Require(auth.LevelUser); err != nil {
			return err
		}
		access, err := s.d.Permissions.Get(ctx, id)
		if err != nil {
			return err
		}
		if !access.AtLeast(AccessControl) {
			return ErrAccessDenied
		}
		if !id.Domain().Controllable() {
			return ErrNotControllable
		}
		if res.Before, err = s.d.HA.State(ctx, id); err != nil {
			return err
		}
		if err := s.d.HA.Control(ctx, id, action); err != nil {
			return err
		}
		if after, err := s.d.HA.State(ctx, id); err == nil {
			res.After = &after
		}
		return nil
	})
	if err != nil {
		return ControlResult{}, err
	}
	return res, nil
}

// audited runs fn and records the outcome. Audit write failures are logged
// but never turn a completed action into an error.
func (s *Service) audited(ctx context.Context, e audit.Entry, fn func() error) error {
	err := fn()
	e.Time = s.d.Now()
	e.Outcome = audit.OutcomeSuccess
	if err != nil {
		e.Detail = err.Error()
		e.Outcome = audit.OutcomeError
		if isDenial(err) {
			e.Outcome = audit.OutcomeDenied
		}
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if recErr := s.d.Audit.Record(auditCtx, e); recErr != nil {
		s.d.Logger.Error("audit write failed", "command", e.Command, "error", recErr)
	}
	return err
}

func isDenial(err error) bool {
	return errors.Is(err, ErrAccessDenied) || errors.Is(err, ErrNotControllable) ||
		errors.Is(err, auth.ErrNotAuthorized) || errors.Is(err, auth.ErrAdminRequired)
}
