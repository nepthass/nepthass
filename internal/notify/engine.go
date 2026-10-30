package notify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// AliasSource names sensors for messages. *entities.Aliases satisfies it. It is optional.
type AliasSource interface {
	All(ctx context.Context) (map[entities.ID]string, error)
}

// EngineConfig tunes the engine; every zero value selects the approved default.
type EngineConfig struct {
	TTL               time.Duration // absolute message lifetime from observation (10m)
	DrainTimeout      time.Duration // bounded shutdown drain (3s)
	HealthTick        time.Duration // monitoring health sampling interval (10s)
	DownAfter         time.Duration // continuous unhealthy time before a down alert (5m)
	DigestMinInterval time.Duration // minimum spacing of resync digests (1m)
	DegradedWindow    time.Duration // how long a drop keeps health degraded (10m)
	BacklogAfter      time.Duration // sustained queue backlog that degrades health (30s)
	LagAfter          time.Duration // evaluator lag that degrades health (5s)
	CriticalCap       int           // critical lane capacity (32)
	NormalCap         int           // normal lane capacity (64)
	DigestMaxLines    int           // lines in one digest (10)
	Rate              float64       // sends per second (1)
	Burst             int           // send burst (5)
	BackoffInitial    time.Duration // first retry delay (1s)
	BackoffMax        time.Duration // retry delay cap (30s)
	Rand              func(n int64) int64
	BreakerThreshold  int           // consecutive permanent failures that open the breaker (3)
	BreakerProbe      time.Duration // probe interval while open (60s)
}

func (c *EngineConfig) withDefaults() {
	d := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	n := func(v *int, def int) {
		if *v <= 0 {
			*v = def
		}
	}
	d(&c.TTL, 10*time.Minute)
	d(&c.DrainTimeout, 3*time.Second)
	d(&c.HealthTick, 10*time.Second)
	d(&c.DownAfter, 5*time.Minute)
	d(&c.DigestMinInterval, time.Minute)
	d(&c.DegradedWindow, 10*time.Minute)
	d(&c.BacklogAfter, 30*time.Second)
	d(&c.LagAfter, 5*time.Second)
	d(&c.BackoffInitial, time.Second)
	d(&c.BackoffMax, 30*time.Second)
	d(&c.BreakerProbe, time.Minute)
	n(&c.CriticalCap, 32)
	n(&c.NormalCap, 64)
	n(&c.DigestMaxLines, 10)
	n(&c.Burst, 5)
	n(&c.BreakerThreshold, 3)
	if c.Rate <= 0 {
		c.Rate = 1
	}
	if c.Rand == nil {
		c.Rand = rand.Int64N
	}
}

// EngineDeps are the collaborators of Engine.
type EngineDeps struct {
	Store   *Store
	GuildID string
	Access  AccessSource
	Sensors SensorLookup
	Aliases AliasSource // optional
	Status  StatusSource
	Sender  Sender
	Clock   Clock // defaults to the wall clock
	Logger  *slog.Logger
	Config  EngineConfig
}

type ruleState struct {
	baselined bool // the rule has seen a reliable state to compare against
	active    bool // a violation episode is in progress (re-arms when reliably clear)
	hasFired  bool
	lastFired time.Time
	muted     int
}

// Engine turns monitor changes into alerts. It owns three goroutines (evaluator,
// dispatcher, health watcher) that Run starts and joins. Nothing it does blocks
// the monitor: OnChange only records into the mailbox.
type Engine struct {
	d     EngineDeps
	cfg   EngineConfig
	clock Clock
	log   *slog.Logger

	rules  atomic.Pointer[ruleSet]
	active atomic.Bool
	mb     *mailbox
	q      *queue
	disp   *dispatcher
	down   *downMachine
	m      counters

	reseed atomic.Bool
	kick   chan struct{}

	// reloadMu serializes Reload so the stored state is always the latest read;
	// reloadFailed is true while the persisted configuration differs from the
	// running one because a reload failed.
	reloadMu     sync.Mutex
	reloadFailed atomic.Bool

	states         map[int64]*ruleState // evaluator goroutine only
	lastDigest     time.Time            // evaluator goroutine only
	resyncMu       sync.Mutex
	resync         map[int64]struct{}
	digestInFlight atomic.Bool
	backlogSince   atomic.Int64
	atShutdown     atomic.Int64

	// onTick, when set (tests only), runs after each health step once the next
	// tick timer is registered, so a test can advance its fake clock deterministically.
	onTick func()
}

