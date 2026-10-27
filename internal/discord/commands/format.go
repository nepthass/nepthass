package commands

import (
	"context"
	"errors"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/ui"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
)

// guildOnly keeps every command out of direct messages.
var guildOnly = &[]discordgo.InteractionContextType{discordgo.InteractionContextGuild}

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
	return Reply{Content: ui.IconFail + " " + msg}
}
