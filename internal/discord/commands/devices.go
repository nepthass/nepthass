package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
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
	e := newEmbed("Devices", b.String(), colorOK)
	e.Footer = &discordgo.MessageEmbedFooter{Text: fmt.Sprintf("Page %d of %d - %d entities", result.Page, result.Pages, result.Total)}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

func deviceLine(d entities.Device) string {
	if d.State == nil {
		return fmt.Sprintf("%s `%s` - not found in Home Assistant", iconWarn, d.ID)
	}
	return fmt.Sprintf("%s **%s** `%s` - %s (%s)", stateIcon(*d.State), escape(d.State.FriendlyName()), d.ID, describeState(*d.State), d.Access)
}

func stateIcon(s entities.State) string {
	if !s.Reliable() {
		return iconWarn
	}
	return iconOK
}

// describeState renders the value with its unit; unavailable and unknown are
// spelled out so they are never mistaken for an inactive or safe state.
func describeState(s entities.State) string {
	if !s.Reliable() {
		return s.State + " (not reporting)"
	}
	if unit := s.Unit(); unit != "" {
		return escape(s.State + " " + unit)
	}
	return escape(s.State)
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
	id, err := entities.ParseID(req.Args["entity"])
	if err != nil {
		return r.fail(req, err)
	}
	st, err := r.deps.Entities.Status(ctx, req.Actor, id)
	if err != nil {
		return r.fail(req, err)
	}
	color := colorOK
	if !st.Reliable() {
		color = colorWarn
	}
	e := newEmbed("Status", "**"+escape(st.FriendlyName())+"**", color)
	e.Fields = []*discordgo.MessageEmbedField{
		field("Entity", "`"+string(st.ID)+"`", true),
		field("State", stateIcon(st)+" "+describeState(st), true),
	}
	if dc := st.DeviceClass(); dc != "" {
		e.Fields = append(e.Fields, field("Device class", escape(dc), true))
	}
	e.Fields = append(e.Fields, field("Last changed", relative(st.LastChanged), true))
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
	id, err := entities.ParseID(req.Args["entity"])
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
	stateText := "State: " + describeState(res.Before)
	if res.After != nil && res.After.State != res.Before.State {
		stateText = "State: " + describeState(res.Before) + " \u2192 " + describeState(*res.After)
	} else {
		stateText += " (Home Assistant has not reported a change yet; devices can take a moment)"
	}
	e := newEmbed("Command Sent", fmt.Sprintf("Turned **%s** `%s`.\n%s", action, id, stateText), colorOK)
	e.Fields = []*discordgo.MessageEmbedField{field("Entity", escape(res.Before.FriendlyName()), false)}
	return Reply{Embeds: []*discordgo.MessageEmbed{e}}
}

// permitted autocompletes the "entity" option from the allowlist.
func (r *Registry) permitted(minimum entities.Access) completer {
	return func(ctx context.Context, req Request) []*discordgo.ApplicationCommandOptionChoice {
		if req.Focused != "entity" {
			return nil
		}
		ids, err := r.deps.Entities.Suggest(ctx, req.Actor, minimum, req.Args["entity"])
		if err != nil {
			r.deps.Logger.Debug("autocomplete failed", "command", req.Command, "error", err)
			return nil
		}
		choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(ids))
		for _, id := range ids {
			choices = append(choices, choice(string(id), string(id)))
		}
		return choices
	}
}