// NewEngine builds an Engine. Notifications are disabled until Reload has found
// a configured, enabled channel.
func NewEngine(d EngineDeps) (*Engine, error) {
	if d.Store == nil || d.Access == nil || d.Sensors == nil || d.Status == nil || d.Sender == nil {
		return nil, errors.New("notify: engine needs a store, access source, sensors, status source and sender")
	}
	if d.Clock == nil {
		d.Clock = realClock{}
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	d.Config.withDefaults()
	c := d.Config
	e := &Engine{d: d, cfg: c, clock: d.Clock, log: d.Logger, mb: newMailbox(MaxRules), q: newQueue(c.CriticalCap, c.NormalCap),
		down: newDownMachine(d.Clock.Now(), c.DownAfter), kick: make(chan struct{}, 1), states: map[int64]*ruleState{}, resync: map[int64]struct{}{}}
	e.rules.Store(newRuleSet(nil))
	e.disp = newDispatcher(e.q, d.Sender, d.Access, d.Clock, dispatcherConfig{ttlDrain: c.DrainTimeout,
		backoff: retryBackoff{initial: c.BackoffInitial, max: c.BackoffMax, rand: c.Rand}, rate: c.Rate, burst: c.Burst,
		threshold: c.BreakerThreshold, probe: c.BreakerProbe}, e.active.Load, e, d.Logger, &e.m)
	return e, nil
}

// OnChange is the monitor callback (monitor.Config.OnChange). It does one atomic
// load, one map lookup and, for a watched entity, an O(1) mailbox update: no
// database, no network, no allocation per event beyond the bounded mailbox, no
// goroutine. With notifications disabled the rule set is empty and it returns
// after the lookup.
func (e *Engine) OnChange(c monitor.Change) {
	cached := e.rules.Load().byEntity[c.ID]
	if len(cached) == 0 {
		return
	}
	e.mb.record(c, cached)
}

// Reload re-reads the settings and the rules from the store and swaps them in
// atomically. Call it after any configuration change. Rules that are new,
// resumed or no longer present have their runtime state re-seeded from the
// current sensor, so enabling a rule never alerts for a state that already existed.
func (e *Engine) Reload(ctx context.Context) error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	err := e.reload(ctx)
	e.reloadFailed.Store(err != nil)
	if err != nil {
		e.log.Error("notification configuration could not be loaded; the previous configuration stays in force and the engine retries", "error", err)
	}
	return err
}

func (e *Engine) reload(ctx context.Context) error {
	st, ok, err := e.d.Store.GetSettings(ctx, e.d.GuildID)
	if err != nil {
		return err
	}
	active := ok && st.Enabled
	var rules []Rule
	if active {
		if rules, err = e.d.Store.ListRules(ctx); err != nil {
			return err
		}
	}
	e.active.Store(active)
	e.rules.Store(newRuleSet(rules))
	e.reseed.Store(true)
	e.kickEvaluator()
	return nil
}

func (e *Engine) kickEvaluator() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run starts the evaluator, the dispatcher and the health watcher and returns
// after ctx ends and all three have stopped (the dispatcher after its bounded drain).
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); e.runEvaluator(ctx) }()
	go func() { defer wg.Done(); e.disp.run(ctx) }()
	go func() { defer wg.Done(); e.runWatcher(ctx) }()
	wg.Wait()
}

func (e *Engine) runEvaluator(ctx context.Context) {
	for {
		select {
		case <-e.mb.wake:
		case <-e.kick:
		case <-ctx.Done():
			n, _ := e.mb.pending(e.clock.Now())
			e.atShutdown.Store(int64(n)) // changes still in the mailbox are not evaluated at shutdown
			return
		}
		e.evaluate(ctx)
	}
}

