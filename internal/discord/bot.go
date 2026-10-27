// Package discord connects NeptHass to the Discord gateway: it keeps the
// connection alive, registers slash commands and routes every interaction
// through authorization before any command logic runs.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/discord/commands"
)

const (
	rateLimitBurst  = 6
	rateLimitWindow = 10 * time.Second
)

// ErrInvalidToken means Discord rejected the bot token; retrying cannot help.
var ErrInvalidToken = errors.New("discord rejected the bot token")

// Bot owns the Discord session.
type Bot struct {
	cfg     config.Discord
	log     *slog.Logger
	auth    *auth.Authorizer
	limiter *auth.Limiter
	reg     *commands.Registry
	session *discordgo.Session
	ready   atomic.Bool
	retry   backoff
}

// New prepares the bot without connecting. deps.Gateway is filled in here.
func New(cfg config.Discord, deps commands.Deps) (*Bot, error) {
	session, err := discordgo.New("Bot " + cfg.Token.Reveal())
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	// Slash commands need no privileged intents.
	session.Identify.Intents = discordgo.IntentsGuilds
	session.LogLevel = discordgo.LogWarning
	routeLibraryLogs(deps.Logger)

	b := &Bot{
		cfg:     cfg,
		log:     deps.Logger,
		auth:    auth.NewAuthorizer(cfg),
		limiter: auth.NewLimiter(rateLimitBurst, rateLimitWindow, time.Now),
		session: session,
		retry:   backoff{initial: time.Second, max: time.Minute},
	}
	deps.Gateway = b.gateway
	b.reg = commands.New(deps)
	return b, nil
}

func (b *Bot) gateway() commands.Gateway {
	return commands.Gateway{Connected: b.ready.Load(), Latency: b.session.HeartbeatLatency()}
}

// Run connects, registers commands and serves interactions until ctx is
// cancelled. Temporary network failures are retried with backoff; once
// connected, discordgo reconnects the gateway on its own. A rejected token
// is returned as ErrInvalidToken.
func (b *Bot) Run(ctx context.Context) error {
	b.session.AddHandler(func(*discordgo.Session, *discordgo.Ready) { b.ready.Store(true) })
	b.session.AddHandler(func(*discordgo.Session, *discordgo.Resumed) { b.ready.Store(true) })
	b.session.AddHandler(func(*discordgo.Session, *discordgo.Disconnect) { b.ready.Store(false) })
	b.session.AddHandler(func(s *discordgo.Session, ic *discordgo.InteractionCreate) {
		b.serve(ctx, s, ic.Interaction)
	})

	if err := b.retry.retry(ctx, b.log, "verify discord token", func() error { return b.verifyToken(ctx) }); err != nil {
		return err
	}
	if err := b.retry.retry(ctx, b.log, "open discord gateway", b.session.Open); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	b.log.Info("connected to discord gateway")
	if err := b.retry.retry(ctx, b.log, "register slash commands", func() error { return b.registerCommands(ctx) }); err != nil {
		_ = b.session.Close()
		return fmt.Errorf("register commands: %w", err)
	}

	<-ctx.Done()
	b.log.Info("closing discord session")
	return b.session.Close()
}

func (b *Bot) verifyToken(ctx context.Context) error {
	_, err := b.session.User("@me", discordgo.WithContext(ctx))
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil && restErr.Response.StatusCode == http.StatusUnauthorized {
		return permanentError{err: ErrInvalidToken}
	}
	return err
}

// registerCommands installs the commands in the configured scope and clears
// the other scope so that a switch between guild and global registration does
// not leave every command listed twice.
func (b *Bot) registerCommands(ctx context.Context) error {
	target, stale := b.cfg.GuildID, ""
	if b.cfg.CommandScope == config.ScopeGlobal {
		target, stale = "", b.cfg.GuildID
	}
	opt := discordgo.WithContext(ctx)
	if _, err := b.session.ApplicationCommandBulkOverwrite(b.cfg.ApplicationID, target, b.reg.Definitions(), opt); err != nil {
		return fmt.Errorf("install commands (is the bot in the guild with the applications.commands scope?): %w", err)
	}
	if _, err := b.session.ApplicationCommandBulkOverwrite(b.cfg.ApplicationID, stale, []*discordgo.ApplicationCommand{}, opt); err != nil {
		return fmt.Errorf("clear commands from the other scope: %w", err)
	}
	b.log.Info("slash commands registered", "scope", string(b.cfg.CommandScope), "count", len(b.reg.Definitions()))
	return nil
}
