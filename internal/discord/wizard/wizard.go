package wizard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/setup"
)

const staleNotice = ui.IconWarn + " That panel was out of date, so it was refreshed. Nothing was changed."

// Prober checks the subsystems shown on the health step (satisfied by *commands.Registry).
type Prober interface {
	Probe(ctx context.Context) commands.HealthReport
}

// ProberFunc adapts a function to Prober.
type ProberFunc func(ctx context.Context) commands.HealthReport

// Probe implements Prober.
func (f ProberFunc) Probe(ctx context.Context) commands.HealthReport { return f(ctx) }

// Deps are the collaborators of UI.
type Deps struct {
	Service *setup.Service
	Prober  Prober
	Version string
	Logger  *slog.Logger
}

// UI is the Discord front end of the setup service.
type UI struct{ d Deps }

// New builds the wizard UI.
func New(d Deps) *UI { return &UI{d: d} }

// Owns reports whether a custom ID belongs to the wizard.
func (u *UI) Owns(raw string) bool { return owns(raw) }

// OpensModal reports whether the component must be answered with a modal.
func (u *UI) OpensModal(raw string) bool {
	cid, ok := parseID(raw)
	return ok && opensModal(cid.act)
}

func caller(actor auth.Actor, guildID string) setup.Caller {
	return setup.Caller{Actor: actor, GuildID: guildID}
}

// Start begins or resumes the administrator's setup session.
func (u *UI) Start(ctx context.Context, actor auth.Actor, guildID string) commands.Reply {
	c := caller(actor, guildID)
	sess, resumed, err := u.d.Service.Begin(ctx, c)
	if err != nil {
		return u.failure(ctx, c, sess, err)
	}
	if resumed && (sess.Step != setup.StepHealth || len(sess.Draft.Entries) > 0) {
		return resumePanel(sess)
	}
	return u.render(ctx, c, sess, "")
}

// Modal answers a click that opens a text-input dialog.
func (u *UI) Modal(ctx context.Context, actor auth.Actor, guildID, raw string) commands.ModalResult {
	cid, ok := parseID(raw)
	if !ok || !opensModal(cid.act) {
		return commands.ModalResult{Notice: commands.Reply{Content: ui.IconFail + " That control is not valid."}}
	}
	c := caller(actor, guildID)
	sess, err := u.d.Service.Get(ctx, c, cid.sid)
	if err != nil {
		return commands.ModalResult{Notice: u.failure(ctx, c, sess, err)}
	}
	stale := commands.ModalResult{Notice: commands.Reply{Content: staleNotice}}
	if sess.Status != setup.StatusActive || sess.Version != cid.ver {
		return stale
	}
	switch {
	case cid.act == actSearch && sess.Step == setup.StepDiscover:
		return commands.ModalResult{Modal: searchModal(sess)}
	case cid.act == actAlias && sess.Step == setup.StepAliases:
		view, err := u.d.Service.Draft(ctx, sess)
		if err != nil || view.Focus == nil {
			return commands.ModalResult{Notice: commands.Reply{Content: ui.IconWarn + " Pick an entity first."}}
		}
		return commands.ModalResult{Modal: aliasModal(sess, view.Focus.Name, view.Focus.Entry.Desired.Alias)}
	}
	return stale
}

// Component handles a button click or select-menu submission.
func (u *UI) Component(ctx context.Context, actor auth.Actor, guildID, raw string, values []string) commands.Reply {
	return u.handle(ctx, actor, guildID, raw, values, nil)
}

// Submit handles a modal submission.
func (u *UI) Submit(ctx context.Context, actor auth.Actor, guildID, raw string, fields map[string]string) commands.Reply {
	return u.handle(ctx, actor, guildID, raw, nil, fields)
}

func (u *UI) handle(ctx context.Context, actor auth.Actor, guildID, raw string, values []string, fields map[string]string) commands.Reply {
	cid, ok := parseID(raw)
	if !ok {
		return commands.Reply{Content: ui.IconFail + " That control is not valid."}
	}
	c := caller(actor, guildID)
	sess, err := u.d.Service.Get(ctx, c, cid.sid)
	if err != nil {
		return u.failure(ctx, c, sess, err)
	}
	if sess.Status != setup.StatusActive {
		return u.render(ctx, c, sess, "")
	}
	if cid.ver != sess.Version {
		return u.render(ctx, c, sess, staleNotice)
	}
	return u.dispatch(ctx, c, sess, cid.act, values, fields)
}

