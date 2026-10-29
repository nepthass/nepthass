package discord

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nepthass/nepthass/internal/audit"
	"github.com/nepthass/nepthass/internal/auth"
	"github.com/nepthass/nepthass/internal/discord/commands"
	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
	"github.com/nepthass/nepthass/internal/notify"
)

var (
	adminActor = auth.Actor{UserID: "900000000000000001", Level: auth.LevelAdmin}
	userActor  = auth.Actor{UserID: "900000000000000002", Level: auth.LevelUser}
)

// fakeClk is a manually advanced notify.Clock.
type fakeClk struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*clkWaiter
	changed chan struct{}
}

type clkWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClk() *fakeClk {
	return &fakeClk{now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), changed: make(chan struct{})}
}

func (c *fakeClk) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClk) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &clkWaiter{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		w.ch <- c.now
		return w.ch
	}
	c.waiters = append(c.waiters, w)
	close(c.changed)
	c.changed = make(chan struct{})
	return w.ch
}

func (c *fakeClk) Advance(d time.Duration) {
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

func (c *fakeClk) await(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(stubWait)
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

type statusSrc struct {
	mu sync.Mutex
	st monitor.Status
}

func (s *statusSrc) Status() monitor.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *statusSrc) set(healthy bool) {
	s.mu.Lock()
	s.st = monitor.Status{State: monitor.StateReady, Healthy: healthy}
	s.mu.Unlock()
}

type afix struct {
	*nfix
	svc     *notify.Service
	eng     *notify.Engine
	admin   *NotificationAdmin
	tracker *monitor.Tracker
	perms   *entities.Permissions
	audit   *audit.Log
	stat    *statusSrc
	clk     *fakeClk
	seq     uint64
	now     time.Time
}

func newAFix(t *testing.T, fake bool, mod func(*notify.EngineConfig)) *afix {
	t.Helper()
	f := newNFix(t, false)
	a := &afix{nfix: f, tracker: monitor.NewTracker(), perms: entities.NewPermissions(f.db), audit: audit.New(f.db), stat: &statusSrc{}, now: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	a.stat.set(true)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a.svc = notify.NewService(notify.Deps{GuildID: stubGuild, DB: f.db, Store: f.store, Permissions: a.perms, Sensors: a.tracker, Audit: a.audit, Now: func() time.Time { return a.now }})
	cfg := notify.EngineConfig{Rand: func(n int64) int64 { return n - 1 }}
	if mod != nil {
		mod(&cfg)
	}
	deps := notify.EngineDeps{Store: f.store, GuildID: stubGuild, Access: a.perms, Sensors: a.tracker, Aliases: entities.NewAliases(f.db), Status: a.stat, Sender: f.n, Logger: log, Config: cfg}
	if fake {
		a.clk = newFakeClk()
		deps.Clock = a.clk
	}
	eng, err := notify.NewEngine(deps)
	if err != nil {
		t.Fatal(err)
	}
	a.eng = eng
	a.admin = NewNotificationAdmin(a.svc, eng, f.n, a.tracker, entities.NewAliases(f.db), log)
	return a
}

func (a *afix) sensor(id string, cat monitor.Category, raw string) monitor.Sensor {
	return monitor.Sensor{ID: entities.ID(id), Category: cat, Name: "Front door", Raw: raw, LastChanged: a.now, LastUpdated: a.now, ObservedAt: a.now, Revision: 1}
}

func (a *afix) ready(id string, cat monitor.Category) {
	a.t().Helper()
	if err := a.perms.Set(ctx0, entities.Grant{ID: entities.ID(id), Access: entities.AccessRead, GrantedBy: "admin", At: a.now}); err != nil {
		a.t().Fatal(err)
	}
	a.now = a.now.Add(time.Second)
	a.tracker.Apply(monitor.Update{ID: entities.ID(id), Category: cat, Name: "Front door", Raw: "off", LastChanged: a.now, LastUpdated: a.now}, a.now)
}

func (a *afix) t() *testing.T { return a.stub.t }

var ctx0 = context.Background()

func (a *afix) change(id string, prev, cur string, src monitor.ChangeSource) monitor.Change {
	a.seq++
	c := monitor.Change{ID: entities.ID(id), Seq: a.seq, Source: src, HadPrev: prev != "", Cur: a.sensor(id, monitor.CategoryDoor, cur), At: time.Now()}
	if prev != "" {
		c.Prev = a.sensor(id, monitor.CategoryDoor, prev)
	}
	if a.clk != nil {
		c.At = a.clk.Now()
	}
	return c
}

func (a *afix) run() (cancel func(), done chan struct{}) {
	runCtx, c := context.WithCancel(context.Background())
	d := make(chan struct{})
	go func() { a.eng.Run(runCtx); close(d) }()
	return c, d
}

func zero() *time.Duration { d := time.Duration(0); return &d }

func TestNotificationAdmin_setChannelValidatesBeforeSaving_thenReloadsTheEngine(t *testing.T) {
	a := newAFix(t, false, nil)
	if _, err := a.admin.SetChannel(ctx0, userActor, stubChannel); !errors.Is(err, auth.ErrAdminRequired) {
		t.Fatalf("only administrators: %v", err)
	}
	a.stub.channels["500000000000000005"] = stubChannelInfo{stubGuild, 2, "voice", 0}
	var ue *commands.UserError
	if _, err := a.admin.SetChannel(ctx0, adminActor, "500000000000000005"); !errors.As(err, &ue) {
		t.Fatalf("a voice channel must be rejected with a reason: %v", err)
	}
	if cfg, _ := a.svc.Config(ctx0, adminActor); cfg.Configured || a.eng.Health().Active {
		t.Fatalf("a rejected channel saves nothing and activates nothing: %+v", cfg)
	}
	name, err := a.admin.SetChannel(ctx0, adminActor, stubChannel)
	if err != nil || name != "alerts" {
		t.Fatalf("%q %v", name, err)
	}
	if cfg, _ := a.svc.Config(ctx0, adminActor); !cfg.Active() || !a.eng.Health().Active {
		t.Error("the engine must be reloaded after a successful change")
	}
	if err := a.admin.SetPaused(ctx0, adminActor, true); err != nil || a.eng.Health().Active {
		t.Errorf("pausing reloads the engine into the disabled state: %v", err)
	}
	if err := a.admin.SetPaused(ctx0, adminActor, false); err != nil || !a.eng.Health().Active {
		t.Errorf("resuming reloads it again: %v", err)
	}
	if line := a.admin.HealthLine(); !strings.Contains(line, "Enabled") || strings.Contains(line, stubChannel) {
		t.Errorf("health line for any user: %q", line)
	}
}

func TestE2E_everyConfigurationChangeReachesTheRunningEngine_andTheInitialSnapshotIsSilent(t *testing.T) {
	a := newAFix(t, false, nil)
	a.ready("binary_sensor.door", monitor.CategoryDoor)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	item, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.door", Kind: notify.KindState, Cooldown: zero()})
	if err != nil || item.Name != "Front door" {
		t.Fatalf("%+v %v", item, err)
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()

	a.eng.OnChange(a.change("binary_sensor.door", "", "on", monitor.SourceSnapshot)) // the initial snapshot: a baseline
	a.eng.OnChange(a.change("binary_sensor.door", "on", "off", monitor.SourceLive))
	a.eng.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceLive))
	posts := a.stub.waitPosts(1)
	content, _ := posts[0].Body["content"].(string)
	if !strings.Contains(content, "Front door") || !strings.Contains(content, "is now open") || len(a.stub.postsSnapshot()) != 1 || a.eng.Health().Enqueued != 1 {
		t.Fatalf("the baseline is silent and the real transition is delivered once: %q enqueued=%d", content, a.eng.Health().Enqueued)
	}

	if err := a.admin.SetRuleEnabled(ctx0, adminActor, item.ID, false); err != nil {
		t.Fatal(err)
	}
	a.eng.OnChange(a.change("binary_sensor.door", "on", "off", monitor.SourceLive))
	a.eng.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceLive))
	if h := a.eng.Health(); h.MailboxPending != 0 || h.Enqueued != 1 {
		t.Fatalf("a paused rule is not watched at all: %+v", h)
	}
	if err := a.admin.SetRuleEnabled(ctx0, adminActor, item.ID, true); err != nil {
		t.Fatal(err)
	}
	a.eng.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceLive)) // resumed: re-seeded from the tracker (door off)
	a.stub.waitPosts(2)

	if err := a.admin.RemoveRule(ctx0, adminActor, item.ID); err != nil {
		t.Fatal(err)
	}
	a.eng.OnChange(a.change("binary_sensor.door", "on", "off", monitor.SourceLive))
	a.eng.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceLive))
	if h := a.eng.Health(); h.MailboxPending != 0 || h.Enqueued != 2 {
		t.Errorf("a removed rule is gone from the running engine: %+v", h)
	}
}

