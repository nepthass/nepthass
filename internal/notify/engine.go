package notify

import (
	"cmp"
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
	// Initial-state alerting (see initialstate.go).
	StartupWindow       time.Duration // how long after the first sighting baseline conditions are collected into one summary (2m)
	StartupHazardBudget int           // individual hazard alerts per start before the rest are folded into one message (3)
	SummaryMinInterval  time.Duration // minimum spacing of start-up summaries, persisted across restarts (10m)
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
	d(&c.StartupWindow, 2*time.Minute)
	d(&c.SummaryMinInterval, 10*time.Minute)
	n(&c.StartupHazardBudget, 3)
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
	Binder  UnitBinder // optional: binds legacy numeric rules to their unit
	Clock   Clock      // defaults to the wall clock
	Logger  *slog.Logger
	Config  EngineConfig
}

// UnitBinder binds numeric rules created before unit binding (see
// Service.BindLegacyUnits). It is optional.
type UnitBinder interface {
	BindLegacyUnits(ctx context.Context) (int, error)
}

// ruleState is the evaluator's memory of one rule. It lives in memory only and is
// owned by the evaluator goroutine.
type ruleState struct {
	baselined bool   // the rule has seen a reliable state to compare against
	armed     bool   // the rule may fire; false after it fires, until it re-arms
	unit      string // the unit the rule was bound to when this state was made
	ep        *episode
	hasFired  bool
	lastFired time.Time
	muted     int
}

// episode is a pending duration: the condition has been seen violating since
// start and the rule fires at due if the sensor still violates it then. Time is
// NeptHass's own clock only (never Home Assistant's last_changed), and start is
// never earlier than the moment NeptHass observed the condition, so an alert can
// be late but never early. exact is true when NeptHass saw the transition live;
// otherwise the condition may have held longer, and the message says "at least".
type episode struct {
	start time.Time
	due   time.Time
	exact bool
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

	// monitoringOK mirrors Status.Healthy and gates duration alerts: a duration is
	// never claimed while Home Assistant cannot be watched. When monitoring comes
	// back, pending episodes are re-seeded from the current sensors (reseedDur),
	// because continuity during the outage cannot be proven.
	monitoringOK atomic.Bool
	reseedDur    atomic.Bool
	pendingEps   atomic.Int64
	startup      startupState // evaluator goroutine only
	bindWake     chan struct{}

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
		down: newDownMachine(d.Clock.Now(), c.DownAfter), kick: make(chan struct{}, 1), bindWake: make(chan struct{}, 1), states: map[int64]*ruleState{}, resync: map[int64]struct{}{}}
	e.monitoringOK.Store(true)
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
		var due <-chan time.Time
		if at, ok := e.nextDue(); ok {
			due = e.clock.After(max(at.Sub(e.clock.Now()), 0))
		}
		select {
		case <-e.mb.wake:
		case <-e.kick:
		case <-due:
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
	// Drain first: a rule that is new or just re-bound is seeded from the state
	// BEFORE the changes still waiting in the mailbox, so a transition that is
	// pending right now is not mistaken for a state that already existed.
	slots, order, overflowed := e.mb.drain()
	if e.reseed.Swap(false) {
		e.reconcileStates(ctx, rs, slots)
	}
	if e.reseedDur.Swap(false) {
		e.reseedEpisodes(rs)
	}
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
	e.fireDue(ctx, rs)
	e.flushStartup(ctx)
	e.maybeDigest(ctx, rs)
	e.noteBacklog()
	e.noteEpisodes()
}

// reconcileStates drops the state of rules that disappeared and seeds new ones
// (and ones whose unit was just bound) from the current sensor, silently: enabling
// a rule never alerts for a state that already existed.
func (e *Engine) reconcileStates(ctx context.Context, rs *ruleSet, pending map[entities.ID]slot) {
	for id := range e.states {
		if _, ok := rs.byID[id]; !ok {
			delete(e.states, id)
		}
	}
	now := e.clock.Now()
	for id, r := range rs.byID {
		if st, ok := e.states[id]; ok && st.unit == r.Unit {
			continue
		}
		st := &ruleState{armed: true, unit: r.Unit}
		e.states[id] = st
		s, monitored := e.d.Sensors.Get(r.Entity)
		if sl, waiting := pending[r.Entity]; waiting {
			switch {
			case sl.reset || sl.removed:
				// A first observation or a removal is waiting: it is baselined when the slot is
				// evaluated next. Seeding from the tracker too would handle it twice.
				continue
			case !r.stateful():
				if _, latched := sl.latchFor(r.ID); latched {
					// A qualifying transition for this very rule is waiting: an event the
					// evaluator processes next, not a condition that already existed.
					st.baselined = true
					continue
				}
			case len(sl.trail) > 0:
				// Changes for this entity are waiting: seed from the state before the first of
				// them, so evaluating them next sees the same transitions a running rule would.
				first := sl.trail[0]
				if sl.trailLost || !first.HadPrev {
					continue // no known "before": the first reliable reading becomes the baseline
				}
				s, monitored = first.Prev, true
			}
		}
		if !monitored || !usable(r, s) || !reliableFor(r, s) {
			continue
		}
		st.baselined = true
		if violates(r, s) {
			e.initialState(&slotEnv{e: e, ctx: ctx, id: r.Entity, now: now, reload: true}, r, st, s, now)
		}
	}
	e.noteEpisodes()
}

