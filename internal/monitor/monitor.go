package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/nepthass/nepthass/internal/config"
	"github.com/nepthass/nepthass/internal/homeassistant/ws"
)

// DefaultSyncBufferMax bounds the events held while the snapshot is fetched.
const DefaultSyncBufferMax = 4096

// Config configures a Monitor. Only HomeAssistant is required; everything else
// has a production default and exists so tests can be deterministic.
type Config struct {
	HomeAssistant config.HomeAssistant
	Logger        *slog.Logger
	Now           func() time.Time
	// Sleep waits for d or until ctx ends. Injected to avoid real sleeping in tests.
	Sleep   func(ctx context.Context, d time.Duration) error
	Backoff Backoff
	// Session holds the WebSocket timing and size limits (Endpoint and Token
	// are filled in from HomeAssistant).
	Session       ws.Options
	SyncBufferMax int
	// Liveness is how recent the last frame must be for the monitor to be
	// considered healthy. It defaults to the session read timeout.
	Liveness time.Duration
	// Dial replaces ws.Dial (tests).
	Dial func(context.Context, ws.Options) (*ws.Session, error)
}

// Monitor owns the WebSocket connection to Home Assistant, its lifecycle and
// the Tracker. One Monitor runs per application; Run blocks until its context
// ends. There is no global state.
type Monitor struct {
	cfg      Config
	endpoint string
	token    string
	log      *slog.Logger
	now      func() time.Time

	tracker *Tracker
	mach    *machine
	backoff Backoff
	session atomic.Pointer[ws.Session]
	running atomic.Bool
	done    chan struct{}

	invalid        atomic.Uint64
	dropped        atomic.Uint64
	unexpectedPast atomic.Uint64
}

