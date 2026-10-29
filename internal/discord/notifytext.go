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
			fmt.Fprintf(&b, "\n- **%s**: %s", name(l.Name), stateClause(l.Trigger, l.Meaning, l.Value, l.HasValue, l.Unit, l.Threshold))
			if !l.ChangedAt.IsZero() {
				fmt.Fprintf(&b, " (since %s)", ui.ClockTime(l.ChangedAt))
			}
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
	fmt.Fprintf(&b, "%s **%s** %s", ui.IconWarn, name(m.Name), stateClause(m.Trigger, m.Meaning, m.Value, m.HasValue, m.Unit, m.Threshold))
	if !m.ObservedAt.IsZero() {
		fmt.Fprintf(&b, " at %s", ui.ClockTime(m.ObservedAt))
	}
	b.WriteString(".")
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

func stateClause(kind notify.Kind, meaning monitor.Meaning, value float64, hasValue bool, unit string, threshold *float64) string {
	switch kind {
	case notify.KindAbove:
		return "rose above " + num(threshold, unit) + now(value, hasValue, unit)
	case notify.KindBelow:
		return "fell below " + num(threshold, unit) + now(value, hasValue, unit)
	}
	switch meaning {
	case monitor.MeaningOpen:
		return "is now open"
	case monitor.MeaningDetected:
		return "detected motion"
	case monitor.MeaningWet:
		return "is wet (water detected)"
	case monitor.MeaningLow:
		return "reports a low battery"
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