func (e *Engine) startEpisode(st *ruleState, at time.Time, d time.Duration, exact bool) {
	st.ep = &episode{start: at, due: at.Add(d), exact: exact}
	e.m.episodesStarted.Add(1)
}

// cancelEpisode drops a pending episode: the condition ended, became unreliable,
// or its continuity could not be proven.
func (e *Engine) cancelEpisode(st *ruleState) {
	if st.ep != nil {
		st.ep = nil
		e.m.episodesCancelled.Add(1)
	}
}

func (e *Engine) noteEpisodes() {
	n := 0
	for _, st := range e.states {
		if st.ep != nil {
			n++
		}
	}
	e.pendingEps.Store(int64(n))
}

// nextDue is the earliest pending duration. While monitoring is unhealthy
// nothing is due: a duration is never claimed while Home Assistant cannot be watched.
func (e *Engine) nextDue() (time.Time, bool) {
	if !e.monitoringOK.Load() {
		return time.Time{}, false
	}
	var first time.Time
	found := false
	for _, st := range e.states {
		if st.ep != nil && (!found || st.ep.due.Before(first)) {
			first, found = st.ep.due, true
		}
	}
	return first, found
}

// slotEnv carries what one evaluation of one entity shares between its rules:
// the sharing read (once), the alias map (once) and the clock reading.
type slotEnv struct {
	e         *Engine
	ctx       context.Context
	id        entities.ID
	now       time.Time
	access    entities.Access
	haveRead  bool
	accessErr bool
	aliases   map[entities.ID]string
	loaded    bool
	// reload is true when the evaluation comes from a rule being created or re-bound
	// rather than from a change at start-up: such a baseline is never listed in the
	// "already so when NeptHass started" summary.
	reload bool
}

func (v *slotEnv) load() {
	if v.haveRead {
		return
	}
	a, err := v.e.d.Access.Get(v.ctx, v.id)
	v.access, v.accessErr, v.haveRead = a, err != nil, true
}

// permitted reports whether sharing allows an alert for the entity right now. A
// sharing read that fails is not "unshared": the rule is flagged for a digest of
// the current state instead.
func (v *slotEnv) permitted(rule int64) bool {
	v.load()
	if v.accessErr {
		v.e.markResync(rule)
		return false
	}
	if !v.access.AtLeast(entities.AccessRead) {
		v.e.m.suppressedUnshared.Add(1)
		return false
	}
	return true
}

func (v *slotEnv) names() map[entities.ID]string {
	if !v.loaded && v.e.d.Aliases != nil {
		v.aliases, _ = v.e.d.Aliases.All(v.ctx)
	}
	v.loaded = true
	return v.aliases
}

// release applies the cooldown and, when it allows, enqueues the alert. A
// violation suppressed by the cooldown is counted and consumed, never deferred.
func (e *Engine) release(v *slotEnv, r cachedRule, st *ruleState, cat monitor.Category, build func(muted int) Message) {
	if cd := cooldownOf(r, cat); cd > 0 && st.hasFired && v.now.Sub(st.lastFired) < cd {
		st.muted++
		e.m.suppressedCooldown.Add(1)
		return
	}
	msg := build(st.muted)
	st.muted, st.hasFired, st.lastFired = 0, true, v.now
	e.enqueue(msg)
}

// maybeRequestBind asks the watcher to bind a legacy numeric rule when its sensor
// reports a real value in the one documented unit of its class. Ambiguous classes
// are never requested: they wait for an administrator.
func (e *Engine) maybeRequestBind(r cachedRule, s monitor.Sensor) {
	if e.d.Binder == nil || r.Kind == KindState || r.Unit != "" || !reliable(s) {
		return
	}
	spec, ok := monitor.SpecFor(s.Category)
	if !ok {
		return
	}
	if unit, single := spec.SingleUnit(); !single || s.Unit != unit {
		return
	}
	select {
	case e.bindWake <- struct{}{}:
	default:
	}
}