func (e *Engine) evaluate(ctx context.Context) {
	rs := e.rules.Load()
	if e.reseed.Swap(false) {
		e.reconcileStates(rs)
	}
	slots, order, overflowed := e.mb.drain()
	if overflowed {
		for id := range rs.byID {
			e.markResync(id)
		}
	}
	// A snapshot after a reconnect usually means monitoring just recovered. Take a
	// health sample NOW, before producing the late alerts it explains, so a pending
	// recovery notice is queued ahead of them instead of up to a tick later.
	for _, s := range slots {
		if s.anySnapshot {
			e.sampleHealth()
			break
		}
	}
	for _, id := range order {
		e.evaluateSlot(ctx, rs, id, slots[id])
	}
	e.maybeDigest(ctx, rs)
	e.noteBacklog()
}

// reconcileStates drops state of rules that disappeared and seeds new ones from
// the current sensor, silently.
func (e *Engine) reconcileStates(rs *ruleSet) {
	for id := range e.states {
		if _, ok := rs.byID[id]; !ok {
			delete(e.states, id)
		}
	}
	for id, r := range rs.byID {
		if _, ok := e.states[id]; ok {
			continue
		}
		st := &ruleState{}
		if s, monitored := e.d.Sensors.Get(r.Entity); monitored && compatible(r.Kind, s.Category) && reliable(s) {
			st.active, st.baselined = violates(r, s), true
		}
		e.states[id] = st
	}
}

// evaluateSlot applies one entity's coalesced changes to its rules.
//
// A rule alerts only for a latched qualifying transition (see enters): a first
// observation, a state that merely persists, and an unreliable-to-violating
// transition (other than a leak reaching wet) re-arm or baseline silently. After
// every pass the rule is "active" while the sensor reliably violates it and
// re-arms when the sensor reliably stops violating it; an unavailable or unknown
// reading changes neither. The qualifying transition is consumed whether or not
// an alert goes out: if cooldown or sharing suppresses it, no alert is deferred
// and none is produced later for that same episode.
func (e *Engine) evaluateSlot(ctx context.Context, rs *ruleSet, id entities.ID, s slot) {
	rules := rs.byEntity[id]
	now := e.clock.Now()
	var (
		access     entities.Access
		haveAccess bool
		accessErr  bool
		aliases    map[entities.ID]string
		loaded     bool
	)
	for _, r := range rules {
		st := e.states[r.ID]
		if st == nil {
			st = &ruleState{}
			e.states[r.ID] = st
		}
		if s.removed {
			st.baselined, st.active = false, false
			continue
		}
		if !s.haveCur {
			continue
		}
		cur := s.cur
		if !compatible(r.Kind, cur.Category) {
			st.baselined = false
			continue
		}
		if s.reset {
			st.baselined = false
		}
		violating, ok := violates(r, cur), reliable(cur)
		l, latched := s.latchFor(r.ID)
		if latched {
			st.active = true
		}
		if ok {
			st.active, st.baselined = violating, true
		}
		if !latched {
			continue
		}
		if !haveAccess {
			var err error
			if access, err = e.d.Access.Get(ctx, id); err != nil {
				accessErr = true
			}
			haveAccess = true
		}
		if accessErr {
			e.markResync(r.ID) // sharing could not be read: re-check the current state later
			continue
		}
		if !access.AtLeast(entities.AccessRead) {
			e.m.suppressedUnshared.Add(1)
			continue
		}
		if cd := cooldownOf(r, cur.Category); cd > 0 && st.hasFired && now.Sub(st.lastFired) < cd {
			st.muted++
			e.m.suppressedCooldown.Add(1)
			continue
		}
		if !loaded && e.d.Aliases != nil {
			aliases, _ = e.d.Aliases.All(ctx)
		}
		loaded = true
		msg := e.alertMessage(r, id, cur, l, violating, st.muted, aliases)
		st.muted, st.hasFired, st.lastFired = 0, true, now
		e.enqueue(msg)
	}
}

