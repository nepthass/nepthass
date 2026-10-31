// Command nepthass runs the NeptHass Discord bridge for Home Assistant.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/discord"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
	"github.com/nepthass/nepthass/internal/logging"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
	"github.com/nepthass/nepthass/internal/setup"
	"github.com/nepthass/nepthass/internal/storage"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const startupCheckTimeout = 15 * time.Second

func main() { os.Exit(run()) }

func run() int {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("nepthass", version)
		return 0
	}
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "nepthass:", err)
		return 2
	}
	log := logging.New(os.Stderr, cfg.Log, []config.Secret{cfg.Discord.Token, cfg.HomeAssistant.Token})
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("nepthass stopped", "error", err)
		return 1
	}
	log.Info("nepthass stopped")
	return 0
}

func serve(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	log.Info("starting nepthass", "version", version, "home_assistant_url", cfg.HomeAssistant.URL,
		"command_scope", string(cfg.Discord.CommandScope), "administrators", len(cfg.Discord.AdminUserIDs))

	db, err := storage.Open(ctx, cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	ha := rest.New(cfg.HomeAssistant)
	if err := checkHomeAssistant(ctx, ha, log); err != nil {
		return err
	}

	auditLog := audit.New(db)
	perms, aliases := entities.NewPermissions(db), entities.NewAliases(db)
	service := entities.NewService(entities.Deps{
		HA: ha, Permissions: perms, Aliases: aliases, Audit: auditLog, Logger: log, Now: time.Now,
	})
	discovery := entities.NewDiscovery(entities.DiscoveryDeps{HA: ha, DB: db, Permissions: perms, Now: time.Now})
	setupService := setup.NewService(setup.Deps{
		DB: db, Store: setup.NewStore(db), Discovery: discovery, Permissions: perms, Aliases: aliases, Audit: auditLog, Now: time.Now,
	})
	hook := &changeHook{hub: monitor.NewHub()}
	mon, err := monitor.New(monitor.Config{HomeAssistant: cfg.HomeAssistant, Logger: log, OnChange: hook.OnChange})
	if err != nil {
		return err
	}
	// Notifications: monitor.OnChange -> engine mailbox -> evaluator -> dispatcher ->
	// Discord notifier. With no notification settings stored the engine is disabled
	// and its callback returns immediately, so v0.3.0 behaviour is unchanged.
	notifyStore := notify.NewStore(db)
	notifySvc := notify.NewService(notify.Deps{GuildID: cfg.Discord.GuildID, DB: db, Store: notifyStore, Permissions: perms,
		Sensors: mon.Tracker(), Audit: auditLog, Now: time.Now, Logger: log})
	notifier := discord.NewNotifier(cfg.Discord.GuildID, notifyStore, log)
	engine, err := notify.NewEngine(notify.EngineDeps{Store: notifyStore, GuildID: cfg.Discord.GuildID, Access: perms, Sensors: mon.Tracker(),
		Aliases: aliases, Status: mon, Sender: notifier, Binder: notifySvc, Logger: log})
	if err != nil {
		return err
	}
	hook.set(engine.OnChange)
	admin := discord.NewNotificationAdmin(notifySvc, engine, notifier, mon.Tracker(), aliases, log)
	bot, err := discord.New(cfg.Discord, commands.Deps{
		Entities: service, Discovery: discovery, Monitoring: monitor.NewReader(mon, perms, aliases), Notifications: admin,
		Audit: auditLog, Denials: audit.NewDenials(auditLog, log, time.Now), HA: ha, DB: db, Version: version, Logger: log,
	}, setupService)
	if err != nil {
		return err
	}

	notifier.Bind(bot.Session())
	if err := engine.Reload(ctx); err != nil {
		log.Warn("notification settings could not be loaded; notifications stay disabled", "error", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	var background sync.WaitGroup
	background.Add(3)
	go func() {
		defer background.Done()
		auditLog.RunRetention(ctx, cfg.Database.AuditRetention, log)
	}()
	// The monitor is owned by the application: it stops with ctx and Run
	// returns only after its socket is closed, so shutdown waits for it below.
	go func() {
		defer background.Done()
		mon.Run(ctx)
	}()
	// The notification engine stops with ctx; Run returns after a drain bounded to
	// three seconds, so shutdown waits for it below like for the monitor.
	go func() {
		defer background.Done()
		engine.Run(ctx)
	}()
	err = bot.Run(ctx)
	cancel()
	background.Wait()
	return err
}

// checkHomeAssistant fails fast on a rejected token, which a restart cannot
// fix, but tolerates an unreachable Home Assistant (it may still be booting).
func checkHomeAssistant(ctx context.Context, ha *rest.Client, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, startupCheckTimeout)
	defer cancel()
	info, err := ha.Info(ctx)
	switch {
	case errors.Is(err, rest.ErrUnauthorized):
		return fmt.Errorf("home assistant rejected HA_TOKEN: %w", err)
	case err != nil:
		log.Warn("home assistant is not reachable yet; NeptHass keeps running and will use it once it answers", "error", err)
	default:
		log.Info("authenticated with home assistant", "version", info.Version)
	}
	return nil
}

// changeHook lets the monitor be built before the notification engine: the
// engine needs the monitor's tracker and status, and the monitor needs the
// engine's callback. Until set it drops changes, which cannot matter because
// the monitor has not started.
type changeHook struct {
	fn atomic.Pointer[func(monitor.Change)]
	// hub fans the same stream out to other consumers (a future browser event stream). With no
	// subscriber it does nothing, so the engine path is exactly what it was.
	hub *monitor.Hub
}

func (h *changeHook) set(f func(monitor.Change)) { h.fn.Store(&f) }

func (h *changeHook) OnChange(c monitor.Change) {
	// The hub never blocks and never panics, so it goes first: a panic in the engine callback
	// (recovered by the monitor) cannot make a subscriber miss the change.
	if h.hub != nil {
		h.hub.Publish(c)
	}
	if f := h.fn.Load(); f != nil {
		(*f)(c)
	}
}
