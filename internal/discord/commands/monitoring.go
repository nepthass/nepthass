package commands

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/monitor"
)

// Monitoring is the live-monitoring view the commands need; *monitor.Reader
// implements it.
type Monitoring interface {
	Status() monitor.Status
	Sensors(ctx context.Context, actor auth.Actor, page int) (monitor.Page, error)
}

func (r *Registry) cmdSensors() command {
	minPage := 1.0
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "sensors", Description: "Live readings of the sensors shared with you", Contexts: guildOnly,
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionInteger, Name: "page", Description: "Page number", MinValue: &minPage,
			}},
		},
		min: auth.LevelUser,
		run: r.sensors,
	}
}

func (r *Registry) sensors(ctx context.Context, req Request) Reply {
	if r.deps.Monitoring == nil {
		return Reply{Content: ui.IconDisabled + " Live monitoring is not running in this deployment."}
	}
	page, _ := strconv.Atoi(req.Args["page"])
	res, err := r.deps.Monitoring.Sensors(ctx, req.Actor, page)
	if err != nil {
		return r.fail(req, err)
	}
	if res.Total == 0 && res.Status.LastSyncAt.IsZero() {
		// The tracker is empty only because monitoring has not loaded anything yet.
		// That says nothing about what is shared, so do not claim nothing is.
		return Reply{Content: notSynchronizedText(res.Status)}
	}
	if res.Total == 0 {
		return Reply{Content: "No monitored sensors are shared with you. An administrator can share a temperature, humidity, battery, motion, door, window or water-leak sensor with `/setup` or `/permissions allow`."}
	}
	desc, color := "", ui.ColorOK
	if !isLive(res.Status) {
		desc = ui.IconWarn + " " + staleBanner(res.Status) + "\n\n"
		color = ui.ColorWarn
	}
	for _, v := range res.Views {
		desc += sensorLine(v) + "\n"
	}
	e := ui.Embed("Sensors", desc, color)
	e.Footer = &discordgo.MessageEmbedFooter{Text: sensorsFooter(res)}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

// isLive is the single definition of "these readings are live": the monitor is
// healthy and its cache was synchronized. The banner and the footer both use it
// so they can never disagree.
func isLive(st monitor.Status) bool { return st.Healthy && st.Synchronized }

// sensorsFooter calls the readings "live" only while monitoring is healthy and
// synchronized. Otherwise the values are last known ones, and saying "live"
// next to the stale banner would contradict it.
func sensorsFooter(res monitor.Page) string {
	source := "Last known values, not live (monitoring is not healthy)."
	if isLive(res.Status) {
		source = "Live readings from Home Assistant's WebSocket."
	}
	return fmt.Sprintf("Page %d of %d. %s Use /status for an authoritative read of one entity.", res.Page, res.Pages, source)
}

// notSynchronizedText explains why there is nothing to show before the first
// synchronization. It deliberately says nothing about what is or is not shared
// with the caller: until Home Assistant has been read, that is unknown.
func notSynchronizedText(st monitor.Status) string {
	const suffix = " Sensor readings are not available yet."
	switch {
	case st.State == monitor.StateStopped || st.State == monitor.StateDisconnected:
		return ui.IconDisabled + " Live monitoring is not running." + suffix
	case st.LastFailure == monitor.FailureAuthentication:
		return ui.IconFail + " Live monitoring cannot sign in to Home Assistant's WebSocket (the access token was rejected; an administrator should check HA_TOKEN)." + suffix
	case st.LastFailure != monitor.FailureNone:
		return ui.IconWarn + " Live monitoring has not reached Home Assistant's WebSocket yet and keeps retrying." + suffix + " Try again in a moment."
	default:
		return "\u23F3 Live monitoring is still starting and has not loaded the sensors yet." + suffix + " Try again in a few seconds."
	}
}

func staleBanner(st monitor.Status) string {
	switch st.State {
	case monitor.StateReady:
		return "Nothing was received from Home Assistant recently, so these are the last known values and may be outdated."
	case monitor.StateDisconnected, monitor.StateStopped:
		return "Live monitoring is not running, so these are the last known values and may be outdated."
	default:
		return "Live monitoring is not connected right now, so these are the last known values and may be outdated."
	}
}

// sensorLine renders one sensor: the interpreted meaning first, with the raw
// state alongside for binary sensors. Unavailable, unknown and invalid values
// and stale data get a warning icon and are never shown as a safe state.
func sensorLine(v monitor.View) string {
	in := v.Interpretation()
	icon := ui.IconOK
	if v.Stale || !in.Meaning.Reliable() {
		icon = ui.IconWarn
	}
	return fmt.Sprintf("%s **%s** `%s`\n\u2003%s: %s \u00b7 changed %s",
		icon, ui.Escape(v.DisplayName()), v.ID, v.Category.Label(), valueText(v, in), ui.Relative(v.LastChanged))
}

func valueText(v monitor.View, in monitor.Interpretation) string {
	switch {
	case in.Meaning == monitor.MeaningReading:
		return ui.Escape(joinUnit(v.Raw, v.Unit))
	case in.Meaning == monitor.MeaningInvalid:
		return "invalid value (reported: " + ui.Escape(ui.Truncate(v.Raw, 40)) + ")"
	case !in.Meaning.Reliable():
		return in.Meaning.Text(v.Category)
	default:
		return in.Meaning.Text(v.Category) + " (" + ui.Escape(v.Raw) + ")"
	}
}

func joinUnit(raw, unit string) string {
	if unit == "" {
		return raw
	}
	return raw + " " + unit
}

// MonitoringText describes monitoring health for /health and the setup wizard.
// It lists the layers separately so a connected but unsynchronized monitor is
// never presented as healthy, and it never includes URLs, tokens or error text.
func MonitoringText(st *monitor.Status) string {
	if st == nil {
		return ui.IconDisabled + " Not running"
	}
	tick := func(ok bool) string {
		if ok {
			return ui.IconOK
		}
		return ui.IconFail
	}
	layers := fmt.Sprintf("Transport %s \u00b7 Authenticated %s \u00b7 Subscribed %s \u00b7 Snapshot %s",
		tick(st.Transport), tick(st.Authenticated), tick(st.Subscribed), tick(st.Synchronized))
	out := monitoringHead(st) + "\n" + layers
	if st.Reconnects > 0 {
		out += fmt.Sprintf("\nReconnects: %d", st.Reconnects)
	}
	if st.InvalidMessages > 0 {
		out += fmt.Sprintf(" \u00b7 Invalid messages ignored: %d", st.InvalidMessages)
	}
	return out
}

func monitoringHead(st *monitor.Status) string {
	switch {
	case st.Healthy:
		head := fmt.Sprintf("%s Healthy: %d %s tracked, synchronized %s", ui.IconOK, st.Tracked, sensorNoun(st.Tracked), ui.Relative(st.LastSyncAt))
		if st.LastEventAt.IsZero() {
			return head + ", no events yet"
		}
		return head + ", last event " + ui.Relative(st.LastEventAt)
	case st.State == monitor.StateReady:
		return ui.IconWarn + " Connected but silent: nothing was received recently, so values may be outdated"
	case st.State == monitor.StateReconnecting:
		head := failureText(st.LastFailure)
		if !st.NextRetryAt.IsZero() {
			// A clock time, not a relative one: this message stays on screen and a
			// relative timestamp would turn "in 24 seconds" into "24 seconds ago".
			head += ", next attempt at " + ui.ClockTime(st.NextRetryAt)
		}
		return head + outdatedNote(st)
	case st.State == monitor.StateStopped:
		return ui.IconDisabled + " Stopped"
	case st.State == monitor.StateDisconnected:
		return ui.IconDisabled + " Not started yet"
	default: // connecting, authenticating, subscribing or synchronizing
		return startingHead(st) + outdatedNote(st)
	}
}

// startingHead describes a connection attempt in progress. After an earlier
// failure it is a reconnection, never "Starting": the first start and every
// later retry are different situations for the person reading it.
func startingHead(st *monitor.Status) string {
	stage := string(st.State)
	switch {
	case st.Reconnects == 0 && st.LastFailure == monitor.FailureNone:
		return "\u23F3 Starting (" + stage + ")"
	case st.State == monitor.StateConnecting && st.LastFailure == monitor.FailureAuthentication:
		return failureText(monitor.FailureAuthentication) + " (retrying now)"
	case st.State == monitor.StateConnecting:
		reason := "network problem"
		if st.LastFailure == monitor.FailureProtocol {
			reason = "unexpected response"
		}
		return ui.IconWarn + " Reconnecting to Home Assistant's WebSocket (the previous attempt failed: " + reason + ")"
	default:
		return "\u23F3 Reconnecting (" + stage + ")"
	}
}

// outdatedNote warns that cached values are not live while a connection is
// being (re)established.
func outdatedNote(st *monitor.Status) string {
	if st.Tracked > 0 {
		return ". Last known values may be outdated"
	}
	return ""
}

func failureText(f monitor.Failure) string {
	switch f {
	case monitor.FailureAuthentication:
		return ui.IconFail + " Home Assistant rejected the access token for the WebSocket (check HA_TOKEN)"
	case monitor.FailureProtocol:
		return ui.IconWarn + " Unexpected response from Home Assistant's WebSocket; retrying"
	default:
		return ui.IconWarn + " Cannot reach Home Assistant's WebSocket; retrying"
	}
}

func sensorNoun(n int) string {
	if n == 1 {
		return "sensor"
	}
	return "sensors"
}
