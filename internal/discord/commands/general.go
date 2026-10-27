package commands

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
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

func (r *Registry) health(ctx context.Context, _ Request) Reply {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	allOK := true
	check := func(err error, okText string) string {
		if err == nil {
			return iconOK + " " + okText
		}
		allOK = false
		return iconFail + " " + describeProbeError(err)
	}

	gateway := r.deps.Gateway()
	gatewayText := iconOK + fmt.Sprintf(" Connected (%d ms)", gateway.Latency.Milliseconds())
	if !gateway.Connected {
		allOK = false
		gatewayText = iconFail + " Not connected"
	}
	info, haErr := r.deps.HA.Info(ctx)
	haText := check(haErr, "Connected (Home Assistant "+escape(info.Version)+")")
	dbText := check(r.deps.DB.PingContext(ctx), "Database is responding")

	color := colorOK
	if !allOK {
		color = colorWarn
	}
	e := newEmbed("Health", "", color)
	e.Fields = []*discordgo.MessageEmbedField{
		field("Discord gateway", gatewayText, false),
		field("Home Assistant REST API", haText, false),
		field("Home Assistant WebSocket", iconDisabled+" Not available in this release (planned for v0.3.0)", false),
		field("Database", dbText, false),
	}
	e.Footer = &discordgo.MessageEmbedFooter{Text: "NeptHass " + r.deps.Version}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
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