// evaluateSlot applies one entity's coalesced changes to its rules.
//
// A plain immediate rule alerts only for a latched qualifying transition (see
// enters): a first observation, a state that merely persists, and an
// unreliable-to-violating transition (other than a hazard reaching its alert
// state) baseline silently. The qualifying transition is consumed whether or not
// an alert goes out: if cooldown or sharing suppresses it, no alert is deferred
// and none is produced later for that same episode. A rule that cannot evaluate
// this sensor (the kind does not fit, or the unit is not the one the threshold is
// bound to) is dormant: it never fires and never re-arms.
//
// A stateful rule (a duration or a clear value) replays the ordered trail instead.
func (e *Engine) evaluateSlot(ctx context.Context, rs *ruleSet, id entities.ID, s slot) {
	v := &slotEnv{e: e, ctx: ctx, id: id, now: e.clock.Now()}
	if e.startup.firstSight.IsZero() {
		e.startup.firstSight = v.now // the start-up window opens at the first sighting
	}
	for _, r := range rs.byEntity[id] {
		st := e.states[r.ID]
		if st == nil {
			st = &ruleState{armed: true, unit: r.Unit}
			e.states[r.ID] = st
		}
		if s.removed {
			st.baselined, st.armed = false, true
			e.cancelEpisode(st)
			continue
		}
		if !s.haveCur {
			continue
		}
		cur := s.cur
		e.maybeRequestBind(r, cur)
		if !usable(r, cur) {
			st.baselined = false
			e.cancelEpisode(st)
			continue
		}
		if s.reset {
			st.baselined = false
		}
		if r.stateful() {
			e.replay(v, r, st, s)
			continue
		}
		first := reliableFor(r, cur) && !st.baselined
		if reliableFor(r, cur) {
			st.baselined = true
		}
		l, latched := s.latchFor(r.ID)
		if !latched {
			if first && violates(r, cur) {
				e.initialState(v, r, st, cur, v.now) // seen already violating: no event happened
			}
			continue
		}
		if !v.permitted(r.ID) {
			continue
		}
		e.release(v, r, st, cur.Category, func(muted int) Message {
			return e.alertMessage(r, id, cur, l, violates(r, cur), muted, v.names())
		})
	}
}

// replay walks the ordered trail of one entity for one stateful rule.
func (e *Engine) replay(v *slotEnv, r cachedRule, st *ruleState, s slot) {
	if s.trailLost {
		// Some changes were dropped: continuity is unknown, so a pending episode is
		// cancelled and the next reliable reading is a new first sighting.
		e.cancelEpisode(st)
		st.baselined = false
	}
	for _, c := range s.trail {
		e.observe(v, r, st, c)
	}
}

// observe applies one observation to a stateful rule.
func (e *Engine) observe(v *slotEnv, r cachedRule, st *ruleState, c monitor.Change) {
	cur := c.Cur
	ok := reliableFor(r, cur)
	violating := ok && violates(r, cur)
	first := ok && !st.baselined
	if ok {
		st.baselined = true
	}
	switch {
	case !ok:
		e.cancelEpisode(st) // continuity is unknown: nothing is guessed
	case !violating:
		e.cancelEpisode(st)
		if !r.HasClear || cleared(r, cur) {
			st.armed = true
		}
	case first:
		e.initialState(v, r, st, cur, c.At)
	case r.Duration > 0:
		if st.ep == nil && st.armed {
			q, _ := enters(r, c)
			e.startEpisode(st, c.At, r.Duration, q && c.Source != monitor.SourceSnapshot)
		}
	default: // an immediate rule with a clear value
		if q, fromUnreliable := enters(r, c); q && (st.armed || fromUnreliable) {
			st.armed = false
			l := latch{rule: r.ID, seq: c.Seq, source: c.Source, at: c.At, sensor: cur, fromUnreliable: fromUnreliable}
			if v.permitted(r.ID) {
				e.release(v, r, st, cur.Category, func(muted int) Message {
					return e.alertMessage(r, v.id, cur, l, true, muted, v.names())
				})
			}
		}
	}
}

