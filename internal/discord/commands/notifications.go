package commands

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

// UserError is a failure whose message is safe and useful to show an
// administrator verbatim (for example why a channel was rejected).
type UserError struct {
	Msg string
	// Reason is a short code from a fixed vocabulary (never user input), used
	// only to label an audit entry. It may be empty.
	Reason string
}

func (e *UserError) Error() string { return e.Msg }

// RuleItem is a rule with its status and a display name that is never the entity ID.
type RuleItem struct {
	notify.RuleView
	Name string
}

// NotificationStatus is everything /notifications status shows.
type NotificationStatus struct {
	Config        notify.Config
	Health        notify.Health
	Rules         map[notify.RuleStatus]int
	LastDelivery  time.Time
	LastFailure   string
	LastFailureAt time.Time
}

// Notifications is the administrator-facing notification service. The Discord
// adapter implements it: it validates channels, saves through notify.Service and
// reloads the engine after every successful change.
type Notifications interface {
	Config(ctx context.Context, actor auth.Actor) (notify.Config, error)
	SetChannel(ctx context.Context, actor auth.Actor, channelID string) (channelName string, err error)
	// SetPaused turns the whole feature off (true) or on (false).
	SetPaused(ctx context.Context, actor auth.Actor, paused bool) error
	AddRule(ctx context.Context, actor auth.Actor, n notify.NewRule) (RuleItem, error)
	Rules(ctx context.Context, actor auth.Actor) ([]RuleItem, error)
	SetRuleEnabled(ctx context.Context, actor auth.Actor, id int64, enabled bool) error
	RemoveRule(ctx context.Context, actor auth.Actor, id int64) error
	SendTest(ctx context.Context, actor auth.Actor) error
	Status(ctx context.Context, actor auth.Actor) (NotificationStatus, error)
	// HealthLine is one non-sensitive line for /health, safe for any user.
	HealthLine() string
}

const (
	maxCooldownMinutes = int(notify.MaxCooldown / time.Minute)
	maxListedRules     = 25
)

func (r *Registry) cmdNotifications() command {
	sub := func(name, description string, opts ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionSubCommand, Name: name, Description: description, Options: opts}
	}
	zero, one, lowest := 0.0, 1.0, -float64(notify.MaxThreshold)
	number := func(name, description string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionNumber, Name: name, Description: description, MinValue: &lowest, MaxValue: notify.MaxThreshold}
	}
	rule := func(required bool, description string) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: "rule", Description: description,
			Required: required, Autocomplete: true, MinValue: &one}
	}
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "notifications", Description: "Configure alerts for shared sensors (administrators)", Contexts: guildOnly,
			Options: []*discordgo.ApplicationCommandOption{
				sub("channel", "Choose the text channel that receives alerts", &discordgo.ApplicationCommandOption{
					Type: discordgo.ApplicationCommandOptionChannel, Name: "channel", Description: "A text channel in this server", Required: true,
					ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText}}),
				sub("add", "Alert when a shared sensor changes (no threshold: state sensors; above or below: numeric sensors)",
					entityOption("Sensor to watch"), number("above", "Alert when a numeric sensor rises above this value"),
					number("below", "Alert when a numeric sensor falls below this value"),
					&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: "cooldown",
						Description: "Minutes between alerts for this rule (0 for none; default depends on the sensor type)", MinValue: &zero, MaxValue: float64(maxCooldownMinutes)}),
				sub("list", "Show every rule and its status"),
				sub("pause", "Pause one rule, or all notifications when no rule is given", rule(false, "Rule to pause")),
				sub("resume", "Resume one rule, or all notifications when no rule is given", rule(false, "Rule to resume")),
				sub("remove", "Delete a rule", rule(true, "Rule to delete")),
				sub("test", "Send a test message to the alert channel"),
				sub("status", "Show delivery health, queue and rule status"),
			},
		},
		min:      auth.LevelAdmin,
		run:      r.notifications,
		complete: r.completeNotifications,
	}
}

func (r *Registry) notifications(ctx context.Context, req Request) Reply {
	switch req.Subcommand {
	case "channel":
		return r.notifyChannel(ctx, req)
	case "add":
		return r.notifyAdd(ctx, req)
	case "list":
		return r.notifyList(ctx, req)
	case "pause":
		return r.notifyToggle(ctx, req, false)
	case "resume":
		return r.notifyToggle(ctx, req, true)
	case "remove":
		return r.notifyRemove(ctx, req)
	case "test":
		if err := r.deps.Notifications.SendTest(ctx, req.Actor); err != nil {
			return r.notifyFail(req, err)
		}
		return Reply{Content: ui.IconOK + " A test message was sent to the alert channel. If nobody sees it there, check the channel and the bot's permissions with /notifications status."}
	case "status":
		return r.notifyStatus(ctx, req)
	}
	return Reply{Content: "Unknown subcommand."}
}

