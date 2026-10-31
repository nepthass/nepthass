package notify

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// The mailbox is the buffer between the monitor callback and the evaluator. It
// holds at most ONE slot per watched entity, so its size is bounded by the number
// of rules (MaxRules), never by the event rate, and recording a change is O(1)
// with no I/O and no goroutine.
//
// What survives coalescing, exactly. While an entity waits to be evaluated, any
// number of changes may arrive. The slot keeps:
//
//   - the LATEST sensor state (the evaluator re-arms and compares against it);
//   - per rule, the FIRST qualifying transition seen since the last evaluation
//     (the latch): its sequence number, source (live, replay or snapshot),
//     observation time and the violating sensor state, and whether the previous
//     state was unreliable. A violation that comes and goes before the evaluator
//     runs (a door opened and closed) therefore still produces its alert, marked
//     Cleared;
//   - whether any snapshot change was coalesced, a reset flag (a removal or a
//     first observation was seen, so every rule of the entity re-baselines
//     silently), and the time of the first unprocessed change (for lag).
//
// What does NOT survive: intermediate values between the first qualifying
// transition and the latest state (only the first qualifying one is kept, later
// ones in the same window are counted as coalesced); intermediate numeric
// readings; and a pending transition of an entity that is removed before it is
// evaluated (the latch dies with the entity: it can no longer be checked for
// sharing or monitoring). Order is preserved per entity (a slot is one entity)
// and slots are drained in the order they first became dirty.
//
// Stateful rules (a duration or a clear value, see cachedRule.stateful) cannot
// live with coalescing: a duration rule must know whether the condition stayed
// continuous, and a hysteresis rule the order of its crossings. For an entity
// with such a rule the slot therefore also keeps an ordered TRAIL of the
// changes, bounded by maxTrail. If the trail overflows, the oldest changes are
// dropped and trailLost is set, which makes the evaluator treat the continuity
// as unknown (it cancels pending episodes instead of guessing).
const maxTrail = 16

type latch struct {
	rule           int64
	seq            uint64
	source         monitor.ChangeSource
	at             time.Time
	sensor         monitor.Sensor
	fromUnreliable bool
}

type slot struct {
	cur         monitor.Sensor
	haveCur     bool
	removed     bool
	reset       bool
	since       time.Time
	lastSeq     uint64
	lastSource  monitor.ChangeSource
	anySnapshot bool
	// latches holds the first qualifying transition of each plain rule: at most one
	// per rule of the entity, so its size is bounded by the number of rules.
	latches []latch
	// trail and trailLost serve stateful rules only.
	trail     []monitor.Change
	trailLost bool
}

func (s *slot) latchFor(rule int64) (latch, bool) {
	for _, l := range s.latches {
		if l.rule == rule {
			return l, true
		}
	}
	return latch{}, false
}

type mailbox struct {
	mu         sync.Mutex
	slots      map[entities.ID]slot
	order      []entities.ID
	capSlots   int
	overflowed bool
	wake       chan struct{}

	coalesced atomic.Uint64
	overflows atomic.Uint64
}

func newMailbox(capSlots int) *mailbox {
	return &mailbox{slots: map[entities.ID]slot{}, capSlots: capSlots, wake: make(chan struct{}, 1)}
}

func (m *mailbox) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// record merges one change for a watched entity. rules are that entity's enabled
// rules. It never blocks on anything but the mailbox mutex, which is only ever
// held for O(1) work.
func (m *mailbox) record(c monitor.Change, rules []cachedRule) {
	m.mu.Lock()
	s, exists := m.slots[c.ID]
	if !exists {
		if len(m.slots) >= m.capSlots {
			m.overflowed = true // defensive: unreachable while rules are capped at MaxRules
			m.mu.Unlock()
			m.overflows.Add(1)
			m.signal()
			return
		}
		s.since = c.At
		m.order = append(m.order, c.ID)
	} else {
		m.coalesced.Add(1)
	}
	s.lastSeq, s.lastSource = c.Seq, c.Source
	if c.Source == monitor.SourceSnapshot {
		s.anySnapshot = true
	}
	if c.Removed {
		s.removed, s.reset, s.haveCur = true, true, false
		s.cur, s.latches, s.trail, s.trailLost = monitor.Sensor{}, nil, nil, false
	} else {
		s.removed, s.haveCur, s.cur = false, true, c.Cur
		if !c.HadPrev {
			s.reset = true
		}
		stateful := false
		for _, r := range rules {
			if r.stateful() {
				stateful = true
				continue
			}
			qualifies, fromUnreliable := enters(r, c)
			if !qualifies {
				continue
			}
			if _, dup := s.latchFor(r.ID); dup {
				continue // keep the first qualifying transition
			}
			s.latches = append(s.latches, latch{rule: r.ID, seq: c.Seq, source: c.Source, at: c.At, sensor: c.Cur, fromUnreliable: fromUnreliable})
		}
		if stateful {
			if len(s.trail) >= maxTrail {
				s.trail = append(s.trail[:0:0], s.trail[1:]...)
				s.trailLost = true
			}
			s.trail = append(s.trail, c)
		}
	}
	m.slots[c.ID] = s
	m.mu.Unlock()
	m.signal()
}

// drain takes everything pending, in the order entities first became dirty.
func (m *mailbox) drain() (map[entities.ID]slot, []entities.ID, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slots, order, over := m.slots, m.order, m.overflowed
	m.slots, m.order, m.overflowed = make(map[entities.ID]slot, 8), nil, false
	return slots, order, over
}

// pending returns how many entities wait and how long the oldest has waited.
func (m *mailbox) pending(now time.Time) (int, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var oldest time.Duration
	for _, s := range m.slots {
		if age := now.Sub(s.since); age > oldest {
			oldest = age
		}
	}
	return len(m.slots), oldest
}
