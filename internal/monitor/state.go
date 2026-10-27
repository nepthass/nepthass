package monitor

import (
	"sync"
	"time"
)

// ConnState is the monitor's connection state. Each state answers a different
// question about health; only Ready means events are flowing and the cache
// was synchronized.
type ConnState string

const (
	StateDisconnected   ConnState = "disconnected"   // created, not started
	StateConnecting     ConnState = "connecting"     // dialing the WebSocket
	StateAuthenticating ConnState = "authenticating" // transport up, token being checked
	StateSubscribing    ConnState = "subscribing"    // authenticated, subscribing to state_changed
	StateSynchronizing  ConnState = "synchronizing"  // subscribed, fetching and reconciling the snapshot
	StateReady          ConnState = "ready"          // synchronized and receiving events
	StateReconnecting   ConnState = "reconnecting"   // waiting out the backoff
	StateStopped        ConnState = "stopped"        // shut down
)

// legal lists the only allowed transitions.
var legal = map[ConnState][]ConnState{
	StateDisconnected:   {StateConnecting, StateStopped},
	StateConnecting:     {StateAuthenticating, StateReconnecting, StateStopped},
	StateAuthenticating: {StateSubscribing, StateReconnecting, StateStopped},
	StateSubscribing:    {StateSynchronizing, StateReconnecting, StateStopped},
	StateSynchronizing:  {StateReady, StateReconnecting, StateStopped},
	StateReady:          {StateReconnecting, StateStopped},
	StateReconnecting:   {StateConnecting, StateStopped},
	StateStopped:        {},
}

// CanTransitionTo reports whether the state machine allows from -> to.
func (s ConnState) CanTransitionTo(to ConnState) bool {
	for _, next := range legal[s] {
		if next == to {
			return true
		}
	}
	return false
}

// Status is a point-in-time copy of everything /health reports. The first
// four booleans are the layers of the design document; Healthy additionally
// requires proof of life.
type Status struct {
	State      ConnState
	StateSince time.Time

	Transport     bool // a WebSocket to Home Assistant exists
	Authenticated bool // Home Assistant accepted the token
	Subscribed    bool // state_changed events will be delivered
	Synchronized  bool // the cache was reconciled with an authoritative snapshot
	// Healthy means Ready and a frame (event or pong) arrived within the
	// liveness window. Values are only presented as current when Healthy.
	Healthy bool

	LastFrameAt time.Time // last frame of any kind from Home Assistant
	LastEventAt time.Time // last monitored state_changed event processed
	LastSyncAt  time.Time // last completed snapshot reconciliation
	NextRetryAt time.Time // when the next attempt starts (Reconnecting)

	LastFailure Failure
	Reconnects  uint64
	Tracked     int

	InvalidMessages uint64 // payloads rejected by validation
	Unexpected      uint64 // protocol messages that were not recognized
	DuplicateEvents uint64
	StaleEvents     uint64
	DroppedEvents   uint64 // buffer overflow and tracker-full drops
}

// layersFor derives the four layers from the state.
func layersFor(s ConnState) (transport, authed, subscribed, synced bool) {
	switch s {
	case StateAuthenticating:
		return true, false, false, false
	case StateSubscribing:
		return true, true, false, false
	case StateSynchronizing:
		return true, true, true, false
	case StateReady:
		return true, true, true, true
	case StateDisconnected, StateConnecting, StateReconnecting, StateStopped:
	}
	return false, false, false, false
}

// machine is the concurrency-safe state holder. It broadcasts every change.
type machine struct {
	mu         sync.Mutex
	state      ConnState
	since      time.Time
	lastEvent  time.Time
	lastSync   time.Time
	nextRetry  time.Time
	failure    Failure
	reconnects uint64
	violations uint64
	changed    chan struct{}
}

func newMachine(now time.Time) *machine {
	return &machine{state: StateDisconnected, since: now, changed: make(chan struct{})}
}

// touch wakes waiters. The caller holds mu.
func (m *machine) touch() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// transition moves to the next state if the table allows it. An illegal
// transition is a programming error: it is counted, ignored and reported.
func (m *machine) transition(to ConnState, now time.Time) (from ConnState, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	from = m.state
	if !from.CanTransitionTo(to) {
		m.violations++
		return from, false
	}
	m.state, m.since = to, now
	if to != StateReconnecting {
		m.nextRetry = time.Time{}
	}
	m.touch()
	return from, true
}

// view is a copy of the machine's fields, safe to use without the lock.
type view struct {
	state                      ConnState
	since, lastEvent, lastSync time.Time
	nextRetry                  time.Time
	failure                    Failure
	reconnects                 uint64
}

func (m *machine) view() view {
	m.mu.Lock()
	defer m.mu.Unlock()
	return view{state: m.state, since: m.since, lastEvent: m.lastEvent, lastSync: m.lastSync, nextRetry: m.nextRetry, failure: m.failure, reconnects: m.reconnects}
}

func (m *machine) update(f func(*machine)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m)
	m.touch()
}