func (e *Engine) alertMessage(r cachedRule, id entities.ID, cur monitor.Sensor, l latch, violating bool, muted int, aliases map[entities.ID]string) Message {
	in := l.sensor.Interpretation()
	m := Message{Kind: MsgAlert, RuleID: r.ID, Category: l.sensor.Category, Name: displayName(id, l.sensor, aliases), Trigger: r.Kind,
		Meaning: in.Meaning, Value: in.Value, HasValue: in.HasValue, Unit: l.sensor.Unit, ObservedAt: l.at, ChangedAt: l.sensor.LastChanged,
		Late: l.source == monitor.SourceSnapshot, Uncertain: l.fromUnreliable, Cleared: !violating, Muted: muted, entity: id}
	if r.Kind != KindState {
		t := r.Threshold
		m.Threshold = &t
	}
	return m
}

// displayName is the alias, else the Home Assistant friendly name, else a
// category noun; it never falls back to the entity ID.
func displayName(id entities.ID, s monitor.Sensor, aliases map[entities.ID]string) string {
	switch {
	case aliases[id] != "":
		return aliases[id]
	case s.Name != "":
		return s.Name
	}
	return categoryNoun(s.Category)
}

func (e *Engine) enqueue(m Message) {
	e.m.enqueued.Add(1)
	for _, dr := range e.q.push(item{msg: m, deadline: m.ObservedAt.Add(e.cfg.TTL)}, false) {
		e.disp.report(dr)
	}
	e.noteBacklog()
}

func (e *Engine) markResync(rule int64) {
	e.resyncMu.Lock()
	e.resync[rule] = struct{}{}
	e.resyncMu.Unlock()
}

// maybeDigest reports the CURRENT state of rules whose alerts were lost. It does
// not and cannot recover a transition that happened and ended: it re-reads each
// flagged rule's sensor and sharing now and lists only those still violating, in
// one bounded message, at most once per DigestMinInterval and only when the
// queue has drained, so a recovered channel is not flooded.
func (e *Engine) maybeDigest(ctx context.Context, rs *ruleSet) {
	now := e.clock.Now()
	if e.digestInFlight.Load() || (!e.lastDigest.IsZero() && now.Sub(e.lastDigest) < e.cfg.DigestMinInterval) {
		return
	}
	if c, n := e.q.depths(); c > e.cfg.CriticalCap/4 || n > e.cfg.NormalCap/4 {
		return
	}
	e.resyncMu.Lock()
	ids := make([]int64, 0, len(e.resync))
	for id := range e.resync {
		ids = append(ids, id)
	}
	clear(e.resync)
	e.resyncMu.Unlock()
	if len(ids) == 0 {
		return
	}
	slices.Sort(ids)
	var aliases map[entities.ID]string
	if e.d.Aliases != nil {
		aliases, _ = e.d.Aliases.All(ctx)
	}
	var lines []DigestLine
	for _, id := range ids {
		r, ok := rs.byID[id]
		if !ok {
			continue
		}
		s, monitored := e.d.Sensors.Get(r.Entity)
		if !monitored || !compatible(r.Kind, s.Category) || !violates(r, s) {
			continue
		}
		if access, err := e.d.Access.Get(ctx, r.Entity); err != nil || !access.AtLeast(entities.AccessRead) {
			continue
		}
		in := s.Interpretation()
		line := DigestLine{Category: s.Category, Name: displayName(r.Entity, s, aliases), Trigger: r.Kind, Meaning: in.Meaning,
			Value: in.Value, HasValue: in.HasValue, Unit: s.Unit, ChangedAt: s.LastChanged, rule: r.ID, entity: r.Entity}
		if r.Kind != KindState {
			t := r.Threshold
			line.Threshold = &t
		}
		lines = append(lines, line)
		st := e.states[id]
		if st == nil {
			st = &ruleState{}
			e.states[id] = st
		}
		st.active, st.baselined, st.hasFired, st.lastFired = true, true, true, now
	}
	if len(lines) == 0 {
		return
	}
	more := 0
	if len(lines) > e.cfg.DigestMaxLines {
		more = len(lines) - e.cfg.DigestMaxLines
		lines = lines[:e.cfg.DigestMaxLines]
	}
	e.digestInFlight.Store(true)
	e.lastDigest = now
	e.m.digests.Add(1)
	e.enqueue(Message{Kind: MsgDigest, Lines: lines, MoreLines: more, ObservedAt: now})
}