// fireDue fires the pending durations that have come due. It trusts nothing it
// remembers: the current sensor is re-read, and the alert goes out only if the
// sensor is still reliably violating the rule and sharing still allows it. While
// monitoring is unhealthy nothing fires.
func (e *Engine) fireDue(ctx context.Context, rs *ruleSet) {
	if !e.monitoringOK.Load() {
		return
	}
	now := e.clock.Now()
	var due []int64
	for id, st := range e.states {
		if st.ep != nil && !now.Before(st.ep.due) {
			due = append(due, id)
		}
	}
	if len(due) == 0 {
		return
	}
	slices.SortFunc(due, func(a, b int64) int {
		if c := e.states[a].ep.due.Compare(e.states[b].ep.due); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	for _, id := range due {
		st := e.states[id]
		r, ok := rs.byID[id]
		if !ok {
			e.cancelEpisode(st)
			continue
		}
		s, monitored := e.d.Sensors.Get(r.Entity)
		if !monitored || !usable(r, s) || !reliableFor(r, s) || !violates(r, s) {
			e.cancelEpisode(st)
			continue
		}
		v := &slotEnv{e: e, ctx: ctx, id: r.Entity, now: now}
		v.load()
		if v.accessErr {
			st.ep.due = now.Add(e.cfg.HealthTick) // sharing could not be read: try again, nothing is lost
			continue
		}
		ep := *st.ep
		st.ep, st.armed = nil, false
		e.m.episodesFired.Add(1)
		if !v.permitted(r.ID) {
			continue
		}
		e.release(v, r, st, s.Category, func(muted int) Message {
			return e.durationMessage(r, r.Entity, s, ep, now, muted, v.names())
		})
	}
}

// reseedEpisodes restarts every pending duration from the current sensors after
// monitoring was unhealthy: continuity during an outage cannot be proven, so the
// clock starts again from now. A duration can be late after an outage, never early.
func (e *Engine) reseedEpisodes(rs *ruleSet) {
	now := e.clock.Now()
	for id, r := range rs.byID {
		st := e.states[id]
		if st == nil || r.Duration <= 0 {
			continue
		}
		st.ep = nil
		s, monitored := e.d.Sensors.Get(r.Entity)
		if monitored && usable(r, s) && reliableFor(r, s) && violates(r, s) && st.armed {
			e.startEpisode(st, now, r.Duration, false)
		}
	}
	e.noteEpisodes()
}

func (e *Engine) durationMessage(r cachedRule, id entities.ID, s monitor.Sensor, ep episode, now time.Time, muted int, aliases map[entities.ID]string) Message {
	in := s.Interpretation()
	m := Message{Kind: MsgAlert, RuleID: r.ID, Category: s.Category, Name: displayName(id, s, aliases), Trigger: r.Kind,
		Meaning: in.Meaning, Value: in.Value, HasValue: in.HasValue, Unit: s.Unit, ObservedAt: now, ChangedAt: s.LastChanged,
		Muted: muted, Duration: r.Duration, HeldAtLeast: !ep.exact, entity: id}
	if r.Kind != KindState {
		t := r.Threshold
		m.Threshold = &t
	}
	return m
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
		st.baselined, st.hasFired, st.lastFired = true, true, now
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
		case <-e.bindWake:
			e.bindLegacy(ctx)
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
	if healthy := st.Healthy; healthy != e.monitoringOK.Swap(healthy) && healthy {
		e.reseedDur.Store(true) // monitoring is back: pending durations restart from the current sensors
		e.kickEvaluator()
	}
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

// bindLegacy runs the one-time unit binding and reloads the rules when it bound any.
func (e *Engine) bindLegacy(ctx context.Context) {
	n, err := e.d.Binder.BindLegacyUnits(ctx)
	if err != nil {
		e.log.Error("binding the unit of legacy notification rules failed", "error", err)
		return
	}
	if n > 0 {
		e.log.Info("notification rules bound to their sensor's unit", "rules", n)
		_ = e.Reload(ctx)
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
	case MsgAlert:
		if it.msg.Category.Hazard() {
			e.putMark(it.msg.RuleID, it.msg.ChangedAt)
		}
	case MsgStartup:
		e.startupDelivered(it.msg)
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
	// PendingEpisodes is how many duration rules are counting; the others count
	// episodes since start that began, fired, or ended without firing.
	PendingEpisodes                                   int
	EpisodesStarted, EpisodesFired, EpisodesCancelled uint64
	// Initial-state alerting: hazard alerts raised at start-up, hazards suppressed
	// because the same episode was already delivered, sensors listed in the start-up
	// summary, summaries sent and summaries suppressed by the persisted window.
	InitialAlerts, MarkSuppressed, StartupListed, StartupSummaries, SummarySuppressed uint64
	Digests, Abandoned                                                                uint64
	PendingAtShutdown                                                                 int64
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
	h.InitialAlerts, h.MarkSuppressed, h.StartupListed = e.m.initialAlerts.Load(), e.m.markSuppressed.Load(), e.m.startupListed.Load()
	h.StartupSummaries, h.SummarySuppressed = e.m.startupSummaries.Load(), e.m.summarySuppressed.Load()
	h.PendingEpisodes = int(e.pendingEps.Load())
	h.EpisodesStarted, h.EpisodesFired, h.EpisodesCancelled = e.m.episodesStarted.Load(), e.m.episodesFired.Load(), e.m.episodesCancelled.Load()
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
