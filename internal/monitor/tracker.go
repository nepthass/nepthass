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
	// seq numbers the changes handed out by applyChange and reconcileChanges, in
	// the order the tracker was mutated. Guarded by mu.
	seq uint64

	applied, duplicates, stale, removed, dropped atomic.Uint64
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker { return &Tracker{sensors: make(map[entities.ID]Sensor)} }

// notReporting reports whether a raw state means "no real value".
func notReporting(raw string) bool { return raw == "unavailable" || raw == "unknown" }

// retainClass lets a tracked sensor that reports unavailable or unknown, with
// no device_class attribute at all, keep the class (and unit and name) it
// already has. Home Assistant normally keeps device_class on an unavailable
// entity, but an update can omit it (for example a placeholder whose registry
// entry never recorded one). Dropping the sensor then would make it vanish from
// the listing instead of showing "unavailable", while retaining it is safe: its
// meaning comes from the raw state, which is not-reporting and never "safe".
//
// It does not apply to a real value without a class (it cannot be interpreted,
// so the sensor stops being tracked), nor to an explicit different class.
func retainClass(cur Sensor, u Update) Update {
	if u.Removed || u.Category != "" || u.DeviceClass != "" || !notReporting(u.Raw) {
		return u
	}
	u.Category, u.DeviceClass = cur.Category, cur.DeviceClass
	if u.Unit == "" {
		u.Unit = cur.Unit
	}
	if u.Name == "" {
		u.Name = cur.Name
	}
	return u
}

func sameObservation(s Sensor, u Update) bool {
	return s.Raw == u.Raw && s.Unit == u.Unit && s.DeviceClass == u.DeviceClass && s.Name == u.Name &&
		s.LastChanged.Equal(u.LastChanged) && s.LastUpdated.Equal(u.LastUpdated)
}

// Apply merges one validated update using the ordering rules.
func (t *Tracker) Apply(u Update, now time.Time) Result {
	res, _, _ := t.apply(SourceLive, u, now, false)
	return res
}

// applyChange is Apply that also returns the Change the update caused. The
// Change is built under the tracker lock, so its Seq follows the mutation order,
// and is returned to the caller to deliver after the lock is released. The bool
// is false when the update changed nothing (duplicate, stale, dropped, untracked).
func (t *Tracker) applyChange(src ChangeSource, u Update, now time.Time) (Result, Change, bool) {
	return t.apply(src, u, now, true)
}

// apply is the single implementation behind Apply and applyChange. With emit
// false no Change is built and no sequence number is consumed.
func (t *Tracker) apply(src ChangeSource, u Update, now time.Time, emit bool) (Result, Change, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, exists := t.sensors[u.ID]
	if exists {
		u = retainClass(cur, u)
	}
	switch {
	case u.Removed:
		if !exists {
			return Result{Outcome: OutcomeUntracked}, Change{}, false
		}
		if !u.PrevUpdated.IsZero() && u.PrevUpdated.Before(cur.LastUpdated) {
			t.stale.Add(1)
			return Result{Outcome: OutcomeStale}, Change{}, false
		}
		res := t.remove(u.ID)
		ch, ok := t.changeLocked(emit, src, u.ID, true, cur, Sensor{}, true, now)
		return res, ch, ok
	case u.Category == "":
		// Not a class we monitor (or its class changed): stop tracking it.
		if exists {
			res := t.remove(u.ID)
			ch, ok := t.changeLocked(emit, src, u.ID, true, cur, Sensor{}, true, now)
			return res, ch, ok
		}
		return Result{Outcome: OutcomeUntracked}, Change{}, false
	case !exists:
		if len(t.sensors) >= MaxTracked {
			t.dropped.Add(1)
			return Result{Outcome: OutcomeDropped}, Change{}, false
		}
		s := newSensor(u, now, 1)
		t.sensors[u.ID] = s
		t.applied.Add(1)
		ch, ok := t.changeLocked(emit, src, u.ID, false, Sensor{}, s, false, now)
		return Result{Outcome: OutcomeApplied, ValueChanged: true}, ch, ok
	}
	switch u.LastUpdated.Compare(cur.LastUpdated) {
	case -1:
		t.stale.Add(1)
		return Result{Outcome: OutcomeStale}, Change{}, false
	case 0:
		if sameObservation(cur, u) {
			t.duplicates.Add(1)
			return Result{Outcome: OutcomeDuplicate}, Change{}, false
		}
	}
	next := newSensor(u, now, cur.Revision+1)
	t.sensors[u.ID] = next
	t.applied.Add(1)
	ch, ok := t.changeLocked(emit, src, u.ID, true, cur, next, false, now)
	return Result{Outcome: OutcomeApplied, ValueChanged: cur.Raw != u.Raw}, ch, ok
}

// changeLocked numbers and builds a Change. The caller holds t.mu. It returns
// false, and consumes no sequence number, when emission was not requested.
func (t *Tracker) changeLocked(emit bool, src ChangeSource, id entities.ID, hadPrev bool, prev, cur Sensor, removed bool, now time.Time) (Change, bool) {
	if !emit {
		return Change{}, false
	}
	t.seq++
	return Change{ID: id, Seq: t.seq, Source: src, HadPrev: hadPrev, Prev: prev, Cur: cur, Removed: removed, At: now}, true
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
	r, _ := t.reconcile(snapshot, now, false)
	return r
}

// reconcileChanges is Reconcile that also returns what it changed, as
// SourceSnapshot changes in a deterministic order: additions and updates in
// snapshot order, then removals in entity ID order. Unchanged sensors produce
// nothing. The slice is built under the tracker lock; the caller delivers it
// after the lock is released.
func (t *Tracker) reconcileChanges(snapshot []Update, now time.Time) (Reconciled, []Change) {
	return t.reconcile(snapshot, now, true)
}

func (t *Tracker) reconcile(snapshot []Update, now time.Time, emit bool) (Reconciled, []Change) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := make(map[entities.ID]Sensor, len(snapshot))
	var r Reconciled
	var changes []Change
	for _, u := range snapshot {
		prev, existed := t.sensors[u.ID]
		if existed {
			u = retainClass(prev, u)
		}
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
		switch {
		case !existed:
			s := newSensor(u, now, 1)
			next[u.ID] = s
			r.Added++
			if ch, ok := t.changeLocked(emit, SourceSnapshot, u.ID, false, Sensor{}, s, false, now); ok {
				changes = append(changes, ch)
			}
		case sameObservation(prev, u):
			prev.ObservedAt = now
			next[u.ID] = prev
			r.Unchanged++
		default:
			s := newSensor(u, now, prev.Revision+1)
			next[u.ID] = s
			r.Updated++
			if ch, ok := t.changeLocked(emit, SourceSnapshot, u.ID, true, prev, s, false, now); ok {
				changes = append(changes, ch)
			}
		}
	}
	var gone []entities.ID
	for id := range t.sensors {
		if _, kept := next[id]; !kept {
			r.Removed++
			if emit {
				gone = append(gone, id)
			}
		}
	}
	slices.Sort(gone) // map order is random; the change order must not be
	for _, id := range gone {
		if ch, ok := t.changeLocked(emit, SourceSnapshot, id, true, t.sensors[id], Sensor{}, true, now); ok {
			changes = append(changes, ch)
		}
	}
	t.sensors = next
	t.applied.Add(uint64(r.Added + r.Updated))
	t.removed.Add(uint64(r.Removed))
	t.dropped.Add(uint64(r.Dropped))
	return r, changes
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
