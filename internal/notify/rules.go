package notify

import (
	"time"

	"github.com/nepthass/nepthass/internal/entities"
	"github.com/nepthass/nepthass/internal/monitor"
)

// cachedRule is the part of a Rule the hot path needs. It is immutable.
type cachedRule struct {
	ID        int64
	Entity    entities.ID
	Kind      Kind
	Threshold float64
	// Clear is the re-arm value of a numeric rule (hysteresis), valid when HasClear.
	Clear    float64
	HasClear bool
	// Unit is the unit the threshold is bound to ("" on a state rule, and on a
	// numeric rule that predates unit binding, which then does not evaluate).
	Unit string
	// Duration is how long the condition must hold; zero is an immediate rule.
	Duration time.Duration
	Cooldown *time.Duration
}

// stateful reports whether the rule needs the ordered observation trail: a
// duration rule must know whether the condition was continuous, and a hysteresis
// rule must know the order of crossings. A plain immediate rule keeps the
// coalescing latch of v0.4.0.
func (r cachedRule) stateful() bool { return r.Duration > 0 || r.HasClear }

// ruleSet is an immutable snapshot of the enabled rules. The engine swaps whole
// snapshots atomically, so the monitor callback never takes a lock to read it
// and a configuration change can never be seen half applied.
type ruleSet struct {
	byEntity map[entities.ID][]cachedRule
	byID     map[int64]cachedRule
}

// newRuleSet keeps the enabled rules, at most MaxRules of them.
func newRuleSet(rules []Rule) *ruleSet {
	rs := &ruleSet{byEntity: map[entities.ID][]cachedRule{}, byID: map[int64]cachedRule{}}
	for _, r := range rules {
		if !r.Enabled || len(rs.byID) >= MaxRules {
			continue
		}
		c := cachedRule{ID: r.ID, Entity: r.Entity, Kind: r.Kind, Cooldown: r.Cooldown, Unit: r.Unit, Duration: r.Duration}
		if r.Threshold != nil {
			c.Threshold = *r.Threshold
		}
		if r.Clear != nil {
			c.Clear, c.HasClear = *r.Clear, true
		}
		rs.byEntity[r.Entity] = append(rs.byEntity[r.Entity], c)
		rs.byID[r.ID] = c
	}
	return rs
}

// reliable reports whether the sensor reports a real value: unavailable,
// unknown and unparseable states are not reliable.
func reliable(s monitor.Sensor) bool {
	switch s.Interpretation().Meaning {
	case monitor.MeaningUnavailable, monitor.MeaningUnknown, monitor.MeaningInvalid:
		return false
	}
	return true
}

// unitOK reports whether the sensor reports the unit the rule's threshold is
// bound to. A state rule has no unit. A numeric rule with no bound unit never
// matches, so a threshold is never evaluated against a unit it was not written for.
func unitOK(r cachedRule, s monitor.Sensor) bool {
	return r.Kind == KindState || (r.Unit != "" && r.Unit == s.Unit)
}

// supports reports whether a sensor category's declared capabilities allow the
// rule's shape: its kind, a duration, a clear value. A hazard (the water leak) is
// immediate-only, so a duration or clear value on it is never honored, even if it
// reached the database by some other route.
func supports(k Kind, duration time.Duration, hasClear bool, c monitor.Category) bool {
	if !compatible(k, c) {
		return false
	}
	spec, _ := monitor.SpecFor(c) // compatible() guarantees the row
	if duration > 0 {
		need := monitor.CapStateDuration
		if k != KindState {
			need = monitor.CapThresholdDuration
		}
		if !spec.Caps.Has(need) {
			return false
		}
	}
	return !hasClear || spec.Caps.Has(monitor.CapHysteresis)
}

// usable reports whether the rule can evaluate this sensor at all: the category
// supports the rule's shape and the unit matches. An unusable rule is dormant,
// never wrong.
func usable(r cachedRule, s monitor.Sensor) bool {
	return supports(r.Kind, r.Duration, r.HasClear, s.Category) && unitOK(r, s)
}

// reliableFor is reliable plus the unit guard: a reading in a different unit is
// not a reliable reading for this rule.
func reliableFor(r cachedRule, s monitor.Sensor) bool { return reliable(s) && unitOK(r, s) }

// alertMeaning is the state a state rule fires on, from the category's registry
// row. A numeric or unknown category has none.
func alertMeaning(c monitor.Category) (monitor.Meaning, bool) {
	s, ok := monitor.SpecFor(c)
	if !ok || s.Value != monitor.ValueBinary {
		return "", false
	}
	return s.AlertMeaning(), true
}

// violates reports whether the sensor currently satisfies the rule. It is false
// for an unreliable reading, for a reading in another unit, and for a sensor
// whose category does not fit the rule kind (a dormant rule never fires).
func violates(r cachedRule, s monitor.Sensor) bool {
	if !unitOK(r, s) {
		return false
	}
	in := s.Interpretation()
	switch r.Kind {
	case KindState:
		m, ok := alertMeaning(s.Category)
		return ok && in.Meaning == m
	case KindAbove:
		return s.Category.Numeric() && in.HasValue && in.Value > r.Threshold
	case KindBelow:
		return s.Category.Numeric() && in.HasValue && in.Value < r.Threshold
	}
	return false
}

// cleared reports whether the reading is past the rule's clear value, so a
// hysteresis rule may fire again: strictly below it for an "above" rule, strictly
// above it for a "below" rule. Equal does not clear.
func cleared(r cachedRule, s monitor.Sensor) bool {
	if !r.HasClear || !reliableFor(r, s) {
		return false
	}
	in := s.Interpretation()
	if !in.HasValue {
		return false
	}
	switch r.Kind {
	case KindAbove:
		return in.Value < r.Clear
	case KindBelow:
		return in.Value > r.Clear
	}
	return false
}

// enters decides whether one Change is a qualifying transition for a rule: the
// sensor was observed NOT violating and now violates. It needs no per-rule
// runtime state, so the monitor callback can compute it.
//
//   - A first observation (HadPrev false) and a removal never qualify.
//   - A reliable, non-violating previous state qualifies.
//   - An unreliable previous state (unavailable, unknown, invalid, or a reading in
//     another unit) does not qualify, with one deliberate exception: a hazard
//     sensor (a water leak) reaching its alert state fails loud (fromUnreliable is
//     true so the message can say the state was uncertain).
func enters(r cachedRule, c monitor.Change) (qualifies, fromUnreliable bool) {
	if c.Removed || !c.HadPrev || !violates(r, c.Cur) || violates(r, c.Prev) {
		return false, false
	}
	if reliableFor(r, c.Prev) {
		return true, false
	}
	if r.Kind == KindState && c.Cur.Category.Hazard() {
		return true, true
	}
	return false, false
}

// defaultCooldown is the cooldown of a category when the rule has no override:
// the registry value (an unknown category gets 15 minutes).
func defaultCooldown(c monitor.Category) time.Duration {
	if s, ok := monitor.SpecFor(c); ok {
		return s.Cooldown
	}
	return 15 * time.Minute
}

// cooldownOf is the effective cooldown: a rule override (zero disables the
// cooldown) takes precedence over the category default.
func cooldownOf(r cachedRule, c monitor.Category) time.Duration {
	if r.Cooldown != nil {
		return *r.Cooldown
	}
	return defaultCooldown(c)
}

// categoryNoun names a sensor when it has neither an alias nor a friendly name.
// An alert never falls back to the entity ID.
func categoryNoun(c monitor.Category) string {
	if s, ok := monitor.SpecFor(c); ok {
		return s.Noun
	}
	return "Sensor"
}