// failure turns an error into a safe panel. Internal details go to the log only.
func (u *UI) failure(ctx context.Context, c setup.Caller, sess setup.Session, err error) commands.Reply {
	var stale *setup.StaleConfigError
	var invalid *setup.ValidationError
	var homeAssistant *setup.HomeAssistantError
	var taken *entities.AliasTakenError
	switch {
	case errors.Is(err, setup.ErrStale):
		return u.render(ctx, c, sess, staleNotice)
	case errors.Is(err, setup.ErrExpired):
		return expiredPanel()
	case errors.Is(err, setup.ErrNotFound), errors.Is(err, auth.ErrAdminRequired), errors.Is(err, auth.ErrNotAuthorized):
		return deniedPanel()
	case errors.Is(err, setup.ErrAlreadyApplied):
		return appliedPanel()
	case errors.Is(err, setup.ErrNotActive):
		if sess.Status == setup.StatusCompleted {
			return appliedPanel()
		}
		return finishedPanel()
	case errors.As(err, &stale):
		names := make([]string, 0, len(stale.Entities))
		for _, id := range stale.Entities {
			names = append(names, "`"+string(id)+"`")
		}
		return u.reload(ctx, c, sess, fmt.Sprintf("%s These entities were changed outside this setup while you were reviewing: %s. Your old confirmation was cancelled and nothing was applied. Review the updated differences and confirm again.", ui.IconWarn, strings.Join(names, ", ")))
	case errors.As(err, &invalid):
		return u.reload(ctx, c, sess, ui.IconWarn+" Nothing was applied: the review lists what blocks it.")
	case errors.As(err, &homeAssistant):
		return unavailablePanel(sess)
	case errors.As(err, &taken):
		return u.render(ctx, c, sess, fmt.Sprintf("%s The alias \"%s\" is already used%s.", ui.IconWarn, ui.Escape(taken.Alias), holder(taken)))
	}
	if msg, ok := userMessage(err); ok && sess.ID != "" {
		return u.render(ctx, c, sess, ui.IconWarn+" "+msg)
	}
	u.d.Logger.Error("setup interaction failed", "error", err)
	return commands.Reply{Content: ui.IconFail + " Something went wrong. The details are in the NeptHass log. Your progress is saved."}
}

func holder(e *entities.AliasTakenError) string {
	if e.Holder == "" {
		return ""
	}
	return " by `" + string(e.Holder) + "`"
}

func (u *UI) reload(ctx context.Context, c setup.Caller, sess setup.Session, notice string) commands.Reply {
	fresh, err := u.d.Service.Get(ctx, c, sess.ID)
	if err != nil {
		return u.failure(ctx, c, sess, err)
	}
	return u.render(ctx, c, fresh, notice)
}

// userMessage maps expected validation errors to text safe to show.
func userMessage(err error) (string, bool) {
	switch {
	case errors.Is(err, setup.ErrNothingSelected):
		return "Select at least one entity first.", true
	case errors.Is(err, setup.ErrTooManySelected):
		return fmt.Sprintf("At most %d entities can be configured in one session.", setup.MaxSelected), true
	case errors.Is(err, entities.ErrNotControllable):
		return "Control is limited to lights and switches.", true
	case errors.Is(err, entities.ErrInvalidAlias):
		return "That alias is not valid: " + strings.TrimPrefix(err.Error(), entities.ErrInvalidAlias.Error()+": "), true
	case errors.Is(err, setup.ErrNotOffered):
		return "That entity was not offered on this page, so the list was refreshed.", true
	case errors.Is(err, setup.ErrNotSelected):
		return "Pick one of the selected entities first.", true
	case errors.Is(err, setup.ErrNothingToApply):
		return "There is nothing to apply: everything already matches the current configuration.", true
	case errors.Is(err, setup.ErrWrongStep), errors.Is(err, setup.ErrInvalidTransition):
		return "That action is not available at this step.", true
	}
	return "", false
}
