package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/homeassistant/ws"
	"github.com/nepthass/nepthass/tests/fakeha"
)

func TestMonitor_synchronizesThenReportsHealthy_withEveryLayerTrue(t *testing.T) {
	r := newRig(t, nil).start()

	st := r.waitReady()

	if !st.Transport || !st.Authenticated || !st.Subscribed || !st.Synchronized || !st.Healthy {
		t.Errorf("layers = %+v", st)
	}
	if st.Tracked != seededSensors || st.LastSyncAt.IsZero() || st.LastFailure != FailureNone {
		t.Errorf("tracked=%d lastSync=%v failure=%q", st.Tracked, st.LastSyncAt, st.LastFailure)
	}
	if _, ok := r.m.Tracker().Get("light.kitchen"); ok {
		t.Error("a light must not be tracked")
	}
	if _, ok := r.m.Tracker().Get("sensor.uptime"); ok {
		t.Error("a sensor without a device class must not be tracked: classification uses metadata, not the ID")
	}
	if s := r.srv.WSStats(); s.Subscribed != 1 || s.Connections != 1 {
		t.Errorf("server stats = %+v", s)
	}
}

func TestHealth_isNotHealthyUntilTheSnapshotIsSynchronized(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Script(func(sc *fakeha.Script) { sc.HoldStates = true })
	r.start()
	if err := r.srv.WaitWS(r.ctx, func(s fakeha.WSStats) bool { return s.StateRequests == 1 }); err != nil {
		t.Fatal(err)
	}

	held := r.m.Status()
	r.srv.ReleaseStates()
	ready := r.waitReady()

	if held.State != StateSynchronizing || !held.Transport || !held.Authenticated || !held.Subscribed || held.Synchronized || held.Healthy {
		t.Errorf("a connected, authenticated, subscribed monitor that has no snapshot yet must not be healthy: %+v", held)
	}
	if held.Tracked != 0 {
		t.Errorf("nothing is exposed before synchronization, tracked=%d", held.Tracked)
	}
	if !ready.Synchronized || ready.Tracked != seededSensors {
		t.Errorf("after release: %+v", ready)
	}
}

func TestMonitor_appliesLiveEvents_includingAttributeOnlyChangesAndRemoval(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()

	r.srv.Update("binary_sensor.door", "on", doorAttrs)
	r.waitSensor("binary_sensor.door", "on")
	opened, _ := r.m.Tracker().Get("binary_sensor.door")
	r.srv.Update("binary_sensor.door", "on", map[string]any{"device_class": "door", "friendly_name": "Front Door"}) // attribute-only
	r.wait(func(Status) bool { s, _ := r.m.Tracker().Get("binary_sensor.door"); return s.Name == "Front Door" })
	renamed, _ := r.m.Tracker().Get("binary_sensor.door")
	r.srv.Remove("sensor.humidity")
	r.wait(func(Status) bool { _, ok := r.m.Tracker().Get("sensor.humidity"); return !ok })

	if opened.Interpretation().Meaning != MeaningOpen || opened.Revision != 2 {
		t.Errorf("opened = %+v (%v)", opened, opened.Interpretation())
	}
	if renamed.Raw != "on" || !renamed.LastChanged.Equal(opened.LastChanged) || !renamed.LastUpdated.After(opened.LastUpdated) || renamed.Revision != 3 {
		t.Errorf("an attribute-only update must keep last_changed and move last_updated: %+v vs %+v", renamed, opened)
	}
	if st := r.m.Status(); st.LastEventAt.IsZero() || st.Tracked != seededSensors-1 {
		t.Errorf("status = %+v", st)
	}
}

func TestReconcile_eventsDuringTheSnapshotAreNeitherLostNorOverwritten(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Script(func(sc *fakeha.Script) { sc.HoldStates = true })
	r.start()
	if err := r.srv.WaitWS(r.ctx, func(s fakeha.WSStats) bool { return s.StateRequests == 1 }); err != nil {
		t.Fatal(err)
	}
	// The snapshot was captured when get_states arrived. Now, before it is delivered:
	stale := fakeha.StateObject("sensor.temp", "99", tempAttrs, base.Add(-time.Hour), base.Add(-time.Hour))
	r.srv.Update("binary_sensor.door", "on", doorAttrs)                             // newer than the snapshot: must survive
	r.srv.EmitStateChanged("sensor.temp", nil, stale)                               // older than the snapshot: must be dropped
	r.srv.EmitStateChanged("sensor.humidity", nil, r.srv.Object("sensor.humidity")) // identical to the snapshot: a duplicate

	r.srv.ReleaseStates()
	st := r.waitReady()

	door, _ := r.m.Tracker().Get("binary_sensor.door")
	temp, _ := r.m.Tracker().Get("sensor.temp")
	if door.Raw != "on" || door.Revision != 2 {
		t.Errorf("door = %+v: an event received during the snapshot was lost or overwritten", door)
	}
	if temp.Raw != "21.5" {
		t.Errorf("temp = %q: an event older than the snapshot overwrote it", temp.Raw)
	}
	if st.StaleEvents < 1 || st.DuplicateEvents < 1 {
		t.Errorf("stale=%d duplicate=%d, want both counted", st.StaleEvents, st.DuplicateEvents)
	}
}

