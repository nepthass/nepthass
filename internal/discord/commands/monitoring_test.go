package commands_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

var observed = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// monitorEnv wires the real Reader over a monitor that is not connected, with
// sensors placed straight into its tracker.
type monitorEnv struct {
	env
	mon *monitor.Monitor
}

func newMonitorEnv(t *testing.T) monitorEnv {
	t.Helper()
	m, err := monitor.New(monitor.Config{HomeAssistant: config.HomeAssistant{URL: "http://127.0.0.1:1", Token: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	return monitorEnv{env: newEnvWith(t, func(p *entities.Permissions, a *entities.Aliases) commands.Monitoring {
		return monitor.NewReader(m, p, a)
	}), mon: m}
}

func (e monitorEnv) track(id string, cat monitor.Category, raw, unit, name string) {
	e.mon.Tracker().Apply(monitor.Update{ID: entities.ID(id), Category: cat, Raw: raw, Unit: unit, Name: name, LastChanged: observed, LastUpdated: observed}, observed)
}

func TestSensors_showInterpretedMeaning_andNeverPresentUnreliableValuesAsSafe(t *testing.T) {
	e := newMonitorEnv(t)
	e.track("binary_sensor.door", monitor.CategoryDoor, "on", "", "Front Door")
	e.track("sensor.temp", monitor.CategoryTemperature, "21.5", "\u00b0C", "Living Temp")
	e.track("binary_sensor.window", monitor.CategoryWindow, "unavailable", "", "Bedroom Window")
	e.track("sensor.humid", monitor.CategoryHumidity, "banana", "%", "Bad Humidity")
	e.track("binary_sensor.leak", monitor.CategoryLeak, "on", "", "Basement Leak")
	for _, id := range []string{"binary_sensor.door", "sensor.temp", "binary_sensor.window", "sensor.humid"} {
		e.grant(t, id, entities.AccessRead)
	}

	got := e.run(user, "sensors", "", nil)

	mustContain(t, got, "Front Door", "open (on)", "Living Temp", "21.5 \u00b0C", "unavailable (not reporting)", "invalid value (reported: banana)", "Page 1 of 1")
	if strings.Contains(got, "closed") || strings.Contains(got, "Basement") || strings.Contains(got, "binary_sensor.leak") {
		t.Errorf("an unreliable value looked safe, or an unshared sensor leaked:\n%s", got)
	}
	mustContain(t, got, "last known values", "may be outdated") // the monitor is not connected
}

func TestSensors_escapeHostileNamesFromHomeAssistant(t *testing.T) {
	e := newMonitorEnv(t)
	e.track("sensor.temp", monitor.CategoryTemperature, "1", "", "**@everyone** <@123456789> [x](http://evil)")
	e.grant(t, "sensor.temp", entities.AccessRead)

	got := e.run(user, "sensors", "", nil)

	// A backslash neutralizes the character after it: strip escaped pairs and
	// make sure no active markdown, mention or link syntax remains.
	active := regexp.MustCompile(`\\.`).ReplaceAllString(got, "")
	for _, live := range []string{"<@123456789>", "@everyone", "[x](", "**@"} {
		if strings.Contains(active, live) {
			t.Errorf("hostile text %q reached Discord as active syntax:\n%s", live, got)
		}
	}
}

func TestSensors_requireAnAllowlistedUser_andExplainTheNotYetSynchronizedState(t *testing.T) {
	e := newMonitorEnv(t)
	e.track("sensor.temp", monitor.CategoryTemperature, "1", "", "T")

	stranger := e.run(auth.Actor{UserID: "9", Level: auth.LevelNone}, "sensors", "", nil)
	empty := e.run(user, "sensors", "", nil)

	mustContain(t, stranger, "not authorized")
	if strings.Contains(stranger, "sensor.temp") {
		t.Error("a stranger saw sensor data")
	}
	// This monitor never ran, so nothing was loaded: that must not be reported as
	// "nothing is shared". The genuine empty state is covered once synchronized
	// (monitoring_startup_test.go).
	mustContain(t, empty, "not available yet")
	if strings.Contains(empty, "No monitored sensors are shared with you") {
		t.Errorf("claimed nothing is shared before any synchronization:\n%s", empty)
	}
}

func TestSensors_revocationHidesTheSensorOnTheNextCall(t *testing.T) {
	e := newMonitorEnv(t)
	e.track("binary_sensor.door", monitor.CategoryDoor, "on", "", "Door")
	e.grant(t, "binary_sensor.door", entities.AccessRead)
	before := e.run(user, "sensors", "", nil)

	if _, err := e.perms.Revoke(context.Background(), "binary_sensor.door"); err != nil {
		t.Fatal(err)
	}
	after := e.run(user, "sensors", "", nil)

	mustContain(t, before, "Door", "open")
	if strings.Contains(after, "Door") || strings.Contains(after, "binary_sensor.door") {
		t.Errorf("a revoked sensor was still shown:\n%s", after)
	}
}

func TestSensors_useAliasesAndWorkWithoutMonitoring(t *testing.T) {
	e := newMonitorEnv(t)
	e.track("sensor.temp", monitor.CategoryTemperature, "20", "\u00b0C", "Home Assistant Name")
	e.grant(t, "sensor.temp", entities.AccessRead)
	if err := e.aliases.Set(context.Background(), entities.AliasSet{ID: "sensor.temp", Alias: "Kitchen", By: admin.UserID, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	plain := newEnv(t)

	withAlias := e.run(user, "sensors", "", nil)
	notWired := plain.run(user, "sensors", "", nil)

	mustContain(t, withAlias, "Kitchen")
	if strings.Contains(withAlias, "Home Assistant Name") {
		t.Error("the alias must replace the Home Assistant name in the listing")
	}
	mustContain(t, notWired, "not running")
}

type stubMonitoring struct{ st monitor.Status }

func (s stubMonitoring) Status() monitor.Status { return s.st }
func (stubMonitoring) Sensors(context.Context, auth.Actor, int) (monitor.Page, error) {
	return monitor.Page{}, nil
}

func healthWith(t *testing.T, st monitor.Status) string {
	t.Helper()
	e := newEnvWith(t, func(*entities.Permissions, *entities.Aliases) commands.Monitoring { return stubMonitoring{st} })
	return e.run(user, "health", "", nil)
}

func TestHealth_reportsTheFiveLayersHonestly(t *testing.T) {
	now := time.Now()
	healthy := healthWith(t, monitor.Status{State: monitor.StateReady, Healthy: true, Transport: true, Authenticated: true, Subscribed: true, Synchronized: true, Tracked: 7, LastSyncAt: now, LastEventAt: now})
	syncing := healthWith(t, monitor.Status{State: monitor.StateSynchronizing, Transport: true, Authenticated: true, Subscribed: true})
	silent := healthWith(t, monitor.Status{State: monitor.StateReady, Transport: true, Authenticated: true, Subscribed: true, Synchronized: true, Tracked: 3})
	network := healthWith(t, monitor.Status{State: monitor.StateReconnecting, LastFailure: monitor.FailureNetwork, NextRetryAt: now.Add(time.Minute), Tracked: 3, Reconnects: 4})
	credentials := healthWith(t, monitor.Status{State: monitor.StateReconnecting, LastFailure: monitor.FailureAuthentication})

	mustContain(t, healthy, "Healthy: 7 sensors tracked", "synchronized", "last event", "Transport \u2705", "Snapshot \u2705")
	mustContain(t, syncing, "Starting (synchronizing)", "Subscribed \u2705", "Snapshot \u274C")
	if strings.Contains(syncing, "Healthy") {
		t.Errorf("a monitor that is subscribed but not synchronized must not be reported healthy:\n%s", syncing)
	}
	mustContain(t, silent, "Connected but silent", "may be outdated")
	if strings.Contains(silent, "Healthy") {
		t.Errorf("a connected but silent monitor must not be healthy:\n%s", silent)
	}
	mustContain(t, network, "Cannot reach Home Assistant", "next attempt", "Last known values may be outdated", "Reconnects: 4")
	mustContain(t, credentials, "rejected the access token", "HA_TOKEN")
}

func TestMonitoringText_neverLeaksInfrastructureOrSecrets(t *testing.T) {
	for _, st := range []monitor.Status{
		{State: monitor.StateReconnecting, LastFailure: monitor.FailureNetwork},
		{State: monitor.StateReconnecting, LastFailure: monitor.FailureProtocol},
		{State: monitor.StateReconnecting, LastFailure: monitor.FailureAuthentication},
		{State: monitor.StateConnecting}, {State: monitor.StateStopped}, {State: monitor.StateDisconnected},
	} {
		text := commands.MonitoringText(&st)
		for _, forbidden := range []string{"http", "ws://", "127.0.0.1", "token=", "Bearer", "error:"} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s: monitoring text contains %q: %s", st.State, forbidden, text)
			}
		}
	}
	if got := commands.MonitoringText(nil); !strings.Contains(got, "Not running") {
		t.Errorf("nil status = %q", got)
	}
}
