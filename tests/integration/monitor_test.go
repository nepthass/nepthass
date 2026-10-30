//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/homeassistant/ws"
	"github.com/nepthass/nepthass/internal/monitor"
)

const monitorWait = 20 * time.Second

// startMonitor runs a Monitor against url (the real Home Assistant, or a proxy
// to it) and stops it when the test ends, proving Run returns.
func startMonitor(t *testing.T, url, token string) *monitor.Monitor {
	t.Helper()
	m := newMonitorHook(t, url, token, nil)
	runMonitor(t, m)
	return m
}

// newMonitorHook builds, without running, a Monitor that delivers its changes to onChange.
func newMonitorHook(t *testing.T, url, token string, onChange func(monitor.Change)) *monitor.Monitor {
	t.Helper()
	m, err := monitor.New(monitor.Config{
		HomeAssistant: config.HomeAssistant{URL: url, Token: config.Secret(token), Timeout: requestTimeout},
		Backoff: monitor.Backoff{
			Initial: 50 * time.Millisecond, Max: 400 * time.Millisecond, AuthInitial: 50 * time.Millisecond, AuthMax: 100 * time.Millisecond,
			StableAfter: time.Hour, Rand: func(n int64) int64 { return n - 1 },
		},
		Session:  ws.Options{HandshakeTimeout: 5 * time.Second, PingInterval: 5 * time.Second, ReadTimeout: 30 * time.Second},
		OnChange: onChange,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// runMonitor runs m and stops it when the test ends, proving Run returns.
func runMonitor(t *testing.T, m *monitor.Monitor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	t.Cleanup(func() {
		cancel()
		select {
		case <-m.Done():
		case <-time.After(monitorWait):
			t.Error("the monitor did not stop")
		}
		if v := m.Violations(); v != 0 {
			t.Errorf("%d illegal state transitions", v)
		}
	})
}

func waitMonitor(t *testing.T, m *monitor.Monitor, pred func(monitor.Status) bool) monitor.Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), monitorWait)
	defer cancel()
	if err := m.WaitFor(ctx, pred); err != nil {
		t.Fatalf("monitoring condition not reached: %v (status %+v)", err, m.Status())
	}
	return m.Status()
}

func healthy(s monitor.Status) bool { return s.State == monitor.StateReady && s.Healthy }

// waitSensor waits, without polling, for a sensor to reach a raw state.
func waitSensor(t *testing.T, m *monitor.Monitor, id, raw string) monitor.Sensor {
	t.Helper()
	waitMonitor(t, m, func(monitor.Status) bool {
		s, ok := m.Tracker().Get(entities.ID(id))
		return ok && s.Raw == raw
	})
	s, _ := m.Tracker().Get(entities.ID(id))
	return s
}

func TestMonitor_synchronizesWithTheRealHomeAssistant_andClassifiesTheVirtualSensors(t *testing.T) {
	resetSimulation(t)
	m := startMonitor(t, target.url, target.token)

	st := waitMonitor(t, m, healthy)

	want := map[string]struct {
		cat     monitor.Category
		raw     string
		unit    string
		meaning monitor.Meaning
	}{
		temperature: {monitor.CategoryTemperature, "21.5", "\u00b0C", monitor.MeaningReading},
		humidity:    {monitor.CategoryHumidity, "45", "%", monitor.MeaningReading},
		battery:     {monitor.CategoryBattery, "87", "%", monitor.MeaningReading},
		motion:      {monitor.CategoryMotion, "off", "", monitor.MeaningClear},
		door:        {monitor.CategoryDoor, "off", "", monitor.MeaningClosed},
		window:      {monitor.CategoryWindow, "off", "", monitor.MeaningClosed},
		waterLeak:   {monitor.CategoryLeak, "off", "", monitor.MeaningDry},
	}
	for id, w := range want {
		s, ok := m.Tracker().Get(entities.ID(id))
		if !ok || s.Category != w.cat || s.Raw != w.raw || s.Unit != w.unit || s.Interpretation().Meaning != w.meaning || s.Name == "" {
			t.Errorf("%s = %+v (tracked=%v), want %+v", id, s, ok, w)
		}
	}
	if _, tracked := m.Tracker().Get(entities.ID(light)); tracked {
		t.Error("a light must not be tracked")
	}
	if !st.Transport || !st.Authenticated || !st.Subscribed || !st.Synchronized || st.Tracked < len(want) || st.LastSyncAt.IsZero() {
		t.Errorf("status = %+v", st)
	}
}