func TestFailure_unavailableAtStartup_retriesUntilItIsUp(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Down()
	r.start()

	down := r.wait(func(s Status) bool { return s.State == StateReconnecting && s.Reconnects >= 2 })
	r.srv.Up()
	up := r.waitReady()

	if down.LastFailure != FailureNetwork || down.Healthy || down.Tracked != 0 {
		t.Errorf("while down: %+v", down)
	}
	if up.Tracked != seededSensors {
		t.Errorf("after recovery: %+v", up)
	}
}

func TestFailure_dropDuringMonitoring_marksValuesStaleThenReconcilesWhatChangedMeanwhile(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()

	r.srv.Down() // drops the connection and refuses new ones
	down := r.wait(func(s Status) bool { return s.State == StateReconnecting })
	cached, stillCached := r.m.Tracker().Get("sensor.temp")
	r.srv.Set("sensor.temp", "30", tempAttrs) // changes while NeptHass cannot see them
	r.srv.Remove("binary_sensor.motion")      // an entity disappears meanwhile
	r.srv.Up()
	up := r.waitReady()

	if down.Healthy || down.Transport || down.LastFailure != FailureNetwork {
		t.Errorf("during the outage: %+v", down)
	}
	if !stillCached || cached.Raw != "21.5" {
		t.Errorf("the last known value stays readable (marked stale by Healthy=false): %+v %v", cached, stillCached)
	}
	temp, _ := r.m.Tracker().Get("sensor.temp")
	if _, motionKept := r.m.Tracker().Get("binary_sensor.motion"); temp.Raw != "30" || motionKept || up.Tracked != seededSensors-1 {
		t.Errorf("reconciliation must adopt the authoritative snapshot: temp=%q motionKept=%v tracked=%d", temp.Raw, motionKept, up.Tracked)
	}
}

func TestFailure_homeAssistantRestart_neverDuplicatesTheSubscription(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()

	for range 3 {
		r.srv.Down()
		r.wait(func(s Status) bool { return s.State == StateReconnecting })
		r.srv.Up()
		r.waitReady()
	}

	s := r.srv.WSStats()
	if s.MaxSubscribed != 1 || s.Connections < 4 {
		t.Errorf("server stats = %+v: more than one subscription was ever active, or it did not reconnect", s)
	}
}

func TestFailure_invalidToken_usesTheSlowScheduleAndNeverBusyLoops(t *testing.T) {
	sleeps := newRecSleep(4)
	r := newRig(t, func(c *Config) {
		c.HomeAssistant.Token = "wrong-token"
		c.Sleep = sleeps.sleep
	}).start()

	delays := sleeps.waitFor(t, 5)
	st := r.wait(func(s Status) bool { return s.State == StateReconnecting })

	want := []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 80 * time.Millisecond, 80 * time.Millisecond}
	for i, w := range want {
		if delays[i] != w {
			t.Errorf("delay %d = %v, want %v (authentication failures retry slowly and cap)", i, delays[i], w)
		}
	}
	if st.LastFailure != FailureAuthentication || st.Healthy || st.Tracked != 0 {
		t.Errorf("status = %+v", st)
	}
	if s := r.srv.WSStats(); s.AuthRejected != 5 || s.Subscribed != 0 {
		t.Errorf("attempts = %+v: exactly one attempt per scheduled wait, and no subscription ever", s)
	}
}

func TestFailure_malformedJSON_dropsTheConnectionAndRecovers(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()

	r.srv.SendText("{this is not json")
	lost := r.wait(func(s Status) bool { return s.Reconnects >= 1 })
	back := r.waitReady()

	if lost.LastFailure != FailureProtocol || back.Tracked != seededSensors {
		t.Errorf("lost=%+v back=%+v", lost, back)
	}
}