// New validates the configuration and returns a Monitor that has not started.
func New(cfg Config) (*Monitor, error) {
	endpoint, err := ws.Endpoint(cfg.HomeAssistant.URL)
	if err != nil {
		return nil, fmt.Errorf("monitoring: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Backoff.Initial <= 0 {
		cfg.Backoff = DefaultBackoff()
	}
	if cfg.Backoff.Rand == nil {
		cfg.Backoff.Rand = rand.Int64N
	}
	if cfg.SyncBufferMax <= 0 {
		cfg.SyncBufferMax = DefaultSyncBufferMax
	}
	if cfg.Dial == nil {
		cfg.Dial = ws.Dial
	}
	if cfg.Liveness <= 0 {
		cfg.Liveness = cfg.Session.ReadTimeout
		if cfg.Liveness <= 0 {
			cfg.Liveness = ws.DefaultReadTimeout
		}
	}
	return &Monitor{
		cfg: cfg, endpoint: endpoint, token: cfg.HomeAssistant.Token.Reveal(), log: cfg.Logger, now: cfg.Now,
		tracker: NewTracker(), mach: newMachine(cfg.Now()), backoff: cfg.Backoff, done: make(chan struct{}),
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Tracker exposes the live sensor state. Reads must go through a Reader when
// they are for a Discord user; this accessor is for the application and tests.
func (m *Monitor) Tracker() *Tracker { return m.tracker }

// Done is closed when Run has returned and everything it owns has been released.
func (m *Monitor) Done() <-chan struct{} { return m.done }

func (m *Monitor) to(next ConnState, why string) {
	from, ok := m.mach.transition(next, m.now())
	if !ok {
		m.log.Error("illegal monitoring state transition", "from", string(from), "to", string(next))
		return
	}
	m.log.Debug("monitoring state changed", "from", string(from), "to", string(next), "reason", why)
}

// Run connects, synchronizes and monitors until ctx is canceled, reconnecting
// with bounded, jittered backoff after any failure. It returns only after the
// socket is closed and both session goroutines have exited. Calling Run a
// second time returns immediately.
func (m *Monitor) Run(ctx context.Context) {
	if !m.running.CompareAndSwap(false, true) {
		return
	}
	defer close(m.done)
	m.to(StateConnecting, "starting")
	for {
		var readyAt time.Time
		err := m.runSession(ctx, &readyAt)
		if ctx.Err() != nil {
			break
		}
		class := classifyFailure(err)
		if !readyAt.IsZero() && m.now().Sub(readyAt) >= m.backoff.StableAfter {
			m.backoff.Reset()
		}
		delay := m.backoff.Next(class)
		m.to(StateReconnecting, string(class))
		m.mach.update(func(x *machine) {
			x.failure, x.reconnects, x.nextRetry = class, x.reconnects+1, m.now().Add(delay)
		})
		m.log.Warn("home assistant monitoring connection lost; reconnect scheduled",
			"failure", string(class), "retry_in", delay.String(), "attempt", m.backoff.Attempts())
		if m.cfg.Sleep(ctx, delay) != nil {
			break
		}
		m.to(StateConnecting, "retrying")
	}
	m.to(StateStopped, "shutdown")
	m.log.Info("home assistant monitoring stopped")
}

// runSession runs one connection from dial to termination. It returns the
// error that ended it (ctx.Err() on shutdown). readyAt is set when the
// session reached Ready.
func (m *Monitor) runSession(ctx context.Context, readyAt *time.Time) error {
	opts := m.cfg.Session
	opts.Endpoint, opts.Token = m.endpoint, m.token
	opts.OnConnected = func() { m.to(StateAuthenticating, "transport connected") }
	sess, err := m.cfg.Dial(ctx, opts)
	if err != nil {
		return err
	}
	m.session.Store(sess)
	defer func() {
		m.unexpectedPast.Add(sess.Unexpected())
		m.session.Store(nil)
		sess.Close() // joins the reader and pinger before the next dial
	}()
	m.log.Info("home assistant websocket authenticated", "home_assistant_version", sess.HAVersion())
	m.to(StateSubscribing, "authenticated")

	sink := newSyncSink(m, m.cfg.SyncBufferMax)
	sess.Start(sink.handle)
	if _, err := sess.Call(ctx, "subscribe_events", map[string]any{"event_type": "state_changed"}); err != nil {
		return fmt.Errorf("subscribe to state changes: %w", err)
	}
	m.log.Info("home assistant event subscription active")
	m.to(StateSynchronizing, "subscribed")

	raw, err := sess.Call(ctx, "get_states", nil)
	if err != nil {
		return fmt.Errorf("fetch the state snapshot: %w", err)
	}
	summary, err := m.synchronize(sink, raw)
	if err != nil {
		return err
	}
	now := m.now()
	m.mach.update(func(x *machine) { x.lastSync = now })
	*readyAt = now
	m.to(StateReady, "synchronized")
	m.log.Info("home assistant initial synchronization completed",
		"sensors", m.tracker.Len(), "added", summary.Added, "updated", summary.Updated, "removed", summary.Removed)

	select {
	case <-sess.Done():
		return sess.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// process validates and applies one state_changed event. It runs on the
// session's reader goroutine and does only O(1) work under the tracker lock.
func (m *Monitor) process(raw json.RawMessage) {
	u, err := ParseEvent(raw)
	switch {
	case errors.Is(err, errIgnored):
		return
	case err != nil:
		m.invalid.Add(1)
		return
	}
	now := m.now()
	m.tracker.Apply(u, now)
	m.mach.update(func(x *machine) { x.lastEvent = now })
}

// Status returns a consistent copy of the monitor's health.
func (m *Monitor) Status() Status {
	x := m.mach.view()
	st := Status{
		State: x.state, StateSince: x.since, LastEventAt: x.lastEvent, LastSyncAt: x.lastSync, NextRetryAt: x.nextRetry,
		LastFailure: x.failure, Reconnects: x.reconnects, Tracked: m.tracker.Len(),
		InvalidMessages: m.invalid.Load(), DroppedEvents: m.dropped.Load(), Unexpected: m.unexpectedPast.Load(),
	}
	st.Transport, st.Authenticated, st.Subscribed, st.Synchronized = layersFor(x.state)
	c := m.tracker.Counters()
	st.DuplicateEvents, st.StaleEvents, st.DroppedEvents = c.Duplicates, c.Stale, st.DroppedEvents+c.Dropped
	if sess := m.session.Load(); sess != nil {
		st.Unexpected += sess.Unexpected()
		st.LastFrameAt = sess.LastFrame()
		st.Healthy = x.state == StateReady && m.now().Sub(st.LastFrameAt) <= m.cfg.Liveness
	}
	return st
}

// WaitFor blocks until pred(Status) holds or ctx ends. It reacts to state
// changes and processed events; it does not poll.
func (m *Monitor) WaitFor(ctx context.Context, pred func(Status) bool) error {
	for {
		m.mach.mu.Lock()
		ch := m.mach.changed
		m.mach.mu.Unlock()
		if pred(m.Status()) {
			return nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Violations counts illegal state transitions (always zero in a correct build).
func (m *Monitor) Violations() uint64 {
	m.mach.mu.Lock()
	defer m.mach.mu.Unlock()
	return m.mach.violations
}