func TestMonitor_receivesRealStateChangedEvents_forEveryScenario(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	m := startMonitor(t, target.url, target.token)
	waitMonitor(t, m, healthy)
	scenarios := []struct {
		script, id, raw string
		meaning         monitor.Meaning
	}{
		{"nepthass_dev_scenario_motion_detected", motion, "on", monitor.MeaningDetected},
		{"nepthass_dev_scenario_door_opened", door, "on", monitor.MeaningOpen},
		{"nepthass_dev_scenario_window_opened", window, "on", monitor.MeaningOpen},
		{"nepthass_dev_scenario_high_temperature", temperature, "31.2", monitor.MeaningReading},
		{"nepthass_dev_scenario_water_leak", waterLeak, "on", monitor.MeaningWet},
		{"nepthass_dev_scenario_low_battery", battery, "12", monitor.MeaningReading},
	}

	for _, sc := range scenarios {
		before, _ := m.Tracker().Get(entities.ID(sc.id))
		if err := target.admin.runScript(context.Background(), sc.script); err != nil {
			t.Fatal(err)
		}
		got := waitSensor(t, m, sc.id, sc.raw)
		if got.Interpretation().Meaning != sc.meaning || got.Revision <= before.Revision || !got.LastUpdated.After(before.LastUpdated) {
			t.Errorf("%s: %+v (before %+v)", sc.script, got, before)
		}
	}
	if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_reset"); err != nil {
		t.Fatal(err)
	}
	waitSensor(t, m, door, "off")
	back := waitSensor(t, m, temperature, "21.5")
	if back.Unit != "\u00b0C" {
		t.Errorf("after the reset the temperature must be back in metric: %+v", back)
	}
	if st := m.Status(); st.LastEventAt.IsZero() || !st.Healthy {
		t.Errorf("status = %+v", st)
	}
}

func TestMonitor_reportsUnavailableSensorsAsNotReporting_neverAsSafe(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	m := startMonitor(t, target.url, target.token)
	waitMonitor(t, m, healthy)

	if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_sensors_offline"); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{motion, door, window, waterLeak, temperature, humidity, battery} {
		s := waitSensor(t, m, id, "unavailable")
		if in := s.Interpretation(); in.Meaning != monitor.MeaningUnavailable || in.Meaning.Reliable() {
			t.Errorf("%s: %+v must be reported as unavailable, never as a safe state", id, in)
		}
	}
	if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_reset"); err != nil {
		t.Fatal(err)
	}
	if s := waitSensor(t, m, door, "off"); s.Interpretation().Meaning != monitor.MeaningClosed {
		t.Errorf("after recovery: %+v", s)
	}
}

