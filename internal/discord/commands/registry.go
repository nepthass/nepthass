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
	GuildID    string
	Command    string
	Subcommand string
	// InteractionID identifies the Discord interaction (set by the bot); it lets the
	// denial audit record a replayed interaction once.
	InteractionID string
	// Args holds option values by name. Integers are decimal strings.
	Args map[string]string
	// Focused is the option being typed during autocomplete ("" otherwise).
	Focused string
	// BadOptions names options whose type NeptHass cannot parse. A request with
	// any is refused rather than run with an option silently missing.
	BadOptions []string
}

// Reply is the message shown to the user (always ephemeral).
type Reply struct {
	Content    string
	Embeds     []*discordgo.MessageEmbed
	Components []discordgo.MessageComponent
}

// ModalInput is one text field of a modal.
type ModalInput struct {
	CustomID    string
	Label       string
	Placeholder string
	Value       string
	Required    bool
	MaxLength   int
}

// Modal is a text-input dialog opened in response to a component click.
type Modal struct {
	CustomID string
	Title    string
	Inputs   []ModalInput
}

// ModalResult is the answer to a modal-opening click: the modal, or a notice to
// show instead when the click is stale or not allowed.
type ModalResult struct {
	Modal  *Modal
	Notice Reply
}

// SetupStarter starts or resumes the setup wizard (implemented by the wizard package).
type SetupStarter interface {
	Start(ctx context.Context, actor auth.Actor, guildID string) Reply
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
	Entities  *entities.Service
	Discovery *entities.Discovery
	Setup     SetupStarter
	// Monitoring may be nil when live monitoring is not wired.
	Monitoring Monitoring
	// Notifications may be nil: then /notifications is not registered and
	// /health shows no notification line (v0.3.0 behaviour).
	Notifications Notifications
	Audit         *audit.Log
	// Denials audits refused administrator commands; nil records nothing.
	Denials *audit.Denials
	HA      HomeAssistant
	DB      Pinger
	Gateway func() Gateway
	Version string
	Logger  *slog.Logger
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
		r.cmdPing(), r.cmdHealth(), r.cmdDevices(), r.cmdStatus(), r.cmdControl(), r.cmdPermissions(), r.cmdAudit(), r.cmdSetup(), r.cmdSync(), r.cmdSensors(),
	} {
		r.cmds[c.def.Name] = c
		r.order = append(r.order, c.def.Name)
	}
	if d.Notifications != nil {
		c := r.cmdNotifications()
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

// Level returns the minimum privilege level of a registered command; false for a
// name the registry does not define.
func (r *Registry) Level(name string) (auth.Level, bool) {
	c, ok := r.cmds[name]
	return c.min, ok
}

// Run executes a command after enforcing its minimum privilege level.
func (r *Registry) Run(ctx context.Context, req Request) Reply {
	c, ok := r.cmds[req.Command]
	if !ok {
		return Reply{Content: "Unknown command."}
	}
	if len(req.BadOptions) > 0 {
		r.deps.Logger.Warn("refused a command with an option type NeptHass cannot parse", "command", req.Command, "options", req.BadOptions)
		return Reply{Content: "This command contains an option that this version of NeptHass does not understand, so it was not run."}
	}
	if err := req.Actor.Require(c.min); err != nil {
		// The refusal is decided; auditing it can neither grant access nor change the reply.
		if c.min >= auth.LevelAdmin {
			r.deps.Denials.Record(ctx, audit.DenialAttempt{InteractionID: req.InteractionID, ActorID: req.Actor.UserID, Command: req.Command,
				Action: req.Subcommand, GuildID: req.GuildID, Reason: audit.ReasonNotAdmin})
		}
		return r.fail(req, err)
	}
	return c.run(ctx, req)
}

// Complete returns autocomplete choices for the focused option.
func (r *Registry) Complete(ctx context.Context, req Request) []*discordgo.ApplicationCommandOptionChoice {
	c, ok := r.cmds[req.Command]
	if !ok || c.complete == nil || len(req.BadOptions) > 0 || req.Actor.Require(c.min) != nil {
		return nil
	}
	return c.complete(ctx, req)
}