func (r *Registry) notifyChannel(ctx context.Context, req Request) Reply {
	id := req.Args["channel"]
	name, err := r.deps.Notifications.SetChannel(ctx, req.Actor, id)
	if err != nil {
		return r.notifyFail(req, err)
	}
	return Reply{Content: fmt.Sprintf("%s Alerts will be sent to <#%s> (#%s).\n%s **Everyone who can read that channel will see these alerts**, including sensor names and states, whether or not they have access to those sensors in NeptHass. Use a private channel if that matters.\nSend a test with `/notifications test`.",
		ui.IconOK, id, ui.Escape(name), ui.IconWarn)}
}

func (r *Registry) notifyAdd(ctx context.Context, req Request) Reply {
	if mon := r.deps.Monitoring; mon != nil && !mon.Status().Synchronized {
		return Reply{Content: ui.IconWarn + " Monitoring has not synchronized with Home Assistant yet, so sensors cannot be checked. Wait a few seconds and try again (see /health)."}
	}
	nr := notify.NewRule{Entity: entities.ID(req.Args["entity"]), Kind: notify.KindState}
	above, hasAbove, err := optFloat(req.Args, "above")
	if err != nil {
		return Reply{Content: ui.IconFail + " " + err.Error()}
	}
	below, hasBelow, err := optFloat(req.Args, "below")
	if err != nil {
		return Reply{Content: ui.IconFail + " " + err.Error()}
	}
	switch {
	case hasAbove && hasBelow:
		return Reply{Content: ui.IconFail + " Choose either `above` or `below`, not both. Add two rules for a sensor that needs both."}
	case hasAbove:
		nr.Kind, nr.Threshold = notify.KindAbove, &above
	case hasBelow:
		nr.Kind, nr.Threshold = notify.KindBelow, &below
	}
	if raw, ok := req.Args["cooldown"]; ok {
		minutes, perr := strconv.Atoi(raw)
		if perr != nil || minutes < 0 || minutes > maxCooldownMinutes {
			return Reply{Content: fmt.Sprintf("%s Cooldown must be between 0 and %d minutes.", ui.IconFail, maxCooldownMinutes)}
		}
		d := time.Duration(minutes) * time.Minute
		nr.Cooldown = &d
	}
	item, err := r.deps.Notifications.AddRule(ctx, req.Actor, nr)
	if err != nil {
		return r.notifyFail(req, err)
	}
	return Reply{Content: fmt.Sprintf("%s Rule #%d added: **%s** %s. Cooldown: %s.", ui.IconOK, item.ID, ui.Escape(item.Name), describeRule(item), cooldownText(item.Cooldown))}
}

func (r *Registry) notifyList(ctx context.Context, req Request) Reply {
	items, err := r.deps.Notifications.Rules(ctx, req.Actor)
	if err != nil {
		return r.notifyFail(req, err)
	}
	if len(items) == 0 {
		return Reply{Content: "There are no notification rules yet. Add one with `/notifications add`."}
	}
	var b strings.Builder
	for i, it := range items {
		if i == maxListedRules {
			fmt.Fprintf(&b, "...and %d more.", len(items)-maxListedRules)
			break
		}
		fmt.Fprintf(&b, "**#%d** %s: %s - %s\n", it.ID, ui.Escape(ui.Truncate(it.Name, 60)), describeRule(it), statusText(it.Status))
	}
	e := ui.Embed("Notification rules", b.String(), ui.ColorOK)
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

func (r *Registry) notifyToggle(ctx context.Context, req Request, enable bool) Reply {
	word := map[bool]string{false: "paused", true: "resumed"}[enable]
	if raw, ok := req.Args["rule"]; ok {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Reply{Content: ui.IconFail + " That is not a rule number."}
		}
		if err := r.deps.Notifications.SetRuleEnabled(ctx, req.Actor, id, enable); err != nil {
			return r.notifyFail(req, err)
		}
		return Reply{Content: fmt.Sprintf("%s Rule #%d %s.", ui.IconOK, id, word)}
	}
	if err := r.deps.Notifications.SetPaused(ctx, req.Actor, !enable); err != nil {
		return r.notifyFail(req, err)
	}
	return Reply{Content: fmt.Sprintf("%s All notifications %s.", ui.IconOK, word)}
}

func (r *Registry) notifyRemove(ctx context.Context, req Request) Reply {
	id, err := strconv.ParseInt(req.Args["rule"], 10, 64)
	if err != nil {
		return Reply{Content: ui.IconFail + " That is not a rule number."}
	}
	if err := r.deps.Notifications.RemoveRule(ctx, req.Actor, id); err != nil {
		return r.notifyFail(req, err)
	}
	return Reply{Content: fmt.Sprintf("%s Rule #%d deleted.", ui.IconOK, id)}
}