func TestE2E_monitoringDown_recoveryAndLateAlertOrder_overTheRealNotifier(t *testing.T) {
	a := newAFix(t, true, nil)
	a.ready("binary_sensor.door", monitor.CategoryDoor)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.door", Kind: notify.KindState, Cooldown: zero()}); err != nil {
		t.Fatal(err)
	}
	a.stat.set(false) // liveness lost: Healthy false with no state transition
	cancel, done := a.run()
	defer func() { cancel(); <-done }()

	a.clk.await(t, 1)
	for i := 0; i < 30; i++ {
		a.clk.Advance(10 * time.Second)
		a.clk.await(t, 1)
	}
	down := a.stub.waitPosts(1)
	if c, _ := down[0].Body["content"].(string); !strings.Contains(c, "down for 5 minutes") {
		t.Fatalf("down alert: %q", c)
	}
	for i := 0; i < 4; i++ {
		a.clk.Advance(10 * time.Second)
		a.clk.await(t, 1)
	}
	if n := len(a.stub.postsSnapshot()); n != 1 {
		t.Fatalf("one alert per outage: %d messages", n)
	}

	// Monitoring recovers and the reconnect snapshot reveals a late transition.
	a.stat.set(true)
	a.eng.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceSnapshot))
	posts := a.stub.waitPosts(3)
	var order []string
	for _, p := range posts {
		c, _ := p.Body["content"].(string)
		switch {
		case strings.Contains(c, "down for"):
			order = append(order, "down")
		case strings.Contains(c, "is back after"):
			order = append(order, "recovered")
		case strings.Contains(c, "reconnected"):
			order = append(order, "late-alert")
		}
	}
	if strings.Join(order, ",") != "down,recovered,late-alert" {
		t.Errorf("the recovery notice must precede the late alert it explains: %v", order)
	}
}

