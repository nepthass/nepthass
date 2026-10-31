package discord

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

// maxMessage keeps a message well inside Discord's 2000 character limit.
const maxMessage = 1800

// FormatMessage renders a transport-neutral notification as Discord text. It
// prints only what the message carries (a name, a state, a value, times): never
// an entity ID, an attribute or a raw payload. Sensor names are escaped, and the
// send also uses an empty allowed-mentions list, so a hostile friendly name can
// neither format the message nor ping anyone.
func FormatMessage(m notify.Message) string {
	var s string
	switch m.Kind {
	case notify.MsgAlert:
		s = formatAlert(m)
	case notify.MsgMonitoringDown:
		s = fmt.Sprintf("%s **Live monitoring of Home Assistant has been down for %d minutes** (since %s).%s\nAlerts about changes are paused until it recovers.",
			ui.IconWarn, minutes(m.DownFor), ui.ClockTime(m.DownSince), failureHint(m.Failure))
	case notify.MsgMonitoringRecovered:
		s = fmt.Sprintf("%s Live monitoring of Home Assistant is back after %d minutes.", ui.IconOK, minutes(m.DownFor))
	case notify.MsgDigest:
		var b strings.Builder
		fmt.Fprintf(&b, "%s Some alerts could not be delivered earlier. **Current state** of the affected sensors (what happened in between is not known):", ui.IconWarn)
		for _, l := range m.Lines {
			fmt.Fprintf(&b, "\n- **%s**: %s", name(l.Name), stateClause(l.Category, l.Trigger, l.Meaning, l.Value, l.HasValue, l.Unit, l.Threshold))
			if !l.ChangedAt.IsZero() {
				fmt.Fprintf(&b, " (since %s)", ui.ClockTime(l.ChangedAt))
			}
		}
		if m.MoreLines > 0 {
			fmt.Fprintf(&b, "\n- and %d more", m.MoreLines)
		}
		s = b.String()
	case notify.MsgStartup:
		var b strings.Builder
		fmt.Fprintf(&b, "%s NeptHass started and found these sensors already in an alert state, as Home Assistant reports them. NeptHass cannot tell when they changed, and a device that sleeps may be showing its last known state:", ui.IconWarn)
		for _, l := range m.Lines {
			fmt.Fprintf(&b, "\n- **%s**: %s", name(l.Name), stateClause(l.Category, l.Trigger, l.Meaning, l.Value, l.HasValue, l.Unit, l.Threshold))
		}
		if m.MoreLines > 0 {
			fmt.Fprintf(&b, "\n- and %d more", m.MoreLines)
		}
		s = b.String()
	default:
		s = "NeptHass notification."
	}
	return ui.Truncate(s, maxMessage)
}

func formatAlert(m notify.Message) string {
	var b strings.Builder
	switch {
	case m.Initial:
		// Found already present when NeptHass started: say what Home Assistant reports and what NeptHass
		// does not know, so it is never mistaken for a new event.
		fmt.Fprintf(&b, "%s **%s** %s, according to Home Assistant.", ui.IconWarn, name(m.Name), stateClause(m.Category, m.Trigger, m.Meaning, m.Value, m.HasValue, m.Unit, m.Threshold))
		b.WriteString("\nNeptHass found it like this when it started, so this is not a new event and NeptHass cannot tell when it began.")
		if !m.ReportedAt.IsZero() {
			fmt.Fprintf(&b, " Home Assistant last updated it at %s; a device that sleeps may be reporting an old state.", ui.ClockTime(m.ReportedAt))
		}
		b.WriteString(" Please check it.")
	case m.Duration > 0:
		fmt.Fprintf(&b, "%s **%s** %s", ui.IconWarn, name(m.Name), heldClause(m))
		if !m.ObservedAt.IsZero() {
			fmt.Fprintf(&b, " (as of %s)", ui.ClockTime(m.ObservedAt))
		}
		b.WriteString(".")
	default:
		fmt.Fprintf(&b, "%s **%s** %s", ui.IconWarn, name(m.Name), stateClause(m.Category, m.Trigger, m.Meaning, m.Value, m.HasValue, m.Unit, m.Threshold))
		if !m.ObservedAt.IsZero() {
			fmt.Fprintf(&b, " at %s", ui.ClockTime(m.ObservedAt))
		}
		b.WriteString(".")
	}
	if m.Late {
		b.WriteString("\nDetected after NeptHass reconnected to Home Assistant, so it may have happened earlier.")
	}
	if m.Uncertain {
		b.WriteString("\nIts state was unknown or unavailable before this, so please check it.")
	}
	if m.Cleared {
		b.WriteString("\nIt is already back to normal.")
	}
	if m.Muted > 0 {
		fmt.Fprintf(&b, "\n(%d more alert(s) for this rule were suppressed by its cooldown.)", m.Muted)
	}
	return b.String()
}

