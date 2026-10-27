package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/entities"
)

func entityOption(description string) *discordgo.ApplicationCommandOption {
	return &discordgo.ApplicationCommandOption{
		Type: discordgo.ApplicationCommandOptionString, Name: "entity", Description: description,
		Required: true, Autocomplete: true, MaxLength: 255,
	}
}

func (r *Registry) cmdDevices() command {
	minPage := 1.0
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "devices", Description: "List the entities you may use", Contexts: guildOnly,
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionInteger, Name: "page", Description: "Page number", MinValue: &minPage,
			}},
		},
		min: auth.LevelUser,
		run: r.devices,
	}
}

func (r *Registry) devices(ctx context.Context, req Request) Reply {
	page, _ := strconv.Atoi(req.Args["page"])
	result, err := r.deps.Entities.Devices(ctx, req.Actor, page)
	if err != nil {
		return r.fail(req, err)
	}
	if result.Total == 0 {
		return Reply{Content: "No entities have been shared yet. An administrator can add some with `/permissions allow`."}
	}
	var b strings.Builder
	for _, d := range result.Devices {
		b.WriteString(deviceLine(d))
		b.WriteByte('\n')
	}
	e := ui.Embed("Devices", b.String(), ui.ColorOK)
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d of %d - %d entities", result.Page, result.Pages, result.Total)}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

func deviceLine(d entities.Device) string {
	if d.State == nil {
		if d.Alias != "" {
			return fmt.Sprintf("%s **%s** `%s` - not found in Home Assistant", ui.IconWarn, ui.Escape(d.Alias), d.ID)
		}
		return fmt.Sprintf("%s `%s` - not found in Home Assistant", ui.IconWarn, d.ID)
	}
	return fmt.Sprintf("%s **%s** `%s` - %s (%s)", ui.StateIcon(*d.State), ui.Escape(d.State.DisplayName()), d.ID, ui.DescribeState(*d.State), d.Access)
}

func (r *Registry) cmdStatus() command {
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "status", Description: "Show the state of an entity", Contexts: guildOnly,
			Options: []*discordgo.ApplicationCommandOption{entityOption("Entity to inspect")},
		},
		min:      auth.LevelUser,
		run:      r.status,
		complete: r.permitted(entities.AccessRead),
	}
}

func (r *Registry) status(ctx context.Context, req Request) Reply {
	id, err := r.deps.Entities.Resolve(ctx, req.Actor, req.Args["entity"], entities.AccessRead)
	if err != nil {
		return r.fail(req, err)
	}
	st, err := r.deps.Entities.Status(ctx, req.Actor, id)
	if err != nil {
		return r.fail(req, err)
	}
	color := ui.ColorOK
	if !st.Reliable() {
		color = ui.ColorWarn
	}
	e := ui.Embed("Status", "**"+ui.Escape(st.DisplayName())+"**", color)
	e.Fields = []*discordgo.MessageEmbedField{
		ui.Field("Entity", "`"+string(st.ID)+"`", true),
		ui.Field("State", ui.StateIcon(st)+" "+ui.DescribeState(st), true),
	}
	if st.Alias != "" {
		e.Fields = append(e.Fields, ui.Field("Home Assistant name", ui.Escape(st.FriendlyName()), true))
	}
	if dc := st.DeviceClass(); dc != "" {
		e.Fields = append(e.Fields, ui.Field("Device class", ui.Escape(dc), true))
	}
	e.Fields = append(e.Fields, ui.Field("Last changed", ui.Relative(st.LastChanged), true))
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

func (r *Registry) cmdControl() command {
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "control", Description: "Turn a light or switch on or off", Contexts: guildOnly,
			Options: []*discordgo.ApplicationCommandOption{
				entityOption("Light or switch to control"),
				{
					Type: discordgo.ApplicationCommandOptionString, Name: "action", Description: "What to do", Required: true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: "on", Value: "on"}, {Name: "off", Value: "off"}},
				},
			},
		},
		min:      auth.LevelUser,
		run:      r.control,
		complete: r.permitted(entities.AccessControl),
	}
}

func (r *Registry) control(ctx context.Context, req Request) Reply {
	id, err := r.deps.Entities.Resolve(ctx, req.Actor, req.Args["entity"], entities.AccessControl)
	if err != nil {
		return r.fail(req, err)
	}
	action, err := entities.ParseAction(req.Args["action"])
	if err != nil {
		return r.fail(req, err)
	}
	res, err := r.deps.Entities.Control(ctx, req.Actor, id, action)
	if err != nil {
		return r.fail(req, err)
	}
	stateText := "State: " + ui.DescribeState(res.Before)
	if res.After != nil && res.After.State != res.Before.State {
		stateText = "State: " + ui.DescribeState(res.Before) + " \u2192 " + ui.DescribeState(*res.After)
	} else {
		stateText += " (Home Assistant has not reported a change yet; devices can take a moment)"
	}
	e := ui.Embed("Command Sent", fmt.Sprintf("Turned **%s** `%s`.\n%s", action, id, stateText), ui.ColorOK)
	e.Fields = []*discordgo.MessageEmbedField{ui.Field("Entity", ui.Escape(res.Before.DisplayName()), false)}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

// permitted autocompletes the "entity" option from the allowlist.
func (r *Registry) permitted(minimum entities.Access) completer {
	return func(ctx context.Context, req Request) []*discordgo.ApplicationCommandOptionChoice {
		if req.Focused != "entity" {
			return nil
		}
		suggestions, err := r.deps.Entities.Suggest(ctx, req.Actor, minimum, req.Args["entity"])
		if err != nil {
			r.deps.Logger.Debug("autocomplete failed", "command", req.Command, "error", err)
			return nil
		}
		choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(suggestions))
		for _, s := range suggestions {
			label := string(s.ID)
			if s.Alias != "" {
				label = s.Alias + " (" + string(s.ID) + ")"
			}
			// The submitted value is always the canonical entity ID.
			choices = append(choices, ui.Choice(label, string(s.ID)))
		}
		return choices
	}
}
