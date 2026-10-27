package commands_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/rest"
	"github.com/nepthass/nepthass/internal/storage"
	"github.com/nepthass/nepthass/tests/fakeha"
)

const haToken = "ha-token-for-tests"

var (
	admin = auth.Actor{UserID: "400000000000000001", Level: auth.LevelAdmin}
	user  = auth.Actor{UserID: "400000000000000002", Level: auth.LevelUser}
)

type env struct {
	reg     *commands.Registry
	ha      *fakeha.Server
	perms   *entities.Permissions
	aliases *entities.Aliases
	log     *audit.Log
}

func newEnv(t *testing.T) env { return newEnvWith(t, nil) }

// newEnvWith builds the environment with live monitoring supplied by mon (nil: not wired).
func newEnvWith(t *testing.T, mon func(*entities.Permissions, *entities.Aliases) commands.Monitoring) env {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "cmd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ha := fakeha.New(t, haToken)
	client := rest.New(config.HomeAssistant{URL: ha.URL, Token: haToken, Timeout: 2 * time.Second})
	auditLog := audit.New(db)
	perms := entities.NewPermissions(db)
	aliases := entities.NewAliases(db)
	logger := slog.New(slog.DiscardHandler)
	svc := entities.NewService(entities.Deps{HA: client, Permissions: perms, Aliases: aliases, Audit: auditLog, Logger: logger, Now: time.Now})
	deps := commands.Deps{
		Entities: svc, Audit: auditLog, HA: client, DB: db, Logger: logger, Version: "test",
		Gateway: func() commands.Gateway { return commands.Gateway{Connected: true, Latency: 42 * time.Millisecond} },
	}
	if mon != nil {
		deps.Monitoring = mon(perms, aliases)
	}
	reg := commands.New(deps)
	return env{reg: reg, ha: ha, perms: perms, aliases: aliases, log: auditLog}
}

