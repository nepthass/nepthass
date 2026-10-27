// Package commands defines NeptHass's slash commands and turns a parsed
// request into a reply. It contains no Discord transport code: the discord
// package authenticates the caller, parses the interaction into a Request and
// delivers the Reply.
package commands

import (
	"context"
	"log/slog"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
)

// Request is a parsed, already-authorized command invocation.
type Request struct {
	Actor      auth.Actor
	Command    string
	Subcommand string
	// Args holds option values by name. Integers are decimal strings.
	Args map[string]string
	// Focused is the option being typed during autocomplete ("" otherwise).
	Focused string
}

// Reply is the message shown to the user (always ephemeral).
type Reply struct {
	Content string
	Embeds  []*discordgo.MessageEmbed
}

// Gateway describes the Discord connection for /ping and /health.
type Gateway struct {
	Connected bool
	Latency   time.Duration
}

// HomeAssistant reports Home Assistant server details for /health.
type HomeAssistant interface {
	Info(ctx context.Context) (rest.Info, error)
}

// Pinger checks that the database answers (satisfied by *sql.DB).
type Pinger interface {
	PingContext(ctx context.Context) error
}

// Deps are the collaborators of the command set.
type Deps struct {
	Entities *entities.Service
	Audit    *audit.Log
	HA       HomeAssistant
	DB       Pinger
	Gateway  func() Gateway
	Version  string
	Logger   *slog.Logger
}

type (
	handler   func(ctx context.Context, req Request) Reply
	completer func(ctx context.Context, req Request) []*discordgo.ApplicationCommandOptionChoice
)

type command struct {
	def      *discordgo.ApplicationCommand
	min      auth.Level
	run      handler
	complete completer
}

// Registry holds every command and dispatches requests to them.
type Registry struct {
	deps  Deps
	cmds  map[string]command
	order []string
}

// New builds the v0.1.0 command set.
func New(d Deps) *Registry {
	r := &Registry{deps: d, cmds: make(map[string]command)}
	for _, c := range []command{
		r.cmdPing(), r.cmdHealth(), r.cmdDevices(), r.cmdStatus(), r.cmdControl(), r.cmdPermissions(), r.cmdAudit(),
	} {
		r.cmds[c.def.Name] = c
		r.order = append(r.order, c.def.Name)
	}
	return r
}

// Definitions returns the command definitions to register with Discord.
func (r *Registry) Definitions() []*discordgo.ApplicationCommand {
	defs := make([]*discordgo.ApplicationCommand, 0, len(r.order))
	for _, name := range r.order {
		defs = append(defs, r.cmds[name].def)
	}
	return defs
}

// Run executes a command after enforcing its minimum privilege level.
func (r *Registry) Run(ctx context.Context, req Request) Reply {
	c, ok := r.cmds[req.Command]
	if !ok {
		return Reply{Content: "Unknown command."}
	}
	if err := req.Actor.Require(c.min); err != nil {
		return r.fail(req, err)
	}
	return c.run(ctx, req)
}

// Complete returns autocomplete choices for the focused option.
func (r *Registry) Complete(ctx context.Context, req Request) []*discordgo.ApplicationCommandOptionChoice {
	c, ok := r.cmds[req.Command]
	if !ok || c.complete == nil || req.Actor.Require(c.min) != nil {
		return nil
	}
	return c.complete(ctx, req)
}