func TestE2E_aRestartStartsTheDownTimerFromTheProcessStart(t *testing.T) {
	a := newAFix(t, true, nil)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	a.stat.set(false) // the application starts while Home Assistant is unreachable
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.clk.await(t, 1)
	for i := 0; i < 29; i++ {
		a.clk.Advance(10 * time.Second)
		a.clk.await(t, 1)
	}
	if n := len(a.stub.postsSnapshot()); n != 0 {
		t.Fatalf("four minutes fifty seconds is not enough: %d", n)
	}
	a.clk.Advance(10 * time.Second)
	a.clk.await(t, 1)
	a.stub.waitPosts(1)
}

func TestE2E_permissionLossIsVisibleInHealth_andStopsFurtherAttempts(t *testing.T) {
	a := newAFix(t, false, nil)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	a.stub.reply = func(int) *stubReply { return &stubReply{403, `{"message":"Missing Permissions","code":50013}`} }
	for i := 0; i < 4; i++ {
		id := "binary_sensor.door" + string(rune('a'+i))
		a.ready(id, monitor.CategoryDoor)
		if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: entities.ID(id), Kind: notify.KindState, Cooldown: zero()}); err != nil {
			t.Fatal(err)
		}
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	for i := 0; i < 3; i++ {
		a.eng.OnChange(a.change("binary_sensor.door"+string(rune('a'+i)), "off", "on", monitor.SourceLive))
	}
	a.stub.waitPosts(3)
	deadline := time.After(stubWait)
	for !a.eng.Health().BreakerOpen {
		select {
		case <-deadline:
			t.Fatal("three permission failures must open the circuit breaker")
		default:
			runtime.Gosched()
		}
	}
	line := a.admin.HealthLine()
	st, _ := a.admin.Status(ctx0, adminActor)
	if !strings.Contains(line, "Degraded") || !strings.Contains(line, "delivery channel failing") || !strings.Contains(st.LastFailure, "lacks permission") {
		t.Errorf("permission loss must be visible: health=%q status=%+v", line, st)
	}
	a.eng.OnChange(a.change("binary_sensor.doord", "off", "on", monitor.SourceLive))
	for a.eng.Health().DroppedBreaker == 0 {
		select {
		case <-deadline:
			t.Fatal("with the breaker open the next alert is dropped, counted and not attempted")
		default:
			runtime.Gosched()
		}
	}
	if n := len(a.stub.postsSnapshot()); n != 3 {
		t.Errorf("no request may be made while the breaker is open: %d", n)
	}
}

func TestE2E_shutdownWithAnInFlightDeliveryIsBounded(t *testing.T) {
	a := newAFix(t, false, func(c *notify.EngineConfig) { c.DrainTimeout = 30 * time.Millisecond })
	a.ready("binary_sensor.door", monitor.CategoryDoor)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.door", Kind: notify.KindState, Cooldown: zero()}); err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	a.stub.gate = gate // Discord accepts the connection and never answers
	cancel, done := a.run()
	a.eng.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceLive))
	a.stub.waitPosts(1) // the request is in flight
	cancel()
	select {
	case <-done:
	case <-time.After(stubWait):
		t.Fatal("Run must return within the drain budget even though a delivery is stuck")
	}
	close(gate)
	if h := a.eng.Health(); h.DroppedShutdown == 0 {
		t.Errorf("the in-flight message is dropped and counted at shutdown: %+v", h)
	}
}