func (e env) grant(t *testing.T, id string, access entities.Access) {
	t.Helper()
	if err := e.perms.Set(context.Background(), entities.Grant{ID: entities.ID(id), Access: access, GrantedBy: admin.UserID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func (e env) run(actor auth.Actor, cmd, sub string, args map[string]string) string {
	return text(e.reg.Run(context.Background(), commands.Request{Actor: actor, Command: cmd, Subcommand: sub, Args: args}))
}

// text flattens a reply so assertions can ignore its layout.
func text(r commands.Reply) string {
	var b strings.Builder
	b.WriteString(r.Content)
	for _, e := range r.Embeds {
		b.WriteString("\n" + e.Title + "\n" + e.Description)
		for _, f := range e.Fields {
			b.WriteString("\n" + f.Name + ": " + f.Value)
		}
		if e.Footer != nil {
			b.WriteString("\n" + e.Footer.Text)
		}
	}
	return b.String()
}

func mustContain(t *testing.T, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("reply does not contain %q:\n%s", w, got)
		}
	}
}

func TestStatus_showsStateWithUnit_andEscapesFriendlyNames(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("sensor.hall_temperature", "21.5", map[string]any{"friendly_name": "**Hall** @everyone", "unit_of_measurement": "\u00b0C", "device_class": "temperature"})
	e.grant(t, "sensor.hall_temperature", entities.AccessRead)

	got := e.run(user, "status", "", map[string]string{"entity": "sensor.hall_temperature"})

	mustContain(t, got, "21.5 \u00b0C", "temperature", "`sensor.hall_temperature`", "\\*\\*Hall\\*\\* \\@everyone")
	if strings.Contains(got, "**Hall**") {
		t.Errorf("friendly name was not escaped:\n%s", got)
	}
}

func TestStatus_neverPresentsUnavailableAsASafeState(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("binary_sensor.leak", "unavailable", nil)
	e.grant(t, "binary_sensor.leak", entities.AccessRead)

	got := e.run(user, "status", "", map[string]string{"entity": "binary_sensor.leak"})

	mustContain(t, got, "unavailable (not reporting)", "\u26A0")
}

func TestStatus_refusesEntitiesThatWereNeverShared(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("device_tracker.phone", "home", nil)

	got := e.run(user, "status", "", map[string]string{"entity": "device_tracker.phone"})

	mustContain(t, got, "do not have access")
	if strings.Contains(got, "home") && strings.Contains(got, "device_tracker.phone`") {
		t.Errorf("state leaked: %s", got)
	}
}

func TestStatus_rejectsMalformedEntityIDs(t *testing.T) {
	e := newEnv(t)

	got := e.run(user, "status", "", map[string]string{"entity": "../../etc/passwd"})

	mustContain(t, got, "not a valid entity ID")
}

func TestControl_switchesPermittedLight(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("light.kitchen", "off", map[string]any{"friendly_name": "Kitchen"})
	e.grant(t, "light.kitchen", entities.AccessControl)

	got := e.run(user, "control", "", map[string]string{"entity": "light.kitchen", "action": "on"})

	mustContain(t, got, "Command Sent", "off \u2192 on")
	calls := e.ha.Calls()
	if len(calls) != 1 || calls[0] != (fakeha.Call{Domain: "light", Service: "turn_on", EntityID: "light.kitchen"}) {
		t.Errorf("calls = %+v", calls)
	}
}

func TestControl_isRefusedWithoutControlPermission(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("light.kitchen", "off", nil)
	e.ha.Set("light.hall", "off", nil)
	e.grant(t, "light.hall", entities.AccessRead)

	for _, id := range []string{"light.kitchen", "light.hall"} {
		got := e.run(user, "control", "", map[string]string{"entity": id, "action": "on"})

		mustContain(t, got, "do not have access")
	}
	if calls := e.ha.Calls(); len(calls) != 0 {
		t.Errorf("Home Assistant received %+v", calls)
	}
}

func TestPermissions_cannotGrantControlOverLocksOrGarageDoors(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("lock.front_door", "locked", nil)
	e.ha.Set("cover.garage_door", "closed", nil)

	for _, id := range []string{"lock.front_door", "cover.garage_door"} {
		got := e.run(admin, "permissions", "allow", map[string]string{"entity": id, "access": "control"})

		mustContain(t, got, "only control lights and switches")
	}
	if perms, _ := e.perms.List(context.Background()); len(perms) != 0 {
		t.Errorf("permissions = %+v", perms)
	}
}

func TestPermissions_adminCommandsAreRefusedForRegularUsers(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("light.kitchen", "off", nil)

	for _, sub := range []string{"allow", "revoke", "list"} {
		got := e.run(user, "permissions", sub, map[string]string{"entity": "light.kitchen", "access": "control"})

		mustContain(t, got, "administrators")
	}
	mustContain(t, e.run(user, "audit", "", nil), "administrators")
	if perms, _ := e.perms.List(context.Background()); len(perms) != 0 {
		t.Errorf("permissions = %+v", perms)
	}
}

func TestPermissions_allowListRevokeFlow(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("switch.fan", "off", nil)

	allowed := e.run(admin, "permissions", "allow", map[string]string{"entity": "switch.fan", "access": "control"})
	listed := e.run(admin, "permissions", "list", nil)
	revoked := e.run(admin, "permissions", "revoke", map[string]string{"entity": "switch.fan"})
	again := e.run(admin, "permissions", "revoke", map[string]string{"entity": "switch.fan"})
	empty := e.run(admin, "permissions", "list", nil)

	mustContain(t, allowed, "now has **control** access")
	mustContain(t, listed, "`switch.fan`", "control")
	mustContain(t, revoked, "removed from the allowlist")
	mustContain(t, again, "was not on the allowlist")
	mustContain(t, empty, "allowlist is empty")
}

func TestPermissions_rejectsEntitiesUnknownToHomeAssistant(t *testing.T) {
	e := newEnv(t)

	got := e.run(admin, "permissions", "allow", map[string]string{"entity": "light.typo", "access": "read"})

	mustContain(t, got, "does not know that entity")
}

func TestDevices_listsPermittedEntitiesOnly_withPaging(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("light.kitchen", "on", map[string]any{"friendly_name": "Kitchen"})
	e.ha.Set("light.secret", "on", nil)
	e.grant(t, "light.kitchen", entities.AccessControl)
	e.grant(t, "light.removed", entities.AccessRead)

	got := e.run(user, "devices", "", map[string]string{"page": "1"})

	mustContain(t, got, "Kitchen", "light.kitchen", "light.removed", "not found in Home Assistant", "Page 1 of 1 - 2 entities")
	if strings.Contains(got, "light.secret") {
		t.Errorf("unshared entity listed:\n%s", got)
	}
}

func TestDevices_explainsAnEmptyAllowlist(t *testing.T) {
	e := newEnv(t)

	mustContain(t, e.run(user, "devices", "", nil), "No entities have been shared yet")
}

func TestHealth_reportsEachSubsystem(t *testing.T) {
	e := newEnv(t)

	got := e.run(user, "health", "", nil)

	mustContain(t, got, "Connected (42 ms)", "Home Assistant 2026.10.0", "Database is responding", "WebSocket", "Not running")
}

func TestHealth_reportsRejectedHomeAssistantToken(t *testing.T) {
	e := newEnv(t)
	badClient := rest.New(config.HomeAssistant{URL: e.ha.URL, Token: "wrong", Timeout: time.Second})
	reg := commands.New(commands.Deps{
		HA: badClient, DB: pinger{}, Logger: slog.New(slog.DiscardHandler), Version: "test",
		Gateway: func() commands.Gateway { return commands.Gateway{} },
	})

	got := text(reg.Run(context.Background(), commands.Request{Actor: user, Command: "health"}))

	mustContain(t, got, "Access token rejected", "Not connected")
}

type pinger struct{}

func (pinger) PingContext(context.Context) error { return nil }

func TestAudit_listsControlAttemptsForAdministrators(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("light.kitchen", "off", nil)
	e.run(user, "control", "", map[string]string{"entity": "light.kitchen", "action": "on"}) // denied
	e.grant(t, "light.kitchen", entities.AccessControl)
	e.run(user, "control", "", map[string]string{"entity": "light.kitchen", "action": "on"}) // allowed

	got := e.run(admin, "audit", "", map[string]string{"count": "5"})

	mustContain(t, got, "<@"+user.UserID+">", "`control`", "`light.kitchen`", "\u26D4", "\u2705")
}

func TestComplete_suggestsOnlyEntitiesTheUserMayUse(t *testing.T) {
	e := newEnv(t)
	e.grant(t, "light.kitchen", entities.AccessControl)
	e.grant(t, "sensor.kitchen_temp", entities.AccessRead)

	forControl := e.reg.Complete(context.Background(), commands.Request{Actor: user, Command: "control", Focused: "entity", Args: map[string]string{"entity": "kit"}})
	forStatus := e.reg.Complete(context.Background(), commands.Request{Actor: user, Command: "status", Focused: "entity", Args: map[string]string{"entity": "kit"}})

	if len(forControl) != 1 || forControl[0].Value != "light.kitchen" || len(forStatus) != 2 {
		t.Errorf("control=%v status=%v", forControl, forStatus)
	}
}

func TestComplete_discoveryIsAdminOnly(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("light.lamp", "on", map[string]any{"friendly_name": "Reading Lamp"})
	req := commands.Request{Command: "permissions", Subcommand: "allow", Focused: "entity", Args: map[string]string{"entity": "lamp"}}

	req.Actor = admin
	asAdmin := e.reg.Complete(context.Background(), req)
	req.Actor = user
	asUser := e.reg.Complete(context.Background(), req)

	if len(asAdmin) != 1 || asAdmin[0].Value != "light.lamp" || asAdmin[0].Name != "Reading Lamp (light.lamp)" {
		t.Errorf("admin choices = %+v", asAdmin)
	}
	if len(asUser) != 0 {
		t.Errorf("regular user can browse every Home Assistant entity: %+v", asUser)
	}
}

// Discord rejects malformed definitions at registration time, which is only
// visible on a live connection, so the constraints are checked here.
func TestDefinitions_satisfyDiscordConstraints(t *testing.T) {
	name := regexp.MustCompile(`^[-_\p{L}\p{N}]{1,32}$`)
	var checkOptions func(path string, opts []*discordgo.ApplicationCommandOption)
	checkOptions = func(path string, opts []*discordgo.ApplicationCommandOption) {
		if len(opts) > 25 {
			t.Errorf("%s has %d options", path, len(opts))
		}
		for _, o := range opts {
			if !name.MatchString(o.Name) || o.Name != strings.ToLower(o.Name) {
				t.Errorf("%s: bad option name %q", path, o.Name)
			}
			if n := len([]rune(o.Description)); n < 1 || n > 100 {
				t.Errorf("%s/%s: description length %d", path, o.Name, n)
			}
			if len(o.Choices) > 25 || (o.Autocomplete && len(o.Choices) > 0) {
				t.Errorf("%s/%s: invalid choices/autocomplete combination", path, o.Name)
			}
			checkOptions(path+"/"+o.Name, o.Options)
		}
	}

	defs := newEnv(t).reg.Definitions()

	var names []string
	for _, d := range defs {
		names = append(names, d.Name)
		if !name.MatchString(d.Name) || d.Name != strings.ToLower(d.Name) {
			t.Errorf("bad command name %q", d.Name)
		}
		if n := len([]rune(d.Description)); n < 1 || n > 100 {
			t.Errorf("/%s description length %d", d.Name, n)
		}
		if d.Contexts == nil || len(*d.Contexts) != 1 || (*d.Contexts)[0] != discordgo.InteractionContextGuild {
			t.Errorf("/%s must be guild-only", d.Name)
		}
		checkOptions("/"+d.Name, d.Options)
	}
	if got := strings.Join(names, ","); got != "ping,health,devices,status,control,permissions,audit,setup,sync,sensors" {
		t.Errorf("commands = %s", got)
	}
}

func TestAliases_workInStatusControlDevicesAndAutocomplete_withoutExposingUnsharedEntities(t *testing.T) {
	e := newEnv(t)
	e.ha.Set("light.kitchen", "off", map[string]any{"friendly_name": "Kitchen Ceiling"})
	e.ha.Set("light.secret", "off", map[string]any{"friendly_name": "Hidden"})
	e.grant(t, "light.kitchen", entities.AccessControl)
	for id, alias := range map[string]string{"light.kitchen": "Kitchen", "light.secret": "Secret Lamp"} {
		if err := e.aliases.Set(context.Background(), entities.AliasSet{ID: entities.ID(id), Alias: alias, By: admin.UserID, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}

	status := e.run(user, "status", "", map[string]string{"entity": "kitchen"})
	control := e.run(user, "control", "", map[string]string{"entity": "KITCHEN", "action": "on"})
	devices := e.run(user, "devices", "", nil)
	secret := e.run(user, "status", "", map[string]string{"entity": "Secret Lamp"})
	complete := e.reg.Complete(context.Background(), commands.Request{Actor: user, Command: "control", Focused: "entity", Args: map[string]string{"entity": "kitch"}})

	mustContain(t, status, "**Kitchen**", "Home Assistant name", "Kitchen Ceiling")
	mustContain(t, control, "Command Sent", "Kitchen")
	if calls := e.ha.Calls(); len(calls) != 1 || calls[0].EntityID != "light.kitchen" {
		t.Errorf("alias did not resolve to the canonical entity: %+v", calls)
	}
	mustContain(t, devices, "**Kitchen**", "light.kitchen")
	mustContain(t, secret, "not a valid entity ID", "alias")
	if strings.Contains(secret, "Hidden") || strings.Contains(devices, "Secret Lamp") || strings.Contains(devices, "light.secret") {
		t.Errorf("an unshared entity leaked:\n%s\n%s", secret, devices)
	}
	if len(complete) != 1 || complete[0].Name != "Kitchen (light.kitchen)" || complete[0].Value != "light.kitchen" {
		t.Errorf("autocomplete = %+v, want the alias in the label and the canonical ID as the value", complete)
	}
}
