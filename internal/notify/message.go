package notify

import (
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// MessageKind says what a Message announces.
type MessageKind uint8

const (
	// MsgAlert: a rule fired.
	MsgAlert MessageKind = iota + 1
	// MsgMonitoringDown: Home Assistant monitoring has been unhealthy for the threshold.
	MsgMonitoringDown
	// MsgMonitoringRecovered: monitoring is back after a delivered MsgMonitoringDown.
	MsgMonitoringRecovered
	// MsgDigest: the CURRENT state of rules whose alerts could not be delivered.
	MsgDigest
)

// Lane is a delivery lane.
type Lane uint8

const (
	// LaneNormal carries motion, door, window, temperature and battery alerts.
	LaneNormal Lane = iota + 1
	// LaneCritical carries leak alerts, monitoring-down, recovery and digests.
	LaneCritical
)

// Message is what a Sender delivers. It is transport-neutral: it carries facts,
// and formatting (Discord or otherwise) is the transport's job. It deliberately
// carries no entity ID that a transport could print: the ID needed for the
// permission re-check is an unexported field.
type Message struct {
	Kind MessageKind

	// RuleID, Category, Name, Trigger, Meaning, Value, HasValue, Unit and
	// Threshold describe an alert. Name is the alias, else the Home Assistant
	// friendly name, else a category noun; never the entity ID.
	RuleID    int64
	Category  monitor.Category
	Name      string
	Trigger   Kind
	Meaning   monitor.Meaning
	Value     float64
	HasValue  bool
	Unit      string
	Threshold *float64
	// ObservedAt is when NeptHass observed the qualifying state (monitor clock);
	// the TTL runs from it. ChangedAt is Home Assistant's last_changed.
	ObservedAt time.Time
	ChangedAt  time.Time
	// Late: the transition was found by a snapshot after a reconnect, so it may
	// have happened earlier. Uncertain: a leak that reached wet from an unknown
	// or unavailable state. Cleared: the sensor is no longer in the alerting
	// state by the time the alert was produced (a transient violation).
	Late      bool
	Uncertain bool
	Cleared   bool
	// Muted counts alerts for this rule suppressed by its cooldown since the
	// previous alert.
	Muted int

	// Lines and MoreLines describe a digest: the current state of up to ten
	// still-violating rules, and how many more there were.
	Lines     []DigestLine
	MoreLines int

	// DownSince, DownFor, Failure and Episode describe a monitoring-down or
	// recovery message.
	DownSince time.Time
	DownFor   time.Duration
	Failure   monitor.Failure
	Episode   uint64

	entity entities.ID // permission re-check only; never rendered
}

// DigestLine is one still-violating rule in a digest, as of when it was built.
type DigestLine struct {
	Category  monitor.Category
	Name      string
	Trigger   Kind
	Meaning   monitor.Meaning
	Value     float64
	HasValue  bool
	Unit      string
	Threshold *float64
	ChangedAt time.Time

	rule   int64
	entity entities.ID
}

// Lane returns the lane a message travels in.
func (m Message) Lane() Lane {
	if m.Kind == MsgAlert && !m.Category.Hazard() {
		return LaneNormal
	}
	return LaneCritical
}

// priority orders messages inside the critical lane when it is full: an active
// leak alert outranks monitoring-down, which outranks a digest, which outranks
// a recovery notice.
func (m Message) priority() int {
	switch m.Kind {
	case MsgAlert:
		if m.Category.Hazard() {
			return 3
		}
		return 1
	case MsgMonitoringDown:
		return 2
	case MsgDigest:
		return 1
	}
	return 0
}
