package commands

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
	"github.com/nepthass/nepthass/internal/monitor"
)

const probeTimeout = 8 * time.Second

func (r *Registry) cmdPing() command {
	return command{
		def: &discordgo.ApplicationCommand{Name: "ping", Description: "Check that NeptHass is responding", Contexts: guildOnly},
		min: auth.LevelUser,
		run: func(_ context.Context, _ Request) Reply {
			return Reply{Content: fmt.Sprintf("Pong! Discord gateway latency: %d ms.", r.deps.Gateway().Latency.Milliseconds())}
		},
	}
}

func (r *Registry) cmdHealth() command {
	return command{
		def: &discordgo.ApplicationCommand{Name: "health", Description: "Show Discord, Home Assistant and database health", Contexts: guildOnly},
		min: auth.LevelUser,
		run: r.health,
	}
}

// HealthReport is the result of probing every subsystem.
type HealthReport struct {
	Gateway   Gateway
	HAVersion string
	HAErr     error
	DBErr     error
	// Monitoring is the WebSocket monitoring status (nil when not running).
	// It is reported but does not make the overall check fail: REST commands
	// and setup work without it.
	Monitoring *monitor.Status
}

// OK reports whether every probed subsystem is healthy.
func (h HealthReport) OK() bool { return h.Gateway.Connected && h.HAErr == nil && h.DBErr == nil }

// Probe checks Discord, Home Assistant and the database with a timeout.
func (r *Registry) Probe(ctx context.Context) HealthReport {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	info, haErr := r.deps.HA.Info(ctx)
	rep := HealthReport{Gateway: r.deps.Gateway(), HAVersion: info.Version, HAErr: haErr, DBErr: r.deps.DB.PingContext(ctx)}
	if r.deps.Monitoring != nil {
		st := r.deps.Monitoring.Status()
		rep.Monitoring = &st
	}
	return rep
}

func (r *Registry) health(ctx context.Context, _ Request) Reply {
	rep := r.Probe(ctx)
	check := func(err error, okText string) string {
		if err == nil {
			return ui.IconOK + " " + okText
		}
		return ui.IconFail + " " + ProbeText(err)
	}
	gatewayText := ui.IconOK + fmt.Sprintf(" Connected (%d ms)", rep.Gateway.Latency.Milliseconds())
	if !rep.Gateway.Connected {
		gatewayText = ui.IconFail + " Not connected"
	}
	color := ui.ColorOK
	if !rep.OK() || (rep.Monitoring != nil && !rep.Monitoring.Healthy) {
		color = ui.ColorWarn
	}
	e := ui.Embed("Health", "", color)
	e.Fields = []*discordgo.MessageEmbedField{
		ui.Field("Discord gateway", gatewayText, false),
		ui.Field("Home Assistant REST API", check(rep.HAErr, "Connected (Home Assistant "+ui.Escape(rep.HAVersion)+")"), false),
		ui.Field("Home Assistant WebSocket (live monitoring)", MonitoringText(rep.Monitoring), false),
		ui.Field("Database", check(rep.DBErr, "Database is responding"), false),
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "NeptHass " + r.deps.Version}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

// ProbeText describes a probe failure without leaking internal details.
func ProbeText(err error) string {
	return describeProbeError(err)
}

func describeProbeError(err error) string {
	switch {
	case errors.Is(err, rest.ErrUnauthorized):
		return "Access token rejected"
	case errors.Is(err, rest.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		return "Unreachable"
	default:
		return "Check failed (see the NeptHass log)"
	}
}
