package wizard

import (
	"context"
	"errors"

	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/setup"
)

// render builds the panel for the session's current step from live data.
func (u *UI) render(ctx context.Context, c setup.Caller, sess setup.Session, notice string) commands.Reply {
	switch sess.Status {
	case setup.StatusCancelled:
		return cancelledPanel()
	case setup.StatusExpired:
		return expiredPanel()
	case setup.StatusCompleted:
		return appliedPanel()
	case setup.StatusActive:
	}
	svc := u.d.Service
	switch sess.Step {
	case setup.StepHealth:
		return healthPanel(sess, u.d.Prober.Probe(ctx), u.d.Version, notice)
	case setup.StepDiscover:
		view, err := svc.Discover(ctx, sess)
		if err != nil {
			return u.renderFailure(ctx, c, sess, err)
		}
		return discoverPanel(sess, view, notice)
	case setup.StepPermissions, setup.StepAliases:
		view, err := svc.Draft(ctx, sess)
		if err != nil {
			return u.renderFailure(ctx, c, sess, err)
		}
		if sess.Step == setup.StepPermissions {
			return permissionsPanel(sess, view, notice)
		}
		return aliasesPanel(sess, view, notice)
	case setup.StepReview:
		plan, err := svc.Plan(ctx, c, sess.ID)
		if err != nil {
			return u.renderFailure(ctx, c, sess, err)
		}
		return reviewPanel(sess, plan, notice)
	case setup.StepDone:
		return appliedPanel()
	}
	return finishedPanel()
}

// renderFailure avoids recursion: a panel that cannot be built is a failure panel.
func (u *UI) renderFailure(ctx context.Context, c setup.Caller, sess setup.Session, err error) commands.Reply {
	var homeAssistant *setup.HomeAssistantError
	if errors.As(err, &homeAssistant) {
		return unavailablePanel(sess)
	}
	u.d.Logger.Error("setup panel failed", "step", string(sess.Step), "error", err)
	return commands.Reply{Content: "\u274C Something went wrong while building this panel. The details are in the NeptHass log. Your progress is saved."}
}

// dispatch performs one authorized, version-checked action and renders the result.
func (u *UI) dispatch(ctx context.Context, c setup.Caller, sess setup.Session, act action, values []string, fields map[string]string) commands.Reply {
	svc, sid, ver := u.d.Service, sess.ID, sess.Version
	var next setup.Session
	var err error
	switch act {
	case actRetry, actResume:
		return u.render(ctx, c, sess, "")
	case actRestart:
		next, err = svc.Restart(ctx, c, sid)
	case actNext:
		next, err = svc.Navigate(ctx, c, sid, ver, setup.EventNext)
	case actBack:
		next, err = svc.Navigate(ctx, c, sid, ver, setup.EventBack)
	case actCancel:
		next, err = svc.Navigate(ctx, c, sid, ver, setup.EventCancel)
	case actConfirm:
		result, applyErr := svc.Apply(ctx, c, sid, ver)
		if applyErr != nil {
			return u.failure(ctx, c, sess, applyErr)
		}
		return donePanel(result)
	case actRefresh:
		next, err = svc.Refresh(ctx, c, sid, ver)
	case actPagePrev:
		next, err = svc.Page(ctx, c, sid, ver, sess.Draft.View.Page-1)
	case actPageNext:
		next, err = svc.Page(ctx, c, sid, ver, sess.Draft.View.Page+1)
	case actSearchForm:
		next, err = svc.Search(ctx, c, sid, ver, fields["q"])
	case actClear:
		next, err = svc.Search(ctx, c, sid, ver, "")
	case actSelect:
		next, err = svc.Select(ctx, c, sid, ver, parseIDs(values))
	case actDomain:
		domain, shared, ok := parseDomainChoice(first(values))
		if !ok {
			return u.render(ctx, c, sess, staleNotice)
		}
		next, err = svc.Filter(ctx, c, sid, ver, domain, shared)
	case actFocus:
		next, err = svc.Focus(ctx, c, sid, ver, entities.ID(first(values)))
	case actRead:
		next, err = svc.SetAccess(ctx, c, sid, ver, entities.AccessRead)
	case actControl:
		next, err = svc.SetAccess(ctx, c, sid, ver, entities.AccessControl)
	case actNone:
		next, err = svc.SetAccess(ctx, c, sid, ver, entities.AccessNone)
	case actBulkRead:
		next, err = svc.BulkAccess(ctx, c, sid, ver, setup.BulkReadAll)
	case actBulkControl:
		next, err = svc.BulkAccess(ctx, c, sid, ver, setup.BulkControlSupported)
	case actBulkClear:
		next, err = svc.BulkAccess(ctx, c, sid, ver, setup.BulkClearAll)
	case actAliasForm:
		next, err = svc.SetAlias(ctx, c, sid, ver, fields["alias"])
	case actUnalias:
		next, err = svc.SetAlias(ctx, c, sid, ver, "")
	case actSearch, actAlias:
		// These open a modal; arriving here as a plain component action is not valid.
		return u.render(ctx, c, sess, staleNotice)
	}
	if err != nil {
		if next.ID == "" {
			next = sess
		}
		return u.failure(ctx, c, next, err)
	}
	return u.render(ctx, c, next, "")
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// parseIDs validates submitted entity IDs; an invalid value becomes an ID the
// service will reject as not offered, so a forged value changes nothing.
func parseIDs(values []string) []entities.ID {
	ids := make([]entities.ID, 0, len(values))
	for _, v := range values {
		id, err := entities.ParseID(v)
		if err != nil {
			id = "" // never offered, so the service rejects the whole submission
		}
		ids = append(ids, id)
	}
	return ids
}