// runWatcher samples monitoring health every HealthTick. Health is polled, not
// awaited, because Status.Healthy can turn false by the passage of time alone,
// with no state transition to wake a waiter.
func (e *Engine) runWatcher(ctx context.Context) {
	for {
		if e.reloadFailed.Load() {
			_ = e.Reload(ctx) // safe recovery: keep trying until the saved configuration is active
		}
		if e.sampleHealth() {
			return
		}
		e.kickEvaluator() // lets a pending digest run when the queue has drained
		timer := e.clock.After(e.cfg.HealthTick)
		if e.onTick != nil {
			e.onTick()
		}
		select {
		case <-timer:
		case <-ctx.Done():
			e.down.shutdown() // shutting down is not an outage
			return
		}
	}
}

// sampleHealth takes one monitoring-health observation and applies the
// monitoring-down machine. It returns true when the monitor was stopped on
// purpose and sampling should end. It is safe to call from the evaluator too.
func (e *Engine) sampleHealth() (stop bool) {
	st := e.d.Status.Status()
	now := e.clock.Now()
	switch {
	case st.State == monitor.StateStopped:
		e.down.shutdown() // the monitor was stopped on purpose: never an outage
		return true
	case !e.active.Load():
		e.down.reset(now) // notifications are off: no episode accumulates
	default:
		e.handleDown(e.down.step(now, st.Healthy, !e.disp.br.isOpen()), st, now)
	}
	return false
}

func (e *Engine) handleDown(act downAction, st monitor.Status, now time.Time) {
	ep, since := e.down.episodeInfo()
	switch act {
	case actSendDown:
		e.enqueue(Message{Kind: MsgMonitoringDown, DownSince: since, DownFor: now.Sub(since), Failure: st.LastFailure, Episode: ep, ObservedAt: now})
	case actSendRecovery:
		e.enqueue(Message{Kind: MsgMonitoringRecovered, DownSince: since, DownFor: now.Sub(since), Episode: ep, ObservedAt: now})
	case actCancelDown:
		for _, it := range e.q.removeWhere(func(it item) bool { return it.msg.Kind == MsgMonitoringDown && it.msg.Episode == ep }) {
			e.disp.report(drop{it: it, reason: dropCancelled})
		}
	}
}

// delivered and dropped implement outcomeSink for the dispatcher.
func (e *Engine) delivered(it item) {
	switch it.msg.Kind {
	case MsgMonitoringDown:
		if e.down.delivered(it.msg.Episode) {
			ep, since := e.down.episodeInfo()
			now := e.clock.Now()
			e.enqueue(Message{Kind: MsgMonitoringRecovered, DownSince: since, DownFor: now.Sub(since), Episode: ep, ObservedAt: now})
		}
	case MsgDigest:
		e.digestInFlight.Store(false)
	}
	e.noteBacklog()
	e.kickEvaluator()
}

func (e *Engine) dropped(dr drop) {
	lost := dr.reason == dropOverflow || dr.reason == dropTTL || dr.reason == dropPermanent || dr.reason == dropBreaker
	switch dr.it.msg.Kind {
	case MsgAlert:
		if lost {
			e.markResync(dr.it.msg.RuleID)
		}
	case MsgDigest:
		e.digestInFlight.Store(false)
		if lost {
			for _, l := range dr.it.msg.Lines {
				e.markResync(l.rule)
			}
		}
	case MsgMonitoringDown:
		if lost {
			e.down.dropped(dr.it.msg.Episode)
		}
	}
	e.noteBacklog()
	e.kickEvaluator()
}

