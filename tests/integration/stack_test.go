//go:build integration

package integration

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
	"github.com/nepthass/nepthass/internal/setup"
	"github.com/nepthass/nepthass/internal/storage"
)

// The virtual devices defined in dev/homeassistant/packages.
const (
	light       = "light.nepthass_dev_light"
	switchID    = "switch.nepthass_dev_switch"
	motion      = "binary_sensor.nepthass_dev_motion"
	door        = "binary_sensor.nepthass_dev_door"
	window      = "binary_sensor.nepthass_dev_window"
	waterLeak   = "binary_sensor.nepthass_dev_water_leak"
	temperature = "sensor.nepthass_dev_temperature"
	humidity    = "sensor.nepthass_dev_humidity"
	battery     = "sensor.nepthass_dev_battery"
)

var (
	adminActor = auth.Actor{UserID: "600000000000000001", Level: auth.LevelAdmin}
	userActor  = auth.Actor{UserID: "600000000000000002", Level: auth.LevelUser}
)

// stack is NeptHass's real service layer wired to the real Home Assistant and a
// real (temporary) SQLite database.
type stack struct {
	db      *sql.DB
	ha      *rest.Client
	svc     *entities.Service
	perms   *entities.Permissions
	aliases *entities.Aliases
	disc    *entities.Discovery
	setup   *setup.Service
	audit   *audit.Log
}

func newStack(t *testing.T) stack {
	t.Helper()
	return openStack(t, filepath.Join(t.TempDir(), "nepthass-it.db"))
}

// openStack opens (or reopens) the database at path, like a process restart.
func openStack(t *testing.T, path string) stack {
	t.Helper()
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ha := rest.New(config.HomeAssistant{URL: target.url, Token: config.Secret(target.token), Timeout: requestTimeout})
	s := stack{db: db, ha: ha, perms: entities.NewPermissions(db), aliases: entities.NewAliases(db), audit: audit.New(db)}
	s.svc = entities.NewService(entities.Deps{
		HA: ha, Permissions: s.perms, Aliases: s.aliases, Audit: s.audit, Logger: slog.New(slog.DiscardHandler), Now: time.Now,
	})
	s.disc = entities.NewDiscovery(entities.DiscoveryDeps{HA: ha, DB: db, Permissions: s.perms, Now: time.Now})
	s.setup = setup.NewService(setup.Deps{
		DB: db, Store: setup.NewStore(db), Discovery: s.disc, Permissions: s.perms, Aliases: s.aliases, Audit: s.audit, Now: time.Now,
	})
	return s
}

func (s stack) allow(t *testing.T, id string, access entities.Access) {
	t.Helper()
	if err := s.svc.Allow(context.Background(), adminActor, entities.ID(id), access); err != nil {
		t.Fatalf("Allow(%s, %s): %v", id, access, err)
	}
}
