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
// AlreadyActive is set on the item returned by AddRule when the sensor was already
// in the rule's alert state at that moment.
type RuleItem struct {
	notify.RuleView
	Name          string
	AlreadyActive bool
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
	// ConfirmUnit binds a rule created before units were tracked to its sensor's
	// current unit and returns that unit.
	ConfirmUnit(ctx context.Context, actor auth.Actor, id int64) (string, error)
	SendTest(ctx context.Context, actor auth.Actor) error
	Status(ctx context.Context, actor auth.Actor) (NotificationStatus, error)
	// HealthLine is one non-sensitive line for /health, safe for any user.
	HealthLine() string
}

const (
	maxCooldownMinutes = int(notify.MaxCooldown / time.Minute)
	minDurationMinutes = int(notify.MinDuration / time.Minute)
	maxDurationMinutes = int(notify.MaxDuration / time.Minute)
	// listBudget keeps the rule list inside the embed description limit.
	listBudget = 3600
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
					number("clear", "With above or below: the value it must pass again before it can alert again (hysteresis)"),
					&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: "for",
						Description: "Alert only after the condition has held this many minutes (not for water leaks)", MinValue: &one, MaxValue: float64(maxDurationMinutes)},
					&discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionInteger, Name: "cooldown",
						Description: "Minutes between alerts for this rule (0 for none; default depends on the sensor type)", MinValue: &zero, MaxValue: float64(maxCooldownMinutes)}),
				sub("list", "Show every rule and its status"),
				sub("pause", "Pause one rule, or all notifications when no rule is given", rule(false, "Rule to pause")),
				sub("resume", "Resume one rule, or all notifications when no rule is given", rule(false, "Rule to resume")),
				sub("remove", "Delete a rule", rule(true, "Rule to delete")),
				sub("unit", "Bind a rule created before v0.5.0 to its sensor's current unit", rule(true, "Rule to bind")),
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
	case "unit":
		return r.notifyUnit(ctx, req)
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
	if clear, has, cerr := optFloat(req.Args, "clear"); cerr != nil {
		return Reply{Content: ui.IconFail + " " + cerr.Error()}
	} else if has {
		nr.Clear = &clear
	}
	if raw, ok := req.Args["for"]; ok {
		minutes, perr := strconv.Atoi(raw)
		if perr != nil || minutes < minDurationMinutes || minutes > maxDurationMinutes {
			return Reply{Content: fmt.Sprintf("%s `for` must be between %d and %d minutes.", ui.IconFail, minDurationMinutes, maxDurationMinutes)}
		}
		nr.Duration = time.Duration(minutes) * time.Minute
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
	msg := fmt.Sprintf("%s Rule #%d added: **%s** %s. Cooldown: %s.", ui.IconOK, item.ID, ui.Escape(item.Name), describeRule(item), cooldownText(item.Cooldown))
	if item.AlreadyActive {
		switch {
		case item.Category.Hazard():
			msg += "\n" + ui.IconWarn + " **This sensor is already in that state right now.** Because it is a critical alert, one alert is on its way to the channel, worded as what Home Assistant reports."
		case item.Duration > 0:
			msg += "\n" + ui.IconWarn + " This sensor is already in that state right now. NeptHass counts the duration from now, so it cannot alert before then."
		default:
			msg += "\n" + ui.IconWarn + " This sensor is already in that state right now, so nothing is sent for it. The rule alerts the next time it enters that state."
		}
	}
	return Reply{Content: msg}
}

func (r *Registry) notifyUnit(ctx context.Context, req Request) Reply {
	id, err := strconv.ParseInt(req.Args["rule"], 10, 64)
	if err != nil {
		return Reply{Content: ui.IconFail + " That is not a rule number."}
	}
	unit, err := r.deps.Notifications.ConfirmUnit(ctx, req.Actor, id)
	if err != nil {
		return r.notifyFail(req, err)
	}
	return Reply{Content: fmt.Sprintf("%s Rule #%d is now bound to **%s**. Its threshold is read in %s from now on, and the rule is evaluating. If the threshold was written in another unit, delete the rule and add it again.", ui.IconOK, id, ui.Escape(unit), ui.Escape(unit))}
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
		line := fmt.Sprintf("**#%d** %s: %s - %s\n", it.ID, ui.Escape(ui.Truncate(it.Name, 60)), describeRule(it), statusText(it.Status))
		if b.Len()+len(line) > listBudget {
			fmt.Fprintf(&b, "...and %d more. Use `/notifications status` for the counts.", len(items)-i)
			break
		}
		b.WriteString(line)
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
		ui.Field("Rules", rulesText(st.Rules), false),
		ui.Field("Delivery", last, false),
		ui.Field("Queue", fmt.Sprintf("critical %d, normal %d, waiting to be evaluated %d\nsent %d, retried %d, muted by cooldown %d",
			h.QueueCritical, h.QueueNormal, h.MailboxPending, h.Delivered, h.Retried, h.SuppressedCooldown), false),
		ui.Field("Dropped", fmt.Sprintf("overflow %d, expired %d, channel failure %d, circuit breaker %d, revoked %d",
			h.DroppedOverflow, h.DroppedTTL, h.DroppedPermanent, h.DroppedBreaker, h.DroppedRevoked), false),
		ui.Field("Duration rules", fmt.Sprintf("%d counting now; %d alerted, %d ended without alerting", h.PendingEpisodes, h.EpisodesFired, h.EpisodesCancelled), false),
		ui.Field("Since NeptHass started", startupText(h), false),
		ui.Field("Health", health, false),
		ui.Field("Monitoring-down alert", downStateText(h.DownState), false),
	}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

