package notify

import (
	"errors"
	"fmt"
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

const (
	// MaxRules is the most rules that can exist. The service enforces it inside
	// its transaction and migration 0003 enforces it with a trigger; a test keeps
	// the two numbers equal.
	MaxRules = 200
	// MaxCooldown is the longest cooldown override.
	MaxCooldown = 24 * time.Hour
	// MaxThreshold bounds the absolute value of a numeric threshold.
	MaxThreshold = 1_000_000
	// MinDuration and MaxDuration bound the duration of a duration rule; zero means
	// an immediate rule.
	MinDuration = time.Minute
	MaxDuration = 24 * time.Hour
	// SystemActor is the audit actor of changes NeptHass makes by itself (the
	// one-time binding of a legacy rule's unit).
	SystemActor = "system"
)

// Kind says when a rule fires.
type Kind string

const (
	// KindState fires on the category's alert state: open (door, window), wet
	// (leak), detected (motion) or low (battery low). It applies to binary sensors.
	KindState Kind = "state"
	// KindAbove fires when a numeric sensor rises above the threshold.
	KindAbove Kind = "above"
	// KindBelow fires when a numeric sensor falls below the threshold.
	KindBelow Kind = "below"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k == KindState || k == KindAbove || k == KindBelow }

// Settings is the guild-wide notification configuration.
type Settings struct {
	GuildID   string
	ChannelID string
	Enabled   bool
	UpdatedBy string
	UpdatedAt time.Time
	// StartupSummaryAt is when the last start-up summary was delivered (zero:
	// never). It limits summaries to one per window, across restarts.
	StartupSummaryAt time.Time
}

// Config is what an administrator sees: whether notifications are configured at
// all, the settings, and how many rules exist.
type Config struct {
	// Configured is false until a channel has been set. Notifications are then
	// disabled, which is the default.
	Configured bool
	Settings   Settings
	Rules      int
}

// Active reports whether alerts may be sent at all (configured and enabled).
func (c Config) Active() bool { return c.Configured && c.Settings.Enabled }

// Rule is one stored notification rule.
type Rule struct {
	ID     int64
	Entity entities.ID
	Kind   Kind
	// Threshold is set for KindAbove and KindBelow and nil for KindState.
	Threshold *float64
	// Cooldown nil selects the category default; a pointer to zero means no cooldown.
	Cooldown *time.Duration
	// Duration is how long the condition must hold before the rule fires; zero is
	// an immediate rule.
	Duration time.Duration
	// Clear is the value at which a numeric rule re-arms (hysteresis); nil: it
	// re-arms as soon as the sensor no longer violates the threshold.
	Clear *float64
	// Unit is the unit the threshold is expressed in, bound when a numeric rule is
	// created. Empty on a numeric rule means it predates unit binding and is
	// unresolved: it does not evaluate. Always empty on a state rule.
	Unit      string
	Enabled   bool
	CreatedBy string
	CreatedAt time.Time
	UpdatedBy string
	UpdatedAt time.Time
}

// RuleStatus explains whether a rule can currently fire.
type RuleStatus string

const (
	// StatusActive: enabled, shared, monitored and compatible.
	StatusActive RuleStatus = "active"
	// StatusPaused: an administrator paused it.
	StatusPaused RuleStatus = "paused"
	// StatusNeedsSharing: the entity is no longer shared with at least read access.
	StatusNeedsSharing RuleStatus = "needs_sharing"
	// StatusNotMonitored: the sensor is not currently monitored (removed from Home
	// Assistant, or monitoring has not synchronized yet).
	StatusNotMonitored RuleStatus = "not_monitored"
	// StatusIncompatible: the sensor's category no longer matches the rule kind.
	StatusIncompatible RuleStatus = "incompatible"
	// StatusUnitUnresolved: a numeric rule created before unit binding that has
	// not been bound yet. It does not evaluate until it is bound or confirmed.
	StatusUnitUnresolved RuleStatus = "unit_unresolved"
	// StatusUnitChanged: the sensor now reports a different unit than the one the
	// threshold was bound to. The rule does not evaluate and resumes if the unit returns.
	StatusUnitChanged RuleStatus = "unit_changed"
)

// RuleView is a rule with its current status. Dormant rules (needs sharing, not
// monitored, incompatible) are kept, never deleted, and resume by themselves.
type RuleView struct {
	Rule
	Status RuleStatus
	// Category is set when the sensor is currently monitored.
	Category monitor.Category
}

// NewRule describes a rule to add.
type NewRule struct {
	Entity entities.ID
	Kind   Kind
	// Threshold is required for KindAbove and KindBelow and forbidden for KindState.
	Threshold *float64
	// Cooldown nil selects the category default; zero disables the cooldown. It
	// must be a whole number of seconds between 0 and MaxCooldown.
	Cooldown *time.Duration
	// Duration is how long the condition must hold; zero is immediate. Otherwise
	// a whole number of seconds between MinDuration and MaxDuration.
	Duration time.Duration
	// Clear is the re-arm value of a numeric rule: below the threshold of an
	// "above" rule, above the threshold of a "below" rule.
	Clear *float64
}

// Mark records that a hazard alert was delivered, so the same alert is not
// repeated after a restart. EpisodeKey is only an identity for "the same
// episode" (Home Assistant's last_changed of the violating state), never a claim
// about when anything began.
type Mark struct {
	RuleID      int64
	EpisodeKey  string
	DeliveredAt time.Time
}

// Errors. Their messages are what is written to the audit log, so none of them
// carries a sensor value, a secret or user-supplied text.
var (
	ErrNotConfigured    = errors.New("notifications are not configured: no channel has been set")
	ErrInvalidChannel   = errors.New("not a valid Discord channel ID")
	ErrInvalidEntity    = errors.New("not a valid entity ID")
	ErrInvalidKind      = errors.New("unknown rule kind")
	ErrInvalidThreshold = errors.New("invalid threshold")
	ErrInvalidCooldown  = errors.New("invalid cooldown")
	ErrNotShared        = fmt.Errorf("the entity is not shared with at least read access: %w", entities.ErrAccessDenied)
	ErrNotMonitored     = errors.New("the sensor is not currently monitored")
	ErrIncompatible     = errors.New("the rule kind does not fit the sensor category")
	ErrRuleLimit        = errors.New("notification rule limit reached")
	ErrRuleExists       = errors.New("a rule of this kind already exists for the entity")
	ErrRuleNotFound     = errors.New("no such notification rule")

	ErrInvalidDuration    = errors.New("invalid duration")
	ErrDurationNotAllowed = errors.New("this sensor type does not support a duration")
	ErrInvalidClear       = errors.New("invalid clear value")
	ErrClearNotAllowed    = errors.New("this rule does not support a clear value")
	ErrHazardImmediate    = errors.New("critical alerts are always immediate")
	ErrUnitUnsupported    = errors.New("the sensor reports a unit that is missing or not supported for this sensor type")
	ErrUnitBound          = errors.New("the rule is already bound to a unit")
)
