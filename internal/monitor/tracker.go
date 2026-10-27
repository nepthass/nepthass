package monitor

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

// MaxTracked bounds memory: a buggy or hostile peer cannot grow the tracker
// past this many distinct sensors.
const MaxTracked = 10000

// Sensor is the latest known state of one monitored entity. Only the fields
// NeptHass needs are kept; other attributes are never retained.
type Sensor struct {
	ID          entities.ID
	Category    Category
	Name        string
	DeviceClass string
	Unit        string
	Raw         string
	LastChanged time.Time
	LastUpdated time.Time
	// ObservedAt is when NeptHass received this state (not Home Assistant's clock).
	ObservedAt time.Time
	// Revision counts accepted updates of this sensor since it was (re)added.
	Revision uint64
}

// Interpretation is the derived meaning of the raw state.
func (s Sensor) Interpretation() Interpretation { return Interpret(s.Category, s.Raw) }

// Outcome is what Apply did with an update.
type Outcome string

const (
	// OutcomeApplied: the update was newer and replaced the stored state
	// (or added the sensor).
	OutcomeApplied Outcome = "applied"
	// OutcomeDuplicate: an identical update was already applied.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeStale: the update is older than the stored state and was dropped.
	OutcomeStale Outcome = "stale"
	// OutcomeRemoved: the sensor was removed (entity deleted, or it stopped classifying).
	OutcomeRemoved Outcome = "removed"
	// OutcomeUntracked: the update concerns something NeptHass does not track.
	OutcomeUntracked Outcome = "untracked"
	// OutcomeDropped: the tracker is full.
	OutcomeDropped Outcome = "dropped"
)

// Result describes one Apply.
type Result struct {
	Outcome Outcome
	// ValueChanged is true when the raw state value differs from before. An
	// attribute-only update has Outcome applied and ValueChanged false.
	ValueChanged bool
}

// Counters are monotonic totals for observability.
type Counters struct {
	Applied, Duplicates, Stale, Removed, Dropped uint64
}

// Tracker holds the latest state of monitored sensors. It is safe for
// concurrent use. Delivery is not assumed exactly-once or ordered: per sensor
// an update is applied only if its last_updated is newer than the stored one.
type Tracker struct {
	mu      sync.RWMutex
	sensors map[entities.ID]Sensor

	applied, duplicates, stale, removed, dropped atomic.Uint64
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker { return &Tracker{sensors: make(map[entities.ID]Sensor)} }

func sameObservation(s Sensor, u Update) bool {
	return s.Raw == u.Raw && s.Unit == u.Unit && s.DeviceClass == u.DeviceClass && s.Name == u.Name &&
		s.LastChanged.Equal(u.LastChanged) && s.LastUpdated.Equal(u.LastUpdated)
}

// Apply merges one validated update using the ordering rules.
func (t *Tracker) Apply(u Update, now time.Time) Result {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, exists := t.sensors[u.ID]

	switch {
	case u.Removed:
		if !exists {
			return Result{Outcome: OutcomeUntracked}
		}
		if !u.PrevUpdated.IsZero() && u.PrevUpdated.Before(cur.LastUpdated) {
			t.stale.Add(1)
			return Result{Outcome: OutcomeStale}
		}
		return t.remove(u.ID)
	case u.Category == "":
		// Not a class we monitor (or its class changed): stop tracking it.
		if exists {
			return t.remove(u.ID)
		}
		return Result{Outcome: OutcomeUntracked}
	case !exists:
		if len(t.sensors) >= MaxTracked {
			t.dropped.Add(1)
			return Result{Outcome: OutcomeDropped}
		}
		t.sensors[u.ID] = newSensor(u, now, 1)
		t.applied.Add(1)
		return Result{Outcome: OutcomeApplied, ValueChanged: true}
	}

	switch u.LastUpdated.Compare(cur.LastUpdated) {
	case -1:
		t.stale.Add(1)
		return Result{Outcome: OutcomeStale}
	case 0:
		if sameObservation(cur, u) {
			t.duplicates.Add(1)
			return Result{Outcome: OutcomeDuplicate}
		}
	}
	next := newSensor(u, now, cur.Revision+1)
	t.sensors[u.ID] = next
	t.applied.Add(1)
	return Result{Outcome: OutcomeApplied, ValueChanged: cur.Raw != u.Raw}
}

func (t *Tracker) remove(id entities.ID) Result {
	delete(t.sensors, id)
	t.removed.Add(1)
	return Result{Outcome: OutcomeRemoved}
}

func newSensor(u Update, now time.Time, revision uint64) Sensor {
	return Sensor{
		ID: u.ID, Category: u.Category, Name: u.Name, DeviceClass: u.DeviceClass, Unit: u.Unit, Raw: u.Raw,
		LastChanged: u.LastChanged, LastUpdated: u.LastUpdated, ObservedAt: now, Revision: revision,
	}
}

// Reconciled summarizes a Reconcile.
type Reconciled struct {
	Added, Updated, Unchanged, Removed, Dropped int
}

// Reconcile replaces the tracker's contents with an authoritative snapshot.
// The snapshot wins regardless of timestamps (so a clock step or a Home
// Assistant restart cannot leave a value stuck) and sensors that are not in it
// are removed. Updates for unclassified entities are skipped.
func (t *Tracker) Reconcile(snapshot []Update, now time.Time) Reconciled {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := make(map[entities.ID]Sensor, len(snapshot))
	var r Reconciled
	for _, u := range snapshot {
		if u.Category == "" || u.Removed {
			continue
		}
		if _, dup := next[u.ID]; dup {
			continue
		}
		if len(next) >= MaxTracked {
			r.Dropped++
			continue
		}
		prev, existed := t.sensors[u.ID]
		switch {
		case !existed:
			next[u.ID] = newSensor(u, now, 1)
			r.Added++
		case sameObservation(prev, u):
			prev.ObservedAt = now
			next[u.ID] = prev
			r.Unchanged++
		default:
			next[u.ID] = newSensor(u, now, prev.Revision+1)
			r.Updated++
		}
	}
	for id := range t.sensors {
		if _, kept := next[id]; !kept {
			r.Removed++
		}
	}
	t.sensors = next
	t.applied.Add(uint64(r.Added + r.Updated))
	t.removed.Add(uint64(r.Removed))
	t.dropped.Add(uint64(r.Dropped))
	return r
}

// Get returns a copy of one sensor.
func (t *Tracker) Get(id entities.ID) (Sensor, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.sensors[id]
	return s, ok
}

// All returns copies of every tracked sensor, sorted by entity ID.
func (t *Tracker) All() []Sensor {
	t.mu.RLock()
	out := make([]Sensor, 0, len(t.sensors))
	for _, s := range t.sensors {
		out = append(out, s)
	}
	t.mu.RUnlock()
	slices.SortFunc(out, func(a, b Sensor) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

// Len is the number of tracked sensors.
func (t *Tracker) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.sensors)
}

// Counters returns the monotonic totals.
func (t *Tracker) Counters() Counters {
	return Counters{Applied: t.applied.Load(), Duplicates: t.duplicates.Load(), Stale: t.stale.Load(), Removed: t.removed.Load(), Dropped: t.dropped.Load()}
}