func TestFailure_oversizedMessage_failsTheSyncAndRecoversWhenItShrinks(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Session.MaxMessageBytes = 4096 })
	r.srv.Script(func(sc *fakeha.Script) { sc.PadSnapshotBytes = 20_000 })
	r.start()

	r.wait(func(s Status) bool { return s.Reconnects >= 2 })
	failed := r.m.Status()
	r.srv.Script(func(sc *fakeha.Script) { sc.PadSnapshotBytes = 0 })
	ready := r.waitReady()

	if failed.LastFailure != FailureProtocol || failed.Healthy {
		t.Errorf("failed = %+v", failed)
	}
	if ready.Tracked != seededSensors {
		t.Errorf("ready = %+v", ready)
	}
}

func TestFailure_unexpectedEntityID_isCountedAndDropped(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()
	good := fakeha.StateObject("sensor.temp", "22", tempAttrs, base.Add(time.Hour), base.Add(time.Hour))

	r.srv.EmitStateChanged("Not A Valid ID!", nil, good)
	r.srv.EmitStateChanged("sensor."+strings.Repeat("a", 300), nil, good)
	r.srv.EmitStateChanged("sensor.other", nil, good) // new_state names a different entity
	r.srv.EmitStateChanged("sensor.temp", nil, good)  // a valid event after them proves they were all processed
	r.waitSensor("sensor.temp", "22")

	st := r.m.Status()
	if st.InvalidMessages < 3 || st.State != StateReady || !st.Healthy {
		t.Errorf("status = %+v: invalid events must be counted without hurting the connection", st)
	}
	if _, leaked := r.m.Tracker().Get("sensor.other"); leaked {
		t.Error("an event whose new_state names another entity was applied")
	}
}

func TestFailure_subscriptionRejected_isAProtocolFailure_andRecovers(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Script(func(sc *fakeha.Script) { sc.RejectSubscribe = true })
	r.start()

	rejected := r.wait(func(s Status) bool { return s.Reconnects >= 2 })
	subscribedWhileRejected := r.srv.WSStats().MaxSubscribed
	r.srv.Script(func(sc *fakeha.Script) { sc.RejectSubscribe = false })
	r.waitReady()

	if rejected.LastFailure != FailureProtocol || rejected.Subscribed || rejected.Healthy || subscribedWhileRejected != 0 {
		t.Errorf("rejected = %+v subscribed=%d", rejected, subscribedWhileRejected)
	}
}

func TestFailure_slowConsumer_boundedProcessing_thenAnEvictionResynchronizes(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()
	const flood = 3000

	for i := 1; i <= flood; i++ {
		r.srv.Update("sensor.temp", fmt.Sprint(i), tempAttrs)
	}
	r.waitSensor("sensor.temp", fmt.Sprint(flood))
	flooded := r.m.Status()
	r.srv.DisconnectAll(true) // what Home Assistant does to a client that cannot keep up
	r.wait(func(s Status) bool { return s.Reconnects >= 1 })
	r.srv.Set("sensor.temp", "after-eviction", tempAttrs)
	r.waitReady()
	r.waitSensor("sensor.temp", "after-eviction")

	if flooded.DroppedEvents != 0 || !flooded.Healthy || flooded.Tracked != seededSensors {
		t.Errorf("after the flood: %+v", flooded)
	}
	if applied := r.m.Tracker().Counters().Applied; applied < flood {
		t.Errorf("applied = %d, want every one of the %d events processed in order", applied, flood)
	}
}

func TestFailure_rapidReconnectCycles_growTheDelay_andNeverStackConnections(t *testing.T) {
	sleeps := newRecSleep(100)
	r := newRig(t, func(c *Config) { c.Sleep = sleeps.sleep }).start()

	for i := 0; i < 5; i++ {
		r.wait(func(s Status) bool { return s.State == StateReady && s.Reconnects == uint64(i) })
		r.srv.DisconnectAll(false)
		r.wait(func(s Status) bool { return s.Reconnects >= uint64(i+1) })
	}
	delays := sleeps.waitFor(t, 5)[:5]

	want := []time.Duration{2, 4, 8, 16, 16}
	for i, w := range want {
		if delays[i] != w*time.Millisecond {
			t.Errorf("delay %d = %v, want %v: a server that keeps dropping us must see growing delays", i, delays[i], w*time.Millisecond)
		}
	}
	r.waitReady()
	if s := r.srv.WSStats(); s.MaxSubscribed != 1 || s.Active != 1 {
		t.Errorf("server stats = %+v: connections or subscriptions stacked up", s)
	}
}