func (r *Registry) notifyStatus(ctx context.Context, req Request) Reply {
	st, err := r.deps.Notifications.Status(ctx, req.Actor)
	if err != nil {
		return r.notifyFail(req, err)
	}
	state := ui.IconDisabled + " Not configured (set a channel with /notifications channel)"
	switch {
	case st.Config.Active():
		state = ui.IconOK + " Enabled"
	case st.Config.Configured:
		state = ui.IconDisabled + " Paused"
	}
	channel := "none"
	if st.Config.Configured {
		channel = "<#" + st.Config.Settings.ChannelID + ">"
	}
	last := "no successful delivery yet"
	if !st.LastDelivery.IsZero() {
		last = "last successful delivery " + ui.ClockTime(st.LastDelivery)
	}
	if st.LastFailure != "" {
		last += fmt.Sprintf("\nlast failure: %s %s", st.LastFailure, ui.ClockTime(st.LastFailureAt))
	}
	h := st.Health
	health := ui.IconOK + " Healthy"
	if h.Degraded {
		health = ui.IconWarn + " Degraded: " + strings.Join(h.Reasons, "; ")
	}
	if h.BreakerOpen {
		health += "\nThe circuit breaker is open: delivery to the channel is failing (check the bot's access to it)."
	}
	e := ui.Embed("Notifications", "", ui.ColorOK)
	if h.Degraded || h.BreakerOpen {
		e.Color = ui.ColorWarn
	}
	e.Fields = []*discordgo.MessageEmbedField{
		ui.Field("Status", state, true),
		ui.Field("Channel", channel, true),
		ui.Field("Rules", fmt.Sprintf("%d active, %d paused, %d need sharing, %d not monitored, %d incompatible", st.Rules[notify.StatusActive],
			st.Rules[notify.StatusPaused], st.Rules[notify.StatusNeedsSharing], st.Rules[notify.StatusNotMonitored], st.Rules[notify.StatusIncompatible]), false),
		ui.Field("Delivery", last, false),
		ui.Field("Queue", fmt.Sprintf("critical %d, normal %d, waiting to be evaluated %d\nsent %d, retried %d, muted by cooldown %d",
			h.QueueCritical, h.QueueNormal, h.MailboxPending, h.Delivered, h.Retried, h.SuppressedCooldown), false),
		ui.Field("Dropped", fmt.Sprintf("overflow %d, expired %d, channel failure %d, circuit breaker %d, revoked %d",
			h.DroppedOverflow, h.DroppedTTL, h.DroppedPermanent, h.DroppedBreaker, h.DroppedRevoked), false),
		ui.Field("Health", health, false),
		ui.Field("Monitoring-down alert", downStateText(h.DownState), false),
	}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

func downStateText(s string) string {
	switch s {
	case "down":
		return "monitoring is unhealthy; an alert follows after five minutes"
	case "down_alerted":
		return "an alert was sent; waiting for monitoring to recover"
	case "recovered", "healthy":
		return "monitoring is healthy"
	case "stopped":
		return "stopped (shutting down)"
	}
	return "starting"
}

func (r *Registry) notifyFail(req Request, err error) Reply {
	var ue *UserError
	switch {
	case errors.As(err, &ue):
		return Reply{Content: ui.IconFail + " " + ue.Msg}
	case errors.Is(err, auth.ErrAdminRequired), errors.Is(err, auth.ErrNotAuthorized):
		return r.fail(req, err)
	case errors.Is(err, notify.ErrNotConfigured):
		return Reply{Content: ui.IconWarn + " Notifications are not configured yet. Choose a channel first with `/notifications channel`."}
	case errors.Is(err, notify.ErrRuleLimit):
		return Reply{Content: fmt.Sprintf("%s You already have the maximum of %d rules. Delete one first.", ui.IconFail, notify.MaxRules)}
	case errors.Is(err, notify.ErrRuleExists):
		return Reply{Content: ui.IconFail + " That sensor already has a rule of that kind. Delete it first, or choose a different kind."}
	case errors.Is(err, notify.ErrRuleNotFound):
		return Reply{Content: ui.IconFail + " There is no rule with that number. See `/notifications list`."}
	case errors.Is(err, notify.ErrNotShared):
		return Reply{Content: ui.IconDenied + " That sensor is not shared with NeptHass users. Share it with `/permissions allow` first."}
	case errors.Is(err, notify.ErrNotMonitored):
		return Reply{Content: ui.IconWarn + " That sensor is not being monitored right now. Monitoring may still be starting (see /health), or it is not a supported sensor type."}
	case errors.Is(err, notify.ErrIncompatible):
		return Reply{Content: ui.IconFail + " That kind of rule does not fit this sensor: door, window, motion, leak and battery-low sensors take no threshold; temperature, humidity and battery sensors need `above` or `below`."}
	case errors.Is(err, notify.ErrInvalidThreshold):
		return Reply{Content: fmt.Sprintf("%s The threshold must be a finite number between -%d and %d, and only numeric sensors take one.", ui.IconFail, notify.MaxThreshold, notify.MaxThreshold)}
	case errors.Is(err, notify.ErrInvalidCooldown):
		return Reply{Content: fmt.Sprintf("%s Cooldown must be between 0 and %d minutes.", ui.IconFail, maxCooldownMinutes)}
	case errors.Is(err, notify.ErrInvalidEntity), errors.Is(err, notify.ErrInvalidKind), errors.Is(err, notify.ErrInvalidChannel):
		return Reply{Content: ui.IconFail + " That value is not valid."}
	}
	r.deps.Logger.Error("notifications command failed", "command", req.Subcommand, "error", err)
	return Reply{Content: ui.IconFail + " Notifications could not be changed. See the NeptHass logs."}
}

func optFloat(args map[string]string, name string) (float64, bool, error) {
	raw, ok := args[name]
	if !ok {
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false, fmt.Errorf("`%s` is not a number", name)
	}
	return v, true, nil
}

func describeRule(it RuleItem) string {
	switch it.Kind {
	case notify.KindAbove:
		return "rises above " + formatThreshold(it.Threshold)
	case notify.KindBelow:
		return "falls below " + formatThreshold(it.Threshold)
	}
	switch it.Category {
	case monitor.CategoryMotion:
		return "when motion is detected"
	case monitor.CategoryLeak:
		return "when it gets wet"
	case monitor.CategoryBatteryLow:
		return "when its battery is low"
	case monitor.CategoryDoor, monitor.CategoryWindow:
		return "when it opens"
	}
	return "when it changes state"
}

func formatThreshold(t *float64) string {
	if t == nil {
		return "?"
	}
	return strconv.FormatFloat(*t, 'g', -1, 64)
}

func cooldownText(d *time.Duration) string {
	switch {
	case d == nil:
		return "the default for this sensor type"
	case *d == 0:
		return "none"
	}
	return fmt.Sprintf("%d min", int(*d/time.Minute))
}

func statusText(s notify.RuleStatus) string {
	switch s {
	case notify.StatusActive:
		return "active"
	case notify.StatusPaused:
		return "paused"
	case notify.StatusNeedsSharing:
		return "needs sharing (not shared, so it is not sending)"
	case notify.StatusNotMonitored:
		return "not monitored right now"
	case notify.StatusIncompatible:
		return "incompatible with the sensor type"
	}
	return string(s)
}

const maxAutocompletePages = 10

// completeNotifications offers only sensors the administrator may see (shared
// and currently monitored, through the same Reader as /sensors) and only for
// the administrator: the registry never calls this for anyone else.
func (r *Registry) completeNotifications(ctx context.Context, req Request) []*discordgo.ApplicationCommandOptionChoice {
	query := strings.ToLower(req.Args[req.Focused])
	switch req.Focused {
	case "entity":
		mon := r.deps.Monitoring
		if mon == nil {
			return nil
		}
		var out []*discordgo.ApplicationCommandOptionChoice
		for page := 1; page <= maxAutocompletePages && len(out) < 25; page++ {
			p, err := mon.Sensors(ctx, req.Actor, page)
			if err != nil {
				return nil
			}
			for _, v := range p.Views {
				label := sensorLabel(v)
				if len(v.ID) > ui.MaxChoice || (query != "" && !strings.Contains(strings.ToLower(label+" "+string(v.ID)), query)) {
					continue
				}
				out = append(out, ui.Choice(ui.Truncate(label+" ("+v.Category.Label()+")", 100), string(v.ID)))
				if len(out) == 25 {
					break
				}
			}
			if page >= p.Pages {
				break
			}
		}
		return out
	case "rule":
		items, err := r.deps.Notifications.Rules(ctx, req.Actor)
		if err != nil {
			return nil
		}
		var out []*discordgo.ApplicationCommandOptionChoice
		for _, it := range items {
			text := fmt.Sprintf("#%d %s - %s (%s)", it.ID, it.Name, describeRule(it), statusText(it.Status))
			if query != "" && !strings.Contains(strings.ToLower(text), query) {
				continue
			}
			out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: ui.Truncate(text, 100), Value: it.ID})
			if len(out) == 25 {
				break
			}
		}
		return out
	}
	return nil
}

// sensorLabel names a sensor for the administrator: alias, else Home Assistant's
// friendly name, else a generic label; never the entity ID.
func sensorLabel(v monitor.View) string {
	switch {
	case v.Alias != "":
		return v.Alias
	case v.Name != "":
		return v.Name
	}
	return "Unnamed sensor"
}