func TestMonitor_reconnectsAfterANetworkOutage_andReconcilesChangesItNeverSaw(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	proxy := newTCPProxy(t, target.url)
	m := startMonitor(t, proxy.URL(), target.token)
	waitMonitor(t, m, healthy)

	proxy.SetDown(true)
	down := waitMonitor(t, m, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting })
	// While NeptHass cannot see Home Assistant, the door opens and a window opens and closes again.
	if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_door_opened"); err != nil {
		t.Fatal(err)
	}
	staleDoor, _ := m.Tracker().Get(entities.ID(door))
	proxy.SetDown(false)
	up := waitMonitor(t, m, func(s monitor.Status) bool { return healthy(s) && s.Reconnects >= 1 })

	if down.Healthy || down.Transport || down.LastFailure != monitor.FailureNetwork {
		t.Errorf("during the outage: %+v", down)
	}
	if staleDoor.Raw != "off" {
		t.Errorf("while disconnected the cache cannot know the door opened: %+v", staleDoor)
	}
	if reconciled, _ := m.Tracker().Get(entities.ID(door)); reconciled.Raw != "on" {
		t.Errorf("reconciliation must adopt Home Assistant's current state without any event: %+v", reconciled)
	}
	if up.Tracked < 7 || up.DroppedEvents != 0 {
		t.Errorf("after recovery: %+v", up)
	}
}

func TestMonitor_anInvalidTokenIsACredentialFailure_notANetworkOne(t *testing.T) {
	m := startMonitor(t, target.url, "this-is-not-a-valid-token")

	st := waitMonitor(t, m, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting })

	if st.LastFailure != monitor.FailureAuthentication || st.Healthy || st.Authenticated || st.Tracked != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestMonitor_serviceBoundary_realEventsReachOnlyAuthorizedReaders(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
	s := newStack(t)
	m := startMonitor(t, target.url, target.token)
	waitMonitor(t, m, healthy)
	reader := monitor.NewReader(m, s.perms, s.aliases)
	ctx := context.Background()
	s.allow(t, temperature, entities.AccessRead)

	if err := target.admin.runScript(ctx, "nepthass_dev_scenario_high_temperature"); err != nil {
		t.Fatal(err)
	}
	waitSensor(t, m, temperature, "31.2")
	if err := target.admin.runScript(ctx, "nepthass_dev_scenario_water_leak"); err != nil {
		t.Fatal(err)
	}
	waitSensor(t, m, waterLeak, "on")

	shared, sharedErr := reader.Sensor(ctx, userActor, entities.ID(temperature))
	_, leakErr := reader.Sensor(ctx, userActor, entities.ID(waterLeak))
	list, listErr := reader.Sensors(ctx, userActor, 1)
	_, strangerErr := reader.Sensors(ctx, auth.Actor{UserID: "9", Level: auth.LevelNone}, 1)
	if _, err := s.perms.Revoke(ctx, entities.ID(temperature)); err != nil {
		t.Fatal(err)
	}
	_, afterRevoke := reader.Sensor(ctx, userActor, entities.ID(temperature))

	if sharedErr != nil || shared.Raw != "31.2" || shared.Stale || shared.Unit != "\u00b0C" {
		t.Errorf("shared sensor = %+v, %v", shared, sharedErr)
	}
	if !errors.Is(leakErr, entities.ErrAccessDenied) {
		t.Errorf("an unshared leak sensor must be denied: %v", leakErr)
	}
	if listErr != nil || list.Total != 1 || list.Views[0].ID != entities.ID(temperature) {
		t.Errorf("list = %+v, %v: only the shared sensor may be listed", list, listErr)
	}
	if !errors.Is(strangerErr, auth.ErrNotAuthorized) || !errors.Is(afterRevoke, entities.ErrAccessDenied) {
		t.Errorf("stranger=%v afterRevoke=%v", strangerErr, afterRevoke)
	}
}

func TestMonitor_shutsDownCleanlyWhileHealthy(t *testing.T) {
	m, err := monitor.New(monitor.Config{HomeAssistant: config.HomeAssistant{URL: target.url, Token: config.Secret(target.token)}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	waitMonitor(t, m, healthy)

	cancel()

	select {
	case <-m.Done():
	case <-time.After(monitorWait):
		t.Fatal("Run did not return after cancellation")
	}
	if st := m.Status(); st.State != monitor.StateStopped || st.Healthy || st.Transport {
		t.Errorf("status after shutdown = %+v", st)
	}
}