func TestFailure_shutdownWhileReconnecting_returnsPromptly(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Backoff.Initial, c.Backoff.Max = time.Hour, time.Hour })
	r.srv.Down()
	r.start()
	r.wait(func(s Status) bool { return s.State == StateReconnecting && !s.NextRetryAt.IsZero() })

	r.cancel()

	select {
	case <-r.m.Done():
	case <-time.After(waitTimeout):
		t.Fatal("Run ignored the cancellation while waiting to reconnect")
	}
	if st := r.m.Status(); st.State != StateStopped {
		t.Errorf("state = %s", st.State)
	}
}

func TestFailure_shutdownWhileSynchronizing_closesTheSocket(t *testing.T) {
	r := newRig(t, nil)
	r.srv.Script(func(sc *fakeha.Script) { sc.HoldStates = true })
	r.start()
	if err := r.srv.WaitWS(r.ctx, func(s fakeha.WSStats) bool { return s.StateRequests == 1 }); err != nil {
		t.Fatal(err)
	}

	r.cancel()

	select {
	case <-r.m.Done():
	case <-time.After(waitTimeout):
		t.Fatal("Run did not stop while the snapshot was pending")
	}
	r.srv.ReleaseStates() // lets the fake's parked handler notice the closed socket
	if st := r.m.Status(); st.State != StateStopped || st.Healthy || st.Synchronized {
		t.Errorf("status = %+v", st)
	}
}

func TestMonitor_neverLogsTheTokenOrSensorValues(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	secret := "super-secret-token-value-8675309"
	srv := fakeha.New(t, secret)
	seed(srv)
	m, err := New(Config{
		HomeAssistant: config.HomeAssistant{URL: srv.URL, Token: config.Secret(secret)}, Logger: logger,
		Backoff: Backoff{Initial: time.Millisecond, Max: 4 * time.Millisecond, AuthInitial: time.Millisecond, AuthMax: time.Millisecond, StableAfter: time.Hour},
		Session: ws.Options{HandshakeTimeout: time.Second, PingInterval: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	wait := func(pred func(Status) bool) {
		wctx, wcancel := context.WithTimeout(context.Background(), waitTimeout)
		defer wcancel()
		if err := m.WaitFor(wctx, pred); err != nil {
			t.Fatalf("%v: %+v", err, m.Status())
		}
	}
	wait(func(s Status) bool { return s.State == StateReady })
	srv.Update("sensor.temp", "12345.678", tempAttrs)
	srv.SendText("{bad json with secret-looking 8675309 content")
	wait(func(s Status) bool { return s.Reconnects >= 1 })
	wait(func(s Status) bool { return s.State == StateReady })
	cancel()
	<-m.Done()

	out := logs.String()
	for _, forbidden := range []string{secret, "access_token", "12345.678", "secret-looking", "Bearer"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the log contains %q:\n%s", forbidden, out)
		}
	}
	for _, expected := range []string{"authenticated", "subscription active", "synchronization completed", "reconnect scheduled", "monitoring stopped"} {
		if !strings.Contains(out, expected) {
			t.Errorf("the log lacks the %q event:\n%s", expected, out)
		}
	}
}

func TestStatus_healthRequiresRecentProofOfLife(t *testing.T) {
	var skew atomic.Int64
	r := newRig(t, func(c *Config) {
		c.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
		c.Liveness = time.Minute
	}).start()
	r.waitReady()

	skew.Store(int64(10 * time.Minute)) // the connection has been silent for far too long
	silent := r.m.Status()

	if silent.State != StateReady || silent.Healthy {
		t.Errorf("a ready but silent connection must not be reported healthy: %+v", silent)
	}
}

func TestRun_isSingleUse_andNewRejectsAnUnusableURL(t *testing.T) {
	r := newRig(t, nil).start()
	r.waitReady()
	second := make(chan struct{})

	go func() { r.m.Run(r.ctx); close(second) }()

	select {
	case <-second:
	case <-time.After(waitTimeout):
		t.Fatal("a second Run must return immediately instead of opening a second connection")
	}
	if s := r.srv.WSStats(); s.Connections != 1 {
		t.Errorf("connections = %d", s.Connections)
	}
	for _, bad := range []string{"", "ftp://h", "http://"} {
		if _, err := New(Config{HomeAssistant: config.HomeAssistant{URL: bad}}); err == nil {
			t.Errorf("New accepted %q", bad)
		}
	}
}
