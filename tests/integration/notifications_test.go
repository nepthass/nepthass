//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

// These tests run the real chain Home Assistant -> monitor -> notification
// engine -> Sender. The Sender is a recorder: delivery to Discord is covered by
// the stub-Discord tests in internal/discord, and real Discord is NOT exercised.

const (
	nGuild   = "100000000000000001"
	nChannel = "200000000000000002"
)

type recSender struct {
	mu      sync.Mutex
	msgs    []notify.Message
	changed chan struct{}
	gate    chan struct{}
	script  func(n int) error
}

func newRecSender() *recSender { return &recSender{changed: make(chan struct{})} }

func (s *recSender) Send(ctx context.Context, m notify.Message) error {
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	n, gate, script := len(s.msgs), s.gate, s.script
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if script != nil {
		return script(n)
	}
	return nil
}

func (s *recSender) all() []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]notify.Message(nil), s.msgs...)
}

func (s *recSender) wait(t *testing.T, n int) []notify.Message {
	t.Helper()
	deadline := time.After(monitorWait)
	for {
		s.mu.Lock()
		got, ch := append([]notify.Message(nil), s.msgs...), s.changed
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("only %d of %d notifications arrived: %+v", len(got), n, got)
		}
	}
}

// until polls a real, networked system with a bounded deadline, like waitForState.
func until(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(monitorWait)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type changeHook struct {
	fn atomic.Pointer[func(monitor.Change)]
}

func (h *changeHook) OnChange(c monitor.Change) {
	if f := h.fn.Load(); f != nil {
		(*f)(c)
	}
}

type itClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*itWaiter
	changed chan struct{}
}

type itWaiter struct {
	at time.Time
	ch chan time.Time
}

func newItClock() *itClock { return &itClock{now: time.Now(), changed: make(chan struct{})} }

func (c *itClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *itClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &itWaiter{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		w.ch <- c.now
		return w.ch
	}
	c.waiters = append(c.waiters, w)
	close(c.changed)
	c.changed = make(chan struct{})
	return w.ch
}

func (c *itClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

func (c *itClock) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(monitorWait)
	for {
		c.mu.Lock()
		got, ch := len(c.waiters), c.changed
		c.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("only %d of %d goroutines are sleeping on the clock", got, n)
		}
	}
}

type nrig struct {
	stack
	mon *monitor.Monitor
	svc *notify.Service
	eng *notify.Engine
	snd *recSender
}

type rigOpts struct {
	path      string // reuse a database (a restart)
	monURL    string
	clock     notify.Clock
	configure bool // set the channel (a first start) rather than rely on persisted settings
	mod       func(*notify.EngineConfig)
	snd       *recSender
}

