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
	Cooldown  *time.Duration
}

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
		c := cachedRule{ID: r.ID, Entity: r.Entity, Kind: r.Kind, Cooldown: r.Cooldown}
		if r.Threshold != nil {
			c.Threshold = *r.Threshold
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

// alertMeaning is the state a state rule fires on, per binary category.
func alertMeaning(c monitor.Category) (monitor.Meaning, bool) {
	switch c {
	case monitor.CategoryDoor, monitor.CategoryWindow:
		return monitor.MeaningOpen, true
	case monitor.CategoryMotion:
		return monitor.MeaningDetected, true
	case monitor.CategoryLeak:
		return monitor.MeaningWet, true
	case monitor.CategoryBatteryLow:
		return monitor.MeaningLow, true
	}
	return "", false
}

// violates reports whether the sensor currently satisfies the rule. It is false
// for an unreliable reading and for a sensor whose category does not fit the
// rule kind (a dormant, incompatible rule never fires).
func violates(r cachedRule, s monitor.Sensor) bool {
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

// enters decides whether one Change is a qualifying transition for a rule: the
// sensor was observed NOT violating and now violates. It needs no per-rule
// runtime state, so the monitor callback can compute it.
//
//   - A first observation (HadPrev false) and a removal never qualify.
//   - A reliable, non-violating previous state qualifies.
//   - An unreliable previous state (unavailable, unknown, invalid) does not
//     qualify, with one deliberate exception: a leak sensor reaching wet fails
//     loud (fromUnreliable is true so the message can say the state was uncertain).
func enters(r cachedRule, c monitor.Change) (qualifies, fromUnreliable bool) {
	if c.Removed || !c.HadPrev || !violates(r, c.Cur) || violates(r, c.Prev) {
		return false, false
	}
	if reliable(c.Prev) {
		return true, false
	}
	if r.Kind == KindState && c.Cur.Category == monitor.CategoryLeak {
		return true, true
	}
	return false, false
}

// defaultCooldown is the cooldown of a category when the rule has no override.
// Humidity is not in the approved list and follows temperature; the binary
// battery-low sensor follows battery.
func defaultCooldown(c monitor.Category) time.Duration {
	switch c {
	case monitor.CategoryMotion:
		return 5 * time.Minute
	case monitor.CategoryDoor, monitor.CategoryWindow:
		return time.Minute
	case monitor.CategoryBattery, monitor.CategoryBatteryLow:
		return 6 * time.Hour
	}
	return 15 * time.Minute // leak, temperature, humidity
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
	switch c {
	case monitor.CategoryDoor:
		return "Door sensor"
	case monitor.CategoryWindow:
		return "Window sensor"
	case monitor.CategoryMotion:
		return "Motion sensor"
	case monitor.CategoryLeak:
		return "Leak sensor"
	case monitor.CategoryBatteryLow, monitor.CategoryBattery:
		return "Battery sensor"
	case monitor.CategoryTemperature:
		return "Temperature sensor"
	case monitor.CategoryHumidity:
		return "Humidity sensor"
	}
	return "Sensor"
}
