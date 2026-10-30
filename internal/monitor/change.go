package monitor

import (
	"time"

	"github.com/nepthass/nepthass/internal/entities"
)

// ChangeSource says which path of the monitor produced a Change.
type ChangeSource uint8

const (
	// SourceLive is a state_changed event processed after the snapshot was
	// reconciled, on the WebSocket reader goroutine.
	SourceLive ChangeSource = iota + 1
	// SourceReplay is an event that arrived while the snapshot was being fetched.
	// It is processed right after the snapshot was reconciled, before any live event.
	SourceReplay
	// SourceSnapshot is a difference found by reconciling an authoritative
	// snapshot (the first synchronization and every one after a reconnect).
	SourceSnapshot
)

// String returns a stable name for logs and tests.
func (s ChangeSource) String() string {
	switch s {
	case SourceLive:
		return "live"
	case SourceReplay:
		return "replay"
	case SourceSnapshot:
		return "snapshot"
	}
	return "unknown"
}

// Change is one accepted mutation of the Tracker, reported to Config.OnChange.
// It carries facts only: the monitor does not decide what is noteworthy.
//
// A Change is produced for every update the tracker accepts: a sensor that is
// added, an applied update (including one that changes only an attribute, so a
// consumer compares Prev.Raw and Cur.Raw to see whether the value moved), and a
// removal (an entity deleted, or one that stopped being classified). Duplicate,
// stale, dropped and untracked updates change nothing and produce no Change.
//
// Ordering contract. Seq is strictly increasing in the order the tracker was
// mutated, and OnChange is called in Seq order, synchronously, by the goroutine
// that mutated the tracker, before that goroutine mutates it again. Within one
// synchronization:
//
//  1. all SourceSnapshot changes come first: additions and updates in snapshot
//     order, then removals in entity ID order;
//  2. then SourceReplay changes, in the order Home Assistant delivered the events;
//  3. then SourceLive changes.
//
// The monitor is the only mutator of its Tracker and serializes synchronization
// (see Monitor.synchronize), so this order is a guarantee rather than a tendency.
// The guarantee does not extend to a caller that mutates the Tracker concurrently
// by other means.
type Change struct {
	// ID is the entity.
	ID entities.ID
	// Seq numbers the changes of this process, starting at 1.
	Seq uint64
	// Source is the path that produced the change.
	Source ChangeSource
	// HadPrev is false for the first observation of a sensor (including a sensor
	// added by a snapshot, and one that is added again after a removal). A first
	// observation is a baseline, not a transition: Prev is the zero Sensor.
	HadPrev bool
	// Prev is the stored state before the change; valid only when HadPrev.
	Prev Sensor
	// Cur is the stored state after the change; the zero Sensor when Removed.
	Cur Sensor
	// Removed is true when the sensor is no longer tracked. Prev is its last state.
	Removed bool
	// At is when NeptHass applied the change (the monitor's clock, not Home
	// Assistant's): the same instant as Cur.ObservedAt. Home Assistant's own
	// timestamps are Cur.LastChanged and Cur.LastUpdated.
	At time.Time
}