// startRig wires the production order: the engine is built and reloaded from the
// persisted configuration BEFORE the monitor runs, so the monitor's initial
// snapshot reaches it exactly as in the application.
func startRig(t *testing.T, o rigOpts) *nrig {
	t.Helper()
	if o.path == "" {
		o.path = filepath.Join(t.TempDir(), "nepthass-notify-it.db")
	}
	if o.monURL == "" {
		o.monURL = target.url
	}
	if o.snd == nil {
		o.snd = newRecSender()
	}
	s := openStack(t, o.path)
	hook := &changeHook{}
	mon := newMonitorHook(t, o.monURL, target.token, hook.OnChange)
	store := notify.NewStore(s.db)
	svc := notify.NewService(notify.Deps{GuildID: nGuild, DB: s.db, Store: store, Permissions: s.perms, Sensors: mon.Tracker(), Audit: s.audit, Now: time.Now})
	cfg := notify.EngineConfig{Rate: 1000, Burst: 1000, HealthTick: 50 * time.Millisecond, DrainTimeout: 200 * time.Millisecond,
		DigestMinInterval: 20 * time.Millisecond, BackoffInitial: 20 * time.Millisecond, BackoffMax: 40 * time.Millisecond}
	if o.mod != nil {
		o.mod(&cfg)
	}
	eng, err := notify.NewEngine(notify.EngineDeps{Store: store, GuildID: nGuild, Access: s.perms, Sensors: mon.Tracker(), Aliases: s.aliases,
		Status: mon, Sender: o.snd, Clock: o.clock, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	f := eng.OnChange
	hook.fn.Store(&f)
	if o.configure {
		if err := svc.SetChannel(context.Background(), adminActor, nChannel); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { eng.Run(runCtx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(monitorWait):
			t.Error("the notification engine did not stop")
		}
	})
	runMonitor(t, mon)
	waitMonitor(t, mon, healthy)
	return &nrig{stack: s, mon: mon, svc: svc, eng: eng, snd: o.snd}
}

func (r *nrig) rule(t *testing.T, id string, kind notify.Kind, thr *float64, cooldown time.Duration) notify.Rule {
	t.Helper()
	r.allow(t, id, entities.AccessRead)
	rule, err := r.svc.AddRule(context.Background(), adminActor, notify.NewRule{Entity: entities.ID(id), Kind: kind, Threshold: thr, Cooldown: &cooldown})
	if err != nil {
		t.Fatalf("AddRule(%s): %v", id, err)
	}
	if err := r.eng.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return rule
}

func scenario(t *testing.T, name string) {
	t.Helper()
	if err := target.admin.runScript(context.Background(), "nepthass_dev_scenario_"+name); err != nil {
		t.Fatalf("scenario %s: %v", name, err)
	}
}

func f64(v float64) *float64 { return &v }

func baseline(t *testing.T, r *nrig, id string) float64 {
	t.Helper()
	s, ok := r.mon.Tracker().Get(entities.ID(id))
	if !ok {
		t.Fatalf("%s is not monitored", id)
	}
	v, ok := s.Interpretation().Value, s.Interpretation().HasValue
	if !ok {
		t.Fatalf("%s has no numeric value: %+v", id, s)
	}
	return v
}

func resetFor(t *testing.T) {
	resetSimulation(t)
	t.Cleanup(func() { resetSimulation(t) })
}

func TestNotify_doorOpensAndCloses_onlyTheOpeningAlerts(t *testing.T) {
	resetFor(t)
	r := startRig(t, rigOpts{configure: true})
	r.rule(t, door, notify.KindState, nil, 0)
	scenario(t, "door_opened")
	m := r.snd.wait(t, 1)[0]
	if m.Kind != notify.MsgAlert || m.Category != monitor.CategoryDoor || m.Meaning != monitor.MeaningOpen || m.Name == "" || m.Late || m.Uncertain {
		t.Fatalf("door alert: %+v", m)
	}
	resetSimulation(t) // the door closes: no alert
	scenario(t, "door_opened")
	r.snd.wait(t, 2)
	if h := r.eng.Health(); h.Enqueued != 2 || len(r.snd.all()) != 2 {
		t.Errorf("closing must not alert, only the two openings: %+v", h)
	}
}

func TestNotify_motionAndWaterLeak_areDetected_theLeakOnTheCriticalLane(t *testing.T) {
	resetFor(t)
	r := startRig(t, rigOpts{configure: true})
	r.rule(t, motion, notify.KindState, nil, 0)
	r.rule(t, waterLeak, notify.KindState, nil, 0)
	scenario(t, "motion_detected")
	if m := r.snd.wait(t, 1)[0]; m.Category != monitor.CategoryMotion || m.Meaning != monitor.MeaningDetected {
		t.Fatalf("motion: %+v", m)
	}
	scenario(t, "water_leak")
	leak := r.snd.wait(t, 2)[1]
	if leak.Category != monitor.CategoryLeak || leak.Meaning != monitor.MeaningWet || leak.Lane() != notify.LaneCritical {
		t.Errorf("a wet leak sensor alerts on the critical lane: %+v", leak)
	}
}

func TestNotify_aLeakThatComesBackWetAfterBeingUnavailable_failsLoud(t *testing.T) {
	resetFor(t)
	r := startRig(t, rigOpts{configure: true})
	r.rule(t, waterLeak, notify.KindState, nil, 0)
	scenario(t, "sensors_offline")
	waitSensor(t, r.mon, waterLeak, "unavailable")
	scenario(t, "water_leak") // wet underneath, still offline: nothing to report yet
	if err := target.admin.call(context.Background(), "input_boolean", "turn_off", map[string]any{"entity_id": "input_boolean.nepthass_dev_offline_leak"}); err != nil {
		t.Fatal(err)
	}
	m := r.snd.wait(t, 1)[0]
	if m.Category != monitor.CategoryLeak || m.Meaning != monitor.MeaningWet || !m.Uncertain {
		t.Errorf("unavailable to wet must alert, marked uncertain: %+v", m)
	}
}

func TestNotify_temperatureAboveAndBatteryBelow_reportTheReadingAndTheThreshold(t *testing.T) {
	resetFor(t)
	r := startRig(t, rigOpts{configure: true})
	temp := baseline(t, r, temperature)
	batt := baseline(t, r, battery)
	r.rule(t, temperature, notify.KindAbove, f64(temp+1), 0)
	r.rule(t, battery, notify.KindBelow, f64(batt-1), 0)
	scenario(t, "high_temperature")
	hot := r.snd.wait(t, 1)[0]
	if hot.Trigger != notify.KindAbove || !hot.HasValue || hot.Value <= temp+1 || hot.Threshold == nil || *hot.Threshold != temp+1 {
		t.Fatalf("temperature above: %+v", hot)
	}
	scenario(t, "low_battery")
	low := r.snd.wait(t, 2)[1]
	if low.Trigger != notify.KindBelow || low.Category != monitor.CategoryBattery || low.Value >= batt-1 {
		t.Errorf("battery below: %+v", low)
	}
}

func TestNotify_cooldownSuppressesARepeat_andRevocationSilencesTheRule(t *testing.T) {
	resetFor(t)
	r := startRig(t, rigOpts{configure: true})
	r.rule(t, door, notify.KindState, nil, 30*time.Minute)
	scenario(t, "door_opened")
	r.snd.wait(t, 1)
	resetSimulation(t)
	scenario(t, "door_opened")
	until(t, "the second opening to be muted by the cooldown", func() bool { return r.eng.Health().SuppressedCooldown == 1 })
	if len(r.snd.all()) != 1 {
		t.Fatalf("cooldown must suppress the repeat: %d", len(r.snd.all()))
	}

	r2 := startRig(t, rigOpts{configure: true})
	r2.rule(t, window, notify.KindState, nil, 0)
	if _, err := r2.perms.Revoke(context.Background(), window); err != nil {
		t.Fatal(err)
	}
	scenario(t, "window_opened")
	until(t, "the revoked entity to be suppressed", func() bool { return r2.eng.Health().SuppressedUnshared == 1 })
	if len(r2.snd.all()) != 0 {
		t.Errorf("a revoked entity must never alert: %+v", r2.snd.all())
	}
}

func TestNotify_anAlreadyOpenDoorIsSilentAtStartup_andOnlyARealTransitionAlerts(t *testing.T) {
	resetFor(t)
	scenario(t, "door_opened")
	waitForState(t, door, "on")
	r := startRig(t, rigOpts{configure: true})
	r.rule(t, door, notify.KindState, nil, 0)
	resetSimulation(t)
	scenario(t, "door_opened")
	m := r.snd.wait(t, 1)
	if len(m) != 1 || len(r.snd.all()) != 1 {
		t.Errorf("the state that existed when NeptHass started must not alert, only the later opening: %+v", r.snd.all())
	}
}

func TestNotify_aRealOutageThatHidesAnOpeningProducesALateAlertAfterReconnect(t *testing.T) {
	resetFor(t)
	proxy := newTCPProxy(t, target.url)
	r := startRig(t, rigOpts{configure: true, monURL: proxy.URL()})
	r.rule(t, door, notify.KindState, nil, 0)
	proxy.SetDown(true)
	waitMonitor(t, r.mon, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting })
	scenario(t, "door_opened") // while NeptHass cannot see Home Assistant
	proxy.SetDown(false)
	waitMonitor(t, r.mon, func(s monitor.Status) bool { return healthy(s) && s.Reconnects >= 1 })
	m := r.snd.wait(t, 1)[0]
	if m.Category != monitor.CategoryDoor || !m.Late {
		t.Errorf("a change found by the reconnect snapshot is reported as late: %+v", m)
	}
}

func TestNotify_aHomeAssistantDisconnectOverFiveMinutes_alertsOnce_andRecoveryFollows(t *testing.T) {
	resetFor(t)
	clk := newItClock()
	proxy := newTCPProxy(t, target.url)
	r := startRig(t, rigOpts{configure: true, monURL: proxy.URL(), clock: clk, mod: func(c *notify.EngineConfig) { c.HealthTick = 10 * time.Second }})
	clk.await(t, 1)
	proxy.SetDown(true)
	waitMonitor(t, r.mon, func(s monitor.Status) bool { return s.State == monitor.StateReconnecting })
	for i := 0; i < 31; i++ { // a little over five minutes, in ten second ticks
		clk.advance(10 * time.Second)
		clk.await(t, 1)
	}
	down := r.snd.wait(t, 1)[0]
	if down.Kind != notify.MsgMonitoringDown || down.DownFor < 5*time.Minute {
		t.Fatalf("one down alert after five continuous minutes: %+v", down)
	}
	proxy.SetDown(false)
	waitMonitor(t, r.mon, func(s monitor.Status) bool { return healthy(s) && s.Reconnects >= 1 })
	clk.advance(10 * time.Second)
	if rec := r.snd.wait(t, 2)[1]; rec.Kind != notify.MsgMonitoringRecovered {
		t.Errorf("recovery notice after a delivered alert: %+v", rec)
	}
}

func TestNotify_aDiscordOutageIsRetriedUntilItRecovers(t *testing.T) {
	resetFor(t)
	snd := newRecSender()
	snd.script = func(n int) error {
		if n <= 2 {
			return notify.Retryable(context.DeadlineExceeded)
		}
		return nil
	}
	r := startRig(t, rigOpts{configure: true, snd: snd})
	r.rule(t, door, notify.KindState, nil, 0)
	scenario(t, "door_opened")
	snd.wait(t, 3)
	until(t, "the alert to be delivered", func() bool { return r.eng.Health().Delivered == 1 })
	if h := r.eng.Health(); h.Retried != 2 || h.DroppedTTL != 0 {
		t.Errorf("two retries and no loss: %+v", h)
	}
}

func TestNotify_aSaturatedQueueDropsOldestAndReportsTheCurrentStateInOneDigest(t *testing.T) {
	resetFor(t)
	snd := newRecSender()
	snd.gate = make(chan struct{}) // Discord stops answering
	r := startRig(t, rigOpts{configure: true, snd: snd, mod: func(c *notify.EngineConfig) { c.NormalCap, c.CriticalCap = 1, 4 }})
	r.rule(t, door, notify.KindState, nil, 0)
	r.rule(t, window, notify.KindState, nil, 0)
	r.rule(t, motion, notify.KindState, nil, 0)
	scenario(t, "door_opened")
	snd.wait(t, 1) // the door alert is in flight and stuck
	scenario(t, "window_opened")
	until(t, "the window alert to be queued", func() bool { return r.eng.Health().QueueNormal == 1 })
	scenario(t, "motion_detected") // evicts the older queued window alert
	until(t, "the overflow to be counted", func() bool { return r.eng.Health().DroppedOverflow == 1 })
	close(snd.gate)
	msgs := snd.wait(t, 3) // the door alert, the motion alert that evicted the window alert, then the digest
	if msgs[1].Category != monitor.CategoryMotion {
		t.Errorf("the queued motion alert is delivered after the stuck one: %+v", msgs[1])
	}
	last := msgs[len(msgs)-1]
	if last.Kind != notify.MsgDigest || len(last.Lines) != 1 || last.Lines[0].Category != monitor.CategoryWindow {
		t.Errorf("one digest of the CURRENT state of the dropped alert (the open window): %+v", last)
	}
}

func TestNotify_configurationSurvivesARestart_andTheOpenDoorAtRestartStaysSilent(t *testing.T) {
	resetFor(t)
	path := filepath.Join(t.TempDir(), "restart.db")
	first := startRig(t, rigOpts{path: path, configure: true})
	first.rule(t, door, notify.KindState, nil, 0)
	first.rule(t, motion, notify.KindState, nil, 0)
	scenario(t, "door_opened")
	first.snd.wait(t, 1)

	// "Restart": a new engine and monitor on the same database file, door still open.
	second := startRig(t, rigOpts{path: path})
	rules, err := second.svc.Rules(context.Background(), adminActor)
	if err != nil || len(rules) != 2 || !second.eng.Health().Active {
		t.Fatalf("channel and rules must persist across a restart: %d rules, active=%v, %v", len(rules), second.eng.Health().Active, err)
	}
	scenario(t, "motion_detected")
	m := second.snd.wait(t, 1)
	if len(m) != 1 || m[0].Category != monitor.CategoryMotion || len(second.snd.all()) != 1 {
		t.Errorf("after the restart only the new motion alerts; the door that was already open is silent: %+v", second.snd.all())
	}
}
