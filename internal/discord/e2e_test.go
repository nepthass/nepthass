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
	a.stub.channels["500000000000000005"] = stubChannelInfo{stubGuild, 2, "voice", 0, nil}
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
		case <-time.After(time.Millisecond): // poll an in-process counter without burning the CPU quota
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
		case <-time.After(time.Millisecond): // poll an in-process counter without burning the CPU quota
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

func auditRows(t *testing.T, a *afix) []string {
	t.Helper()
	rows, err := a.db.QueryContext(ctx0, "SELECT actor_id||'|'||command||'|'||action||'|'||outcome||'|'||detail FROM command_audit_logs ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestNotificationAdmin_deniedAndRejectedAttemptsAreAudited_withoutSecretsOrRawInput(t *testing.T) {
	a := newAFix(t, false, nil)
	voice := "500000000000000005"
	a.stub.channels[voice] = stubChannelInfo{stubGuild, 2, "private-voice", 0, nil}
	_, _ = a.admin.SetChannel(ctx0, userActor, stubChannel)
	_, _ = a.admin.SetChannel(ctx0, adminActor, voice)
	_ = a.admin.SendTest(ctx0, userActor)
	if err := a.admin.SendTest(ctx0, adminActor); !errors.Is(err, notify.ErrNotConfigured) {
		t.Fatalf("a test before configuration: %v", err)
	}
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if err := a.admin.SendTest(ctx0, adminActor); err != nil {
		t.Fatal(err)
	}
	a.stub.reply = func(int) *stubReply { return &stubReply{403, `{"message":"Missing Permissions","code":50013}`} }
	_ = a.admin.SendTest(ctx0, adminActor)

	want := []string{
		userActor.UserID + "|notifications.channel|set|denied|" + auth.ErrAdminRequired.Error(),
		adminActor.UserID + "|notifications.channel|rejected_not_text|error|channel rejected",
		userActor.UserID + "|notifications.test|send|denied|" + auth.ErrAdminRequired.Error(),
		adminActor.UserID + "|notifications.test|send|error|" + notify.ErrNotConfigured.Error(),
		adminActor.UserID + "|notifications.channel|set|success|channel=" + stubChannel,
		adminActor.UserID + "|notifications.test|send|success|",
		adminActor.UserID + "|notifications.test|send|error|test message not delivered",
	}
	got := auditRows(t, a)
	for i, r := range got {
		if !strings.Contains(r, "|success|") && strings.Contains(r, stubChannel) {
			t.Errorf("a denied or failed row must not name a channel: row %d %q", i, r)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("audit rows:\n%s", strings.Join(got, "\n"))
	}
	for i := range want {
		if !strings.HasPrefix(got[i], strings.TrimSuffix(strings.Replace(want[i], userActor.UserID, userActor.UserID, 1), "|")) {
			t.Errorf("row %d = %q, want %q", i, got[i], want[i])
		}
		for _, secret := range []string{voice, "private-voice", "Missing Permissions", "token"} {
			if strings.Contains(got[i], secret) {
				t.Errorf("row %d leaks %q: %q", i, secret, got[i])
			}
		}
	}
}

func TestNotificationAdmin_aFailedReloadIsReportedAsSavedButNotActive_visibleInHealth_andSelfHeals(t *testing.T) {
	a := newAFix(t, true, nil)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if err := a.admin.SetPaused(ctx0, adminActor, true); err != nil || a.eng.Health().Active {
		t.Fatalf("paused: %v", err)
	}
	if _, err := a.db.Exec("ALTER TABLE notification_rules RENAME TO notification_rules_broken"); err != nil {
		t.Fatal(err)
	}
	err := a.admin.SetPaused(ctx0, adminActor, false) // commits the setting, then the reload cannot read the rules
	var ue *commands.UserError
	if !errors.As(err, &ue) || !strings.Contains(ue.Msg, "NOT active") {
		t.Fatalf("must say the change is saved but not active: %v", err)
	}
	saved, ok, _ := a.store.GetSettings(ctx0, stubGuild)
	h := a.eng.Health()
	if !ok || !saved.Enabled || h.Active || !h.ConfigStale || !h.Degraded {
		t.Fatalf("persisted (enabled=%v) and runtime (active=%v) differ and health must say so: %+v", saved.Enabled, h.Active, h)
	}
	if line := a.admin.HealthLine(); !strings.Contains(line, "Degraded") || !strings.Contains(line, "not active") {
		t.Errorf("health line: %q", line)
	}
	if _, err := a.db.Exec("ALTER TABLE notification_rules_broken RENAME TO notification_rules"); err != nil {
		t.Fatal(err)
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.clk.await(t, 1) // the watcher's first pass retried the reload before sleeping
	if h := a.eng.Health(); h.ConfigStale || !h.Active || h.Degraded {
		t.Errorf("the engine must recover by itself once the store works: %+v", h)
	}
}

func TestE2E_aQueuedDownAlertIsWithdrawnWhenMonitoringRecoversBeforeDelivery(t *testing.T) {
	a := newAFix(t, true, func(c *notify.EngineConfig) { c.BackoffInitial, c.BackoffMax = 8*time.Minute, 8*time.Minute })
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	a.stub.reply = func(n int) *stubReply {
		if n == 1 {
			return &stubReply{500, "{}"}
		}
		return nil
	}
	a.stat.set(false)
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.clk.await(t, 1)
	for i := 0; i < 30; i++ {
		a.clk.Advance(10 * time.Second)
		a.clk.await(t, 1)
	}
	a.stub.waitPosts(1) // Discord fails the down alert: it waits for its retry
	a.clk.await(t, 2)   // the watcher's tick and the dispatcher's retry timer
	a.stat.set(true)
	a.clk.Advance(10 * time.Second) // recovery, long before the eight minute retry
	settle := time.After(stubWait)
	// The state flips to "recovered" before the stale alert is removed, so wait for the removal itself.
	for a.eng.Health().DroppedCancelled != 1 {
		select {
		case <-settle:
			t.Fatalf("the stale alert was never withdrawn: %+v", a.eng.Health())
		case <-time.After(time.Millisecond): // poll an in-process counter without burning the CPU quota
		}
	}
	if h := a.eng.Health(); h.DroppedCancelled != 1 || h.QueueCritical != 0 {
		t.Fatalf("recovery must withdraw the stale queued alert: %+v", h)
	}
	a.clk.Advance(9 * time.Minute)
	a.clk.await(t, 1)
	if n := len(a.stub.postsSnapshot()); n != 1 {
		t.Errorf("neither the stale alert nor a recovery notice may ever be delivered: %d messages", n)
	}
}

func TestE2E_anIntentionalShutdownDuringAnOutageProducesNoDownAlert(t *testing.T) {
	a := newAFix(t, true, nil)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	a.stat.set(false)
	cancel, done := a.run()
	a.clk.await(t, 1)
	for i := 0; i < 29; i++ { // four minutes fifty seconds of outage
		a.clk.Advance(10 * time.Second)
		a.clk.await(t, 1)
	}
	cancel()
	<-done
	if n := len(a.stub.postsSnapshot()); n != 0 {
		t.Errorf("shutting down is not an outage: %d messages", n)
	}
	awaitNoEngineGoroutines(t)
}

func awaitNoEngineGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.After(stubWait)
	for {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		s := string(buf)
		if !strings.Contains(s, "notify.(*Engine).run") && !strings.Contains(s, "notify.(*dispatcher)") {
			return
		}
		select {
		case <-deadline:
			t.Fatal("engine goroutines are still running after Run returned")
		case <-time.After(time.Millisecond): // poll an in-process counter without burning the CPU quota
		}
	}
}

func TestE2E_anAmbiguousFailureIsRetriedAndCanDuplicateTheMessage(t *testing.T) {
	a := newAFix(t, true, nil)
	a.ready("binary_sensor.door", monitor.CategoryDoor)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.door", Kind: notify.KindState, Cooldown: zero()}); err != nil {
		t.Fatal(err)
	}
	a.stub.reply = func(n int) *stubReply {
		if n == 1 {
			return &stubReply{-1, ""} // Discord has the message, the answer never arrives
		}
		return nil
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.clk.await(t, 1)
	a.eng.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceLive))
	a.stub.waitPosts(1)
	a.clk.await(t, 2) // the retry is scheduled
	a.clk.Advance(2 * time.Second)
	posts := a.stub.waitPosts(2)
	c1, _ := posts[0].Body["content"].(string)
	c2, _ := posts[1].Body["content"].(string)
	deadline := time.After(stubWait)
	for a.eng.Health().Delivered != 1 {
		select {
		case <-deadline:
			t.Fatal("the retry must be delivered")
		case <-time.After(time.Millisecond): // poll an in-process counter without burning the CPU quota
		}
	}
	if c1 == "" || c1 != c2 {
		t.Errorf("an ambiguous failure is retried, so the channel can receive the alert twice (documented): %q vs %q", c1, c2)
	}
}

