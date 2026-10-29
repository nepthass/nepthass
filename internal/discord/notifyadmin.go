package discord

import (
	"context"
	"log/slog"
	"strings"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/notify"
)

// AliasLister names sensors. *entities.Aliases satisfies it.
type AliasLister interface {
	All(ctx context.Context) (map[entities.ID]string, error)
}

// NotificationAdmin implements commands.Notifications. Every successful change
// is followed by an engine reload, so the running engine never lags the stored
// configuration. Channel checks that need Discord happen here, before the
// administrator-only service is called.
type NotificationAdmin struct {
	svc      *notify.Service
	eng      *notify.Engine
	notifier *Notifier
	sensors  notify.SensorLookup
	aliases  AliasLister
	log      *slog.Logger
}

// NewNotificationAdmin returns the adapter.
func NewNotificationAdmin(svc *notify.Service, eng *notify.Engine, n *Notifier, sensors notify.SensorLookup, aliases AliasLister, log *slog.Logger) *NotificationAdmin {
	return &NotificationAdmin{svc: svc, eng: eng, notifier: n, sensors: sensors, aliases: aliases, log: log}
}

var _ commands.Notifications = (*NotificationAdmin)(nil)

func (a *NotificationAdmin) reload(ctx context.Context) error {
	if err := a.eng.Reload(ctx); err != nil {
		a.log.Error("notification engine reload failed", "error", err)
		return &commands.UserError{Msg: "The change was saved, but the running notification engine could not reload it. It will apply after the next restart."}
	}
	return nil
}

func (a *NotificationAdmin) Config(ctx context.Context, actor auth.Actor) (notify.Config, error) {
	return a.svc.Config(ctx, actor)
}

func (a *NotificationAdmin) SetChannel(ctx context.Context, actor auth.Actor, channelID string) (string, error) {
	if err := actor.Require(auth.LevelAdmin); err != nil {
		return "", err
	}
	name, err := a.notifier.ValidateChannel(ctx, channelID)
	if err != nil {
		return "", err
	}
	if err := a.svc.SetChannel(ctx, actor, channelID); err != nil {
		return "", err
	}
	return name, a.reload(ctx)
}

func (a *NotificationAdmin) SetPaused(ctx context.Context, actor auth.Actor, paused bool) error {
	if err := a.svc.SetEnabled(ctx, actor, !paused); err != nil {
		return err
	}
	return a.reload(ctx)
}

func (a *NotificationAdmin) AddRule(ctx context.Context, actor auth.Actor, n notify.NewRule) (commands.RuleItem, error) {
	r, err := a.svc.AddRule(ctx, actor, n)
	if err != nil {
		return commands.RuleItem{}, err
	}
	if err := a.reload(ctx); err != nil {
		return commands.RuleItem{}, err
	}
	names, _ := a.aliases.All(ctx)
	item := commands.RuleItem{RuleView: notify.RuleView{Rule: r, Status: notify.StatusActive}, Name: a.nameOf(r.Entity, names)}
	if s, ok := a.sensors.Get(r.Entity); ok {
		item.Category = s.Category
	}
	return item, nil
}

func (a *NotificationAdmin) Rules(ctx context.Context, actor auth.Actor) ([]commands.RuleItem, error) {
	views, err := a.svc.Rules(ctx, actor)
	if err != nil {
		return nil, err
	}
	names, _ := a.aliases.All(ctx)
	items := make([]commands.RuleItem, len(views))
	for i, v := range views {
		items[i] = commands.RuleItem{RuleView: v, Name: a.nameOf(v.Entity, names)}
	}
	return items, nil
}

// nameOf is the alias, else the Home Assistant friendly name, else a generic
// label. It never returns the entity ID.
func (a *NotificationAdmin) nameOf(id entities.ID, aliases map[entities.ID]string) string {
	if n := aliases[id]; n != "" {
		return n
	}
	if s, ok := a.sensors.Get(id); ok {
		if s.Name != "" {
			return s.Name
		}
		return s.Category.Label() + " sensor"
	}
	return "Sensor that is not monitored"
}

func (a *NotificationAdmin) SetRuleEnabled(ctx context.Context, actor auth.Actor, id int64, enabled bool) error {
	if err := a.svc.SetRuleEnabled(ctx, actor, id, enabled); err != nil {
		return err
	}
	return a.reload(ctx)
}

func (a *NotificationAdmin) RemoveRule(ctx context.Context, actor auth.Actor, id int64) error {
	if err := a.svc.RemoveRule(ctx, actor, id); err != nil {
		return err
	}
	return a.reload(ctx)
}

func (a *NotificationAdmin) SendTest(ctx context.Context, actor auth.Actor) error {
	if err := actor.Require(auth.LevelAdmin); err != nil {
		return err
	}
	return a.notifier.SendTest(ctx)
}

func (a *NotificationAdmin) Status(ctx context.Context, actor auth.Actor) (commands.NotificationStatus, error) {
	cfg, err := a.svc.Config(ctx, actor)
	if err != nil {
		return commands.NotificationStatus{}, err
	}
	views, err := a.svc.Rules(ctx, actor)
	if err != nil {
		return commands.NotificationStatus{}, err
	}
	counts := map[notify.RuleStatus]int{}
	for _, v := range views {
		counts[v.Status]++
	}
	ns := a.notifier.Status()
	return commands.NotificationStatus{Config: cfg, Health: a.eng.Health(), Rules: counts, LastDelivery: ns.LastSuccess,
		LastFailure: describeFailure(ns.LastFailure), LastFailureAt: ns.LastFailureAt}, nil
}

func describeFailure(category string) string {
	switch category {
	case "":
		return ""
	case "missing_permissions":
		return "the bot lacks permission in the channel"
	case "channel_missing":
		return "the channel was deleted or is not visible to the bot"
	case "unauthorized":
		return "Discord rejected the bot token"
	case "rate_limited":
		return "rate limited by Discord"
	case "server_error":
		return "Discord had a server error"
	case "network":
		return "network problem reaching Discord"
	}
	return "delivery failed"
}

// HealthLine is the one line /health shows to any user. It says whether
// notifications work and nothing about channels, rules or sensors.
func (a *NotificationAdmin) HealthLine() string {
	h := a.eng.Health()
	ns := a.notifier.Status()
	switch {
	case !h.Active:
		return ui.IconDisabled + " Disabled"
	case h.Degraded || h.BreakerOpen:
		return ui.IconWarn + " Degraded: " + strings.Join(append([]string(nil), h.Reasons...), "; ")
	case ns.LastFailure != "" && ns.LastFailureAt.After(ns.LastSuccess):
		return ui.IconWarn + " The last delivery failed: " + describeFailure(ns.LastFailure)
	}
	return ui.IconOK + " Enabled"
}