func (e *Engine) noteBacklog() {
	c, n := e.q.depths()
	if c*5 > e.cfg.CriticalCap*4 || n*5 > e.cfg.NormalCap*4 {
		e.backlogSince.CompareAndSwap(0, e.clock.Now().UnixNano())
		return
	}
	e.backlogSince.Store(0)
}

// Health is what a status command shows: whether notifications are degraded and why.
type Health struct {
	Active         bool
	Degraded       bool
	Reasons        []string
	MailboxPending int
	MailboxLag     time.Duration
	Coalesced      uint64
	QueueCritical  int
	QueueNormal    int
	ResyncPending  int
	BreakerOpen    bool
	DownState      string

	Enqueued, Delivered, Retried                                       uint64
	DroppedOverflow, DroppedTTL, DroppedPermanent, DroppedBreaker      uint64
	DroppedRevoked, DroppedDisabled, DroppedShutdown, DroppedCancelled uint64
	SuppressedCooldown, SuppressedUnshared                             uint64
	Digests, Abandoned                                                 uint64
	PendingAtShutdown                                                  int64
	// ConfigStale is true while the saved configuration is NOT the running one
	// (a reload failed). The engine keeps the previous configuration and retries.
	ConfigStale bool
}

// Health reports counters and whether notifications are degraded: an alert was
// lost (dropped for overflow, TTL, a broken channel) within DegradedWindow, the
// queues have been over 80% for BacklogAfter, the evaluator lags by LagAfter, or
// the circuit breaker is open.
func (e *Engine) Health() Health {
	now := e.clock.Now()
	pending, lag := e.mb.pending(now)
	crit, norm := e.q.depths()
	e.resyncMu.Lock()
	resync := len(e.resync)
	e.resyncMu.Unlock()
	h := Health{Active: e.active.Load(), MailboxPending: pending, MailboxLag: lag, Coalesced: e.mb.coalesced.Load(), QueueCritical: crit, QueueNormal: norm,
		ResyncPending: resync, BreakerOpen: e.disp.br.isOpen(), DownState: e.down.current().String(),
		Enqueued: e.m.enqueued.Load(), Delivered: e.m.delivered.Load(), Retried: e.m.retried.Load(),
		DroppedOverflow: e.m.droppedOverflow.Load(), DroppedTTL: e.m.droppedTTL.Load(), DroppedPermanent: e.m.droppedPermanent.Load(),
		DroppedBreaker: e.m.droppedBreaker.Load(), DroppedRevoked: e.m.droppedRevoked.Load(), DroppedDisabled: e.m.droppedDisabled.Load(),
		DroppedShutdown: e.m.droppedShutdown.Load(), DroppedCancelled: e.m.droppedCancelled.Load(),
		SuppressedCooldown: e.m.suppressedCooldown.Load(), SuppressedUnshared: e.m.suppressedUnshared.Load(),
		Digests: e.m.digests.Load(), Abandoned: e.m.abandoned.Load(), PendingAtShutdown: e.atShutdown.Load()}
	h.ConfigStale = e.reloadFailed.Load()
	if h.ConfigStale {
		h.Reasons = append(h.Reasons, "saved configuration is not active (reload failed; retrying)")
	}
	if last := e.m.lastDropNanos.Load(); last != 0 && now.Sub(time.Unix(0, last)) < e.cfg.DegradedWindow {
		h.Reasons = append(h.Reasons, "alerts were dropped recently")
	}
	if since := e.backlogSince.Load(); since != 0 && now.Sub(time.Unix(0, since)) >= e.cfg.BacklogAfter {
		h.Reasons = append(h.Reasons, "delivery backlog")
	}
	if lag >= e.cfg.LagAfter {
		h.Reasons = append(h.Reasons, "evaluator lag")
	}
	if h.BreakerOpen {
		h.Reasons = append(h.Reasons, "delivery channel failing")
	}
	h.Degraded = len(h.Reasons) > 0
	return h
}