// heldClause words a duration alert: how long the condition has held. When NeptHass
// did not see it begin (a restart, a reconnect, an outage), the real time may be
// longer, so it says "at least".
func heldClause(m notify.Message) string {
	d := durationText(m.Duration)
	if m.HeldAtLeast {
		d = "at least " + d
	}
	switch m.Trigger {
	case notify.KindAbove:
		return "has been above " + num(m.Threshold, m.Unit) + " for " + d + now(m.Value, m.HasValue, m.Unit)
	case notify.KindBelow:
		return "has been below " + num(m.Threshold, m.Unit) + " for " + d + now(m.Value, m.HasValue, m.Unit)
	}
	if s, ok := monitor.SpecFor(m.Category); ok && s.Held != "" && m.Meaning == s.AlertMeaning() {
		return s.Held + " " + d
	}
	return "has been in its alert state for " + d
}

// durationText writes a duration in words: "10 minutes", "1 hour 30 minutes".
func durationText(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	var parts []string
	unit := func(n int, one, many string) {
		switch {
		case n == 1:
			parts = append(parts, "1 "+one)
		case n > 1:
			parts = append(parts, strconv.Itoa(n)+" "+many)
		}
	}
	unit(h, "hour", "hours")
	unit(m, "minute", "minutes")
	unit(sec, "second", "seconds")
	if len(parts) == 0 {
		return "0 seconds"
	}
	return strings.Join(parts, " ")
}

func stateClause(cat monitor.Category, kind notify.Kind, meaning monitor.Meaning, value float64, hasValue bool, unit string, threshold *float64) string {
	switch kind {
	case notify.KindAbove:
		return "rose above " + num(threshold, unit) + now(value, hasValue, unit)
	case notify.KindBelow:
		return "fell below " + num(threshold, unit) + now(value, hasValue, unit)
	}
	if s, ok := monitor.SpecFor(cat); ok && s.Alert != "" && meaning == s.AlertMeaning() {
		return s.Alert
	}
	return "changed state"
}

func num(t *float64, unit string) string {
	if t == nil {
		return "the threshold"
	}
	return withUnit(*t, unit)
}

func now(v float64, has bool, unit string) string {
	if !has {
		return ""
	}
	return " (now " + withUnit(v, unit) + ")"
}

func withUnit(v float64, unit string) string {
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if unit = ui.Escape(ui.Truncate(unit, 12)); unit != "" {
		return s + " " + unit
	}
	return s
}

func name(n string) string { return ui.Escape(ui.Truncate(n, 80)) }

func minutes(d time.Duration) int {
	if m := int(d.Round(time.Minute) / time.Minute); m > 0 {
		return m
	}
	return 1
}

func failureHint(f monitor.Failure) string {
	switch f {
	case monitor.FailureNetwork:
		return " Last problem: Home Assistant could not be reached."
	case monitor.FailureAuthentication:
		return " Last problem: Home Assistant rejected NeptHass's access token."
	case monitor.FailureProtocol:
		return " Last problem: Home Assistant sent an unexpected response."
	}
	return ""
}
