package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/entities"
)

const listShown = 8

func (r *Registry) cmdSetup() command {
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "setup", Description: "Choose which Home Assistant entities are shared, step by step (administrators)", Contexts: guildOnly,
		},
		min: auth.LevelAdmin,
		run: func(ctx context.Context, req Request) Reply {
			if r.deps.Setup == nil {
				return Reply{Content: ui.IconFail + " Setup is not available in this deployment."}
			}
			return r.deps.Setup.Start(ctx, req.Actor, req.GuildID)
		},
	}
}

func (r *Registry) cmdSync() command {
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "sync", Description: "Refresh NeptHass's view of Home Assistant's entities (administrators)", Contexts: guildOnly,
		},
		min: auth.LevelAdmin,
		run: r.sync,
	}
}

func (r *Registry) sync(ctx context.Context, req Request) Reply {
	report, err := r.deps.Discovery.Sync(ctx)
	if err != nil {
		return r.fail(req, err)
	}
	color := ui.ColorOK
	if len(report.SharedMissing) > 0 || len(report.SharedUnavailable) > 0 {
		color = ui.ColorWarn
	}
	desc := fmt.Sprintf("Home Assistant reports **%d** %s (%d can be controlled: lights and switches).", report.Total, ui.Entity(report.Total), report.Controllable)
	if report.FirstSync {
		desc += "\nThis is the first synchronization: the current entities were recorded as the baseline, so none are reported as new."
	}
	if report.Skipped > 0 {
		desc += fmt.Sprintf("\n%d %s cannot be offered in the setup wizard: the ID is longer than 100 characters.", report.Skipped, ui.Entity(report.Skipped))
	}
	e := ui.Embed("Sync", desc, color)
	add := func(name string, ids []entities.ID, empty string) {
		if len(ids) == 0 && empty == "" {
			return
		}
		value := empty
		if len(ids) > 0 {
			value = idList(ids)
		}
		e.Fields = append(e.Fields, ui.Field(fmt.Sprintf("%s (%d)", name, len(ids)), value, false))
	}
	if !report.FirstSync {
		add("New since the last sync", report.New, "None")
		add("Removed since the last sync", report.Removed, "None")
		add("Returned", report.Returned, "")
	}
	add(ui.IconWarn+" Shared but no longer in Home Assistant", report.SharedMissing, "")
	add(ui.IconWarn+" Shared but unavailable or unknown", report.SharedUnavailable, "")
	e.Footer = &discordgo.MessageEmbedFooter{Text: "Nothing was shared or changed. New entities stay private until you share them with /setup."}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

// idList renders up to listShown entity IDs and a count of the rest.
func idList(ids []entities.ID) string {
	var b strings.Builder
	for i, id := range ids {
		if i == listShown {
			fmt.Fprintf(&b, "... and %d more", len(ids)-listShown)
			break
		}
		fmt.Fprintf(&b, "`%s`\n", id)
	}
	return strings.TrimSpace(b.String())
}