func TestE2E_discordUnavailableThenRecovered_deliversOnceWithBackoff_underTheSameDeadline(t *testing.T) {
	a := newAFix(t, true, nil)
	a.ready("binary_sensor.door", monitor.CategoryDoor)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.door", Kind: notify.KindState, Cooldown: zero()}); err != nil {
		t.Fatal(err)
	}
	a.stub.reply = func(n int) *stubReply {
		if n <= 2 {
			return &stubReply{503, `{"message":"unavailable"}`}
		}
		return nil
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.clk.await(t, 1)
	a.eng.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceLive))
	for i := 1; i <= 2; i++ {
		a.stub.waitPosts(i)
		a.clk.await(t, 2)
		a.clk.Advance(3 * time.Second)
	}
	a.stub.waitPosts(3)
	deadline := time.After(stubWait)
	for a.eng.Health().Delivered != 1 {
		select {
		case <-deadline:
			t.Fatal("the alert must be delivered once Discord recovers")
		case <-time.After(time.Millisecond): // poll an in-process counter without burning the CPU quota
		}
	}
	if h := a.eng.Health(); h.Retried != 2 || h.DroppedTTL != 0 || h.Degraded {
		t.Errorf("two retries, no loss: %+v", h)
	}
}

func TestE2E_theCircuitBreakerRecoversAndHealthReturnsToNormal(t *testing.T) {
	a := newAFix(t, true, nil)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	ids := []string{"binary_sensor.da", "binary_sensor.db", "binary_sensor.dc", "binary_sensor.dd", "binary_sensor.de"}
	for _, id := range ids {
		a.ready(id, monitor.CategoryDoor)
		if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: entities.ID(id), Kind: notify.KindState, Cooldown: zero()}); err != nil {
			t.Fatal(err)
		}
	}
	a.stub.reply = func(n int) *stubReply {
		if n <= 3 {
			return &stubReply{403, `{"code":50013}`}
		}
		return nil
	}
	cancel, done := a.run()
	defer func() { cancel(); <-done }()
	a.clk.await(t, 1)
	for _, id := range ids[:3] {
		a.eng.OnChange(a.change(id, "off", "on", monitor.SourceLive))
	}
	a.stub.waitPosts(3)
	deadline := time.After(stubWait)
	for !a.eng.Health().BreakerOpen {
		select {
		case <-deadline:
			t.Fatal("the breaker must open")
		case <-time.After(time.Millisecond): // poll an in-process counter without burning the CPU quota
		}
	}
	if line := a.admin.HealthLine(); !strings.Contains(line, "Degraded") {
		t.Fatalf("health while broken: %q", line)
	}
	a.clk.Advance(61 * time.Second) // the probe interval
	a.clk.await(t, 1)
	a.eng.OnChange(a.change(ids[3], "off", "on", monitor.SourceLive))
	a.stub.waitPosts(4) // the probe
	for a.eng.Health().BreakerOpen || a.eng.Health().Delivered == 0 {
		select {
		case <-deadline:
			t.Fatal("a successful probe must close the breaker")
		case <-time.After(time.Millisecond): // poll an in-process counter without burning the CPU quota
		}
	}
	a.eng.OnChange(a.change(ids[4], "off", "on", monitor.SourceLive))
	a.stub.waitPosts(5)
	a.clk.Advance(11 * time.Minute) // lets the 'alerts were dropped' window pass
	a.clk.await(t, 1)
	if line := a.admin.HealthLine(); !strings.Contains(line, "Enabled") {
		t.Errorf("health must return to normal after recovery: %q", line)
	}
}