// rulesText counts rules by status, naming only the statuses that occur beyond
// the always-shown ones, and explains the two unit statuses.
func rulesText(c map[notify.RuleStatus]int) string {
	s := fmt.Sprintf("%d active, %d paused, %d need sharing, %d not monitored, %d incompatible", c[notify.StatusActive],
		c[notify.StatusPaused], c[notify.StatusNeedsSharing], c[notify.StatusNotMonitored], c[notify.StatusIncompatible])
	if n := c[notify.StatusUnitUnresolved]; n > 0 {
		s += fmt.Sprintf("\n%s **%d rule(s) need their unit confirmed** and are not evaluating: they were created before units were tracked. Use `/notifications unit`.", ui.IconWarn, n)
	}
	if n := c[notify.StatusUnitChanged]; n > 0 {
		s += fmt.Sprintf("\n%s **%d rule(s) are paused because their sensor changed unit.** Delete and add them again in the new unit; they resume by themselves if the unit returns.", ui.IconWarn, n)
	}
	return s
}

// startupText summarizes what NeptHass announced when it started.
func startupText(h notify.Health) string {
	s := fmt.Sprintf("%d hazard alert(s) for conditions already present; %d sensor(s) listed in the start-up summary (%d sent, %d held back to keep to one per 10 minutes); %d hazard alert(s) skipped because the same one was already delivered",
		h.InitialAlerts, h.StartupListed, h.StartupSummaries, h.SummarySuppressed, h.MarkSuppressed)
	return s
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
		return Reply{Content: ui.IconFail + " That sensor already has a rule of that kind and duration. Delete it first, or choose a different kind or duration."}
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
	case errors.Is(err, notify.ErrHazardImmediate):
		return Reply{Content: ui.IconFail + " Water leak alerts are always immediate: they cannot be delayed or given a clear value."}
	case errors.Is(err, notify.ErrInvalidDuration):
		return Reply{Content: fmt.Sprintf("%s `for` must be between %d and %d minutes.", ui.IconFail, minDurationMinutes, maxDurationMinutes)}
	case errors.Is(err, notify.ErrDurationNotAllowed):
		return Reply{Content: ui.IconFail + " That sensor type does not support waiting for a duration."}
	case errors.Is(err, notify.ErrInvalidClear):
		return Reply{Content: ui.IconFail + " The clear value must be a finite number on the recovery side of the threshold: below it for `above`, above it for `below`."}
	case errors.Is(err, notify.ErrClearNotAllowed):
		return Reply{Content: ui.IconFail + " A clear value only applies to a numeric rule (`above` or `below`) on a sensor type that supports it."}
	case errors.Is(err, notify.ErrUnitUnsupported):
		return Reply{Content: ui.IconFail + " That sensor reports no unit, or a unit NeptHass does not support for this sensor type, so a threshold cannot be tied to it safely. Check the sensor in Home Assistant."}
	case errors.Is(err, notify.ErrUnitBound):
		return Reply{Content: ui.IconFail + " That rule is already bound to a unit. A unit is never rebound: delete the rule and add it again to use another unit."}
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
	var s string
	switch it.Kind {
	case notify.KindAbove:
		s = "rises above " + withUnit(it.Threshold, it.Unit)
	case notify.KindBelow:
		s = "falls below " + withUnit(it.Threshold, it.Unit)
	default:
		s = "when it changes state"
		if sp, ok := monitor.SpecFor(it.Category); ok && sp.Phrase != "" {
			s = sp.Phrase
		}
	}
	if it.Clear != nil {
		dir := "below"
		if it.Kind == notify.KindBelow {
			dir = "above"
		}
		s += fmt.Sprintf(" (can alert again only after it is %s %s)", dir, withUnit(it.Clear, it.Unit))
	}
	if it.Duration > 0 {
		s += ", and stays that way for " + durationWords(it.Duration)
	}
	return s
}

// durationWords writes a rule duration: "10 min", "1 h 30 min".
func durationWords(d time.Duration) string {
	m := int(d / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

func withUnit(t *float64, unit string) string {
	s := formatThreshold(t)
	if unit != "" {
		s += " " + ui.Escape(ui.Truncate(unit, 12))
	}
	return s
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
	case notify.StatusUnitUnresolved:
		return "NOT EVALUATING: created before units were tracked; confirm its unit with /notifications unit"
	case notify.StatusUnitChanged:
		return "NOT EVALUATING: the sensor now reports a different unit (delete and re-add it, or it resumes if the unit returns)"
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
				out = append(out, ui.Choice(ui.Truncate(label+" ("+sensorKind(v)+")", 100), string(v.ID)))
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
			if req.Subcommand == "unit" && it.Status != notify.StatusUnitUnresolved {
				continue // only a rule that needs its unit confirmed is offered
			}
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

// sensorKind describes what a sensor supports, so the administrator sees which
// options apply before choosing: a numeric sensor shows its unit, a water leak
// says its alerts are always immediate.
func sensorKind(v monitor.View) string {
	s := v.Category.Label()
	switch {
	case v.Category.Hazard():
		s += ", immediate alerts only"
	case v.Category.Numeric() && v.Unit != "":
		s += ", " + v.Unit
	}
	return s
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
