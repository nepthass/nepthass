package commands_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// pageMonitoring serves a fixed page, so the footer can be tested for any status.
type pageMonitoring struct{ page monitor.Page }

func (p pageMonitoring) Status() monitor.Status { return p.page.Status }
func (p pageMonitoring) Sensors(context.Context, auth.Actor, int) (monitor.Page, error) {
	return p.page, nil
}

func footerFor(t *testing.T, st monitor.Status) string {
	t.Helper()
	view := monitor.View{Sensor: monitor.Sensor{ID: "sensor.temp", Category: monitor.CategoryTemperature, Name: "Temp", Raw: "21.5", Unit: "\u00b0C", LastChanged: time.Now()}, Stale: !st.Healthy}
	page := monitor.Page{Views: []monitor.View{view}, Page: 1, Pages: 1, Total: 1, Status: st}
	e := newEnvWith(t, func(*entities.Permissions, *entities.Aliases) commands.Monitoring { return pageMonitoring{page} })
	return e.run(user, "sensors", "", nil)
}

func TestSensors_footerSaysLiveOnlyWhileMonitoringIsHealthyAndSynchronized(t *testing.T) {
	now := time.Now()
	healthy := monitor.Status{State: monitor.StateReady, Healthy: true, Synchronized: true, LastSyncAt: now}
	notLive := map[string]monitor.Status{
		"reconnecting":              {State: monitor.StateReconnecting, LastFailure: monitor.FailureNetwork, LastSyncAt: now},
		"connecting after failures": {State: monitor.StateConnecting, Reconnects: 3, LastSyncAt: now},
		"synchronizing":             {State: monitor.StateSynchronizing, Reconnects: 1, LastSyncAt: now},
		"ready but silent":          {State: monitor.StateReady, Synchronized: true, LastSyncAt: now},
		"stopped":                   {State: monitor.StateStopped, LastSyncAt: now},
		"healthy flag without sync": {State: monitor.StateReady, Healthy: true, LastSyncAt: now},
	}

	live := footerFor(t, healthy)

	mustContain(t, live, "Live readings from Home Assistant's WebSocket", "Page 1 of 1")
	if strings.Contains(live, "last known") || strings.Contains(live, "Last known") {
		t.Errorf("a healthy listing must not talk about stale values:\n%s", live)
	}
	for name, st := range notLive {
		t.Run(name, func(t *testing.T) {
			got := footerFor(t, st)

			mustContain(t, got, "Last known values, not live", "Page 1 of 1", "may be outdated")
			if strings.Contains(got, "Live readings") {
				t.Errorf("stale values were called live:\n%s", got)
			}
		})
	}
}

func TestSensors_footerFollowsARealMonitorFromLiveToStaleAndBack(t *testing.T) {
	le := newLiveEnv(t, nil)
	le.shareBoth(t)
	le.start()
	le.wait(t, func(s monitor.Status) bool { return s.Healthy })
	live := le.run(user, "sensors", "", nil)

	le.srv.Down()
	le.wait(t, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting })
	stale := le.run(user, "sensors", "", nil)

	le.srv.Up()
	le.wait(t, func(s monitor.Status) bool { return s.Healthy })
	recovered := le.run(user, "sensors", "", nil)

	mustContain(t, live, "Live readings")
	mustContain(t, stale, "Last known values, not live", "last known values and may be outdated")
	mustContain(t, recovered, "Live readings")
	if strings.Contains(stale, "Live readings") || strings.Contains(recovered, "Last known values") {
		t.Errorf("footer and banner disagree:\nstale:\n%s\nrecovered:\n%s", stale, recovered)
	}
}