func TestE2E_configurationSurvivesARestart_andTheExistingStateStaysSilent(t *testing.T) {
	a := newAFix(t, false, nil)
	a.ready("binary_sensor.door", monitor.CategoryDoor)
	if _, err := a.admin.SetChannel(ctx0, adminActor, stubChannel); err != nil {
		t.Fatal(err)
	}
	if _, err := a.admin.AddRule(ctx0, adminActor, notify.NewRule{Entity: "binary_sensor.door", Kind: notify.KindState, Cooldown: zero()}); err != nil {
		t.Fatal(err)
	}
	// The process restarts: a brand new engine on the same database, door already open.
	a.tracker.Apply(monitor.Update{ID: "binary_sensor.door", Category: monitor.CategoryDoor, Name: "Front door", Raw: "on", LastChanged: a.now, LastUpdated: a.now}, a.now)
	restarted, err := notify.NewEngine(notify.EngineDeps{Store: a.store, GuildID: stubGuild, Access: a.perms, Sensors: a.tracker, Status: a.stat, Sender: a.n,
		Config: notify.EngineConfig{Rand: func(n int64) int64 { return n - 1 }}})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reload(ctx0); err != nil || !restarted.Health().Active {
		t.Fatalf("the persisted configuration must be active after a restart: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { restarted.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	restarted.OnChange(a.change("binary_sensor.door", "", "on", monitor.SourceSnapshot)) // the new session's first look
	restarted.OnChange(a.change("binary_sensor.door", "on", "off", monitor.SourceLive))
	restarted.OnChange(a.change("binary_sensor.door", "off", "on", monitor.SourceLive))
	a.stub.waitPosts(1)
	if n := len(a.stub.postsSnapshot()); n != 1 || restarted.Health().Enqueued != 1 {
		t.Errorf("an already-open door is silent after a restart and only the real transition alerts: %d", n)
	}
}
