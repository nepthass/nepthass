package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
)

const (
	discoverTimeout  = 2 * time.Second
	defaultAuditRows = 10
	maxAuditRows     = 25
	maxDetailShown   = 80
)

func (r *Registry) cmdPermissions() command {
	sub := func(name, description string, opts ...*discordgo.ApplicationCommandOption) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{
			Type: discordgo.ApplicationCommandOptionSubCommand, Name: name, Description: description, Options: opts,
		}
	}
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "permissions", Description: "Manage which entities NeptHass users may see or control (administrators)", Contexts: guildOnly,
			Options: []*discordgo.ApplicationCommandOption{
				sub("allow", "Grant access to an entity",
					entityOption("Entity to share"),
					&discordgo.ApplicationCommandOption{
						Type: discordgo.ApplicationCommandOptionString, Name: "access", Description: "Access level", Required: true,
						Choices: []*discordgo.ApplicationCommandOptionChoice{
							{Name: "read (status only)", Value: "read"},
							{Name: "control (lights and switches only)", Value: "control"},
						},
					}),
				sub("revoke", "Remove an entity from the allowlist", entityOption("Entity to remove")),
				sub("list", "Show every shared entity"),
			},
		},
		min:      auth.LevelAdmin,
		run:      r.permissions,
		complete: r.completePermissions,
	}
}

func (r *Registry) permissions(ctx context.Context, req Request) Reply {
	switch req.Subcommand {
	case "allow":
		return r.allow(ctx, req)
	case "revoke":
		return r.revoke(ctx, req)
	case "list":
		return r.listPermissions(ctx, req)
	default:
		return Reply{Content: "Unknown subcommand."}
	}
}

func (r *Registry) allow(ctx context.Context, req Request) Reply {
	id, err := entities.ParseID(req.Args["entity"])
	if err != nil {
		return r.fail(req, err)
	}
	access, err := entities.ParseAccess(req.Args["access"])
	if err != nil {
		return r.fail(req, err)
	}
	if err := r.deps.Entities.Allow(ctx, req.Actor, id, access); err != nil {
		return r.fail(req, err)
	}
	return Reply{Content: fmt.Sprintf("%s `%s` now has **%s** access.", iconOK, id, access)}
}

func (r *Registry) revoke(ctx context.Context, req Request) Reply {
	id, err := entities.ParseID(req.Args["entity"])
	if err != nil {
		return r.fail(req, err)
	}
	removed, err := r.deps.Entities.Revoke(ctx, req.Actor, id)
	if err != nil {
		return r.fail(req, err)
	}
	if !removed {
		return Reply{Content: fmt.Sprintf("`%s` was not on the allowlist.", id)}
	}
	return Reply{Content: fmt.Sprintf("%s `%s` was removed from the allowlist.", iconOK, id)}
}

func (r *Registry) listPermissions(ctx context.Context, req Request) Reply {
	perms, err := r.deps.Entities.Permissions(ctx, req.Actor)
	if err != nil {
		return r.fail(req, err)
	}
	if len(perms) == 0 {
		return Reply{Content: "The allowlist is empty: nobody can see or control any entity yet."}
	}
	var b strings.Builder
	for i, p := range perms {
		line := fmt.Sprintf("\u2022 `%s` - **%s**\n", p.ID, p.Access)
		if b.Len()+len(line) > maxDescription-60 {
			fmt.Fprintf(&b, "... and %d more", len(perms)-i)
			break
		}
		b.WriteString(line)
	}
	return Reply{Embeds: []*discordgo.MessageEmbed{newEmbed("Permissions", b.String(), colorOK)}}
}

func (r *Registry) completePermissions(ctx context.Context, req Request) []*discordgo.ApplicationCommandOptionChoice {
	if req.Focused != "entity" {
		return nil
	}
	if req.Subcommand == "revoke" {
		return r.permitted(entities.AccessRead)(ctx, req)
	}
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	states, err := r.deps.Entities.Discover(ctx, req.Actor, req.Args["entity"])
	if err != nil {
		r.deps.Logger.Debug("entity discovery autocomplete failed", "error", err)
		return nil
	}
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(states))
	for _, st := range states {
		if len(st.ID) <= maxChoice {
			choices = append(choices, choice(st.FriendlyName()+" ("+string(st.ID)+")", string(st.ID)))
		}
	}
	return choices
}

func (r *Registry) cmdAudit() command {
	minRows, maxRows := 1.0, float64(maxAuditRows)
	return command{
		def: &discordgo.ApplicationCommand{
			Name: "audit", Description: "Review recent control actions and permission changes (administrators)", Contexts: guildOnly,
			Options: []*discordgo.ApplicationCommandOption{{
				Type: discordgo.ApplicationCommandOptionInteger, Name: "count", Description: "How many entries (default 10)",
				MinValue: &minRows, MaxValue: maxRows,
			}},
		},
		min: auth.LevelAdmin,
		run: r.audit,
	}
}

func (r *Registry) audit(ctx context.Context, req Request) Reply {
	count, err := strconv.Atoi(req.Args["count"])
	if err != nil || count < 1 || count > maxAuditRows {
		count = defaultAuditRows
	}
	entries, err := r.deps.Audit.Recent(ctx, count)
	if err != nil {
		return r.fail(req, err)
	}
	if len(entries) == 0 {
		return Reply{Content: "The audit log is empty."}
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(auditLine(e))
		b.WriteByte('\n')
	}
	return Reply{Embeds: []*discordgo.MessageEmbed{newEmbed("Audit Log", b.String(), colorOK)}}
}

func auditLine(e audit.Entry) string {
	icon := iconOK
	switch e.Outcome {
	case audit.OutcomeDenied:
		icon = iconDenied
	case audit.OutcomeError:
		icon = iconWarn
	case audit.OutcomeSuccess:
		// default icon
	}
	line := fmt.Sprintf("%s %s <@%s> `%s` %s", icon, relative(e.Time), e.ActorID, e.Command, escape(e.Action))
	if e.EntityID != "" {
		line += " `" + e.EntityID + "`"
	}
	if e.Detail != "" {
		line += " - " + escape(truncate(e.Detail, maxDetailShown))
	}
	return line
}
