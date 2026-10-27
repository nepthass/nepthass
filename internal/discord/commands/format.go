package commands

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
)

const (
	titlePrefix = "\U0001F531 NeptHass \u2014 "

	colorOK    = 0x2B7FFF
	colorWarn  = 0xF5A623
	colorError = 0xD0021B

	iconOK       = "\u2705"
	iconDenied   = "\u26D4"
	iconWarn     = "\u26A0\uFE0F"
	iconFail     = "\u274C"
	iconDisabled = "\u2796"

	maxDescription = 4000
	maxFieldValue  = 1000
	maxChoice      = 100
)

// guildOnly keeps every command out of direct messages.
var guildOnly = &[]discordgo.InteractionContextType{discordgo.InteractionContextGuild}

func newEmbed(title, description string, color int) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       titlePrefix + title,
		Description: truncate(description, maxDescription),
		Color:       color,
	}
}

func field(name, value string, inline bool) *discordgo.MessageEmbedField {
	return &discordgo.MessageEmbedField{Name: name, Value: truncate(value, maxFieldValue), Inline: inline}
}

// truncate shortens s to at most limit runes, ending in an ellipsis when cut.
func truncate(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "\u2026"
}

// escape neutralizes Discord markdown and mention syntax in text that comes
// from Home Assistant (friendly names are attacker-influenced strings).
func escape(s string) string {
	return strings.NewReplacer(
		"\\", "\\\\", "*", "\\*", "_", "\\_", "~", "\\~", "`", "\\`",
		"|", "\\|", ">", "\\>", "#", "\\#", "[", "\\[", "<", "\\<", "@", "\\@",
	).Replace(s)
}

func relative(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return "<t:" + strconv.FormatInt(t.Unix(), 10) + ":R>"
}

func choice(name, value string) *discordgo.ApplicationCommandOptionChoice {
	return &discordgo.ApplicationCommandOptionChoice{Name: truncate(name, maxChoice), Value: value}
}

// fail converts an error into a safe user message. Internal details (URLs,
// HTTP bodies, SQL) go to the log only.
func (r *Registry) fail(req Request, err error) Reply {
	var msg string
	switch {
	case errors.Is(err, auth.ErrAdminRequired):
		msg = "This command is for NeptHass administrators."
	case errors.Is(err, auth.ErrNotAuthorized):
		msg = "You are not authorized to use NeptHass."
	case errors.Is(err, entities.ErrAccessDenied):
		msg = "You do not have access to that entity. An administrator can grant it with `/permissions allow`."
	case errors.Is(err, entities.ErrNotControllable):
		msg = "NeptHass can only control lights and switches. Other kinds of entity are read-only."
	case errors.Is(err, entities.ErrInvalidID):
		msg = "That is not a valid entity ID (for example `light.kitchen`) or alias."
	case errors.Is(err, entities.ErrInvalidAction), errors.Is(err, entities.ErrInvalidAccess):
		msg = "That option value is not valid."
	case errors.Is(err, rest.ErrNotFound):
		msg = "Home Assistant does not know that entity."
	case errors.Is(err, rest.ErrUnauthorized):
		msg = "Home Assistant rejected NeptHass's access token. An administrator needs to fix the server configuration."
	case errors.Is(err, rest.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		msg = "Home Assistant is not reachable right now. Try again in a moment."
	default:
		r.deps.Logger.Error("command failed", "command", req.Command, "subcommand", req.Subcommand, "error", err)
		msg = "Something went wrong. The details are in the NeptHass log."
	}
	return Reply{Content: iconFail + " " + msg}
}
