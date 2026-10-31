package monitor

import "time"

// ValueKind says whether a sensor class carries an on/off state or a number.
type ValueKind uint8

const (
	// ValueBinary is a binary_sensor class: the state is "on" or "off".
	ValueBinary ValueKind = iota + 1
	// ValueNumeric is a sensor class: the state is a plain decimal number.
	ValueNumeric
)

// Caps is the set of notification rule capabilities a class supports. It is
// declared explicitly per class, never derived from a name, so an unknown or
// future class can never accidentally gain a capability.
type Caps uint8

const (
	// CapState: alert when the declared alert state is entered (binary classes).
	CapState Caps = 1 << iota
	// CapStateDuration: alert when the alert state has held for a duration.
	CapStateDuration
	// CapThreshold: alert above or below a number (numeric classes).
	CapThreshold
	// CapThresholdDuration: the same, held for a duration.
	CapThresholdDuration
	// CapHysteresis: a separate clear value for re-arming a numeric threshold rule.
	CapHysteresis
)

// Has reports whether every capability in want is present.
func (c Caps) Has(want Caps) bool { return c&want == want }

// Range is the plausible interval of a numeric class; a value outside it is invalid.
type Range struct{ Min, Max float64 }

// Spec is everything NeptHass knows about one supported Home Assistant sensor
// class. The registry below is the single authoritative source: nothing outside
// this file switches on a category name to decide behavior.
type Spec struct {
	Category    Category
	Domain      string // "sensor" or "binary_sensor"
	DeviceClass string // Home Assistant device_class

	// Label names the category ("Door"); Noun names an unnamed sensor in a
	// message ("Door sensor"); Phrase words a state rule ("when it opens").
	Label, Noun, Phrase string

	// Alert and Held word a binary class's alert ("is now open") and its duration alert
	// ("has been open for"). They belong to the class, never to a shared meaning, so a new
	// row cannot inherit another class's wording.
	Alert, Held string

	Value ValueKind

	// Binary classes: AlertState is the raw state that means "alert"; On and Off
	// are the meanings of the two raw states (Home Assistant's documented contract
	// for the class, not an inference from its name).
	AlertState string
	On, Off    Meaning

	// Numeric classes: Range bounds plausible values (nil: unbounded) and Units
	// lists the units a numeric rule may be bound to. A class with exactly one
	// unit can be bound without asking; a class with several cannot.
	Range *Range
	Units []string

	// Cooldown is the default cooldown between two alerts of one rule.
	Cooldown time.Duration

	// Hazard marks a class whose alerts are critical: immediate only (no
	// duration, no hysteresis), critical lane, fail-loud from an unreliable
	// state, and announced at start-up. It is explicit per class and is never
	// inferred or configurable.
	Hazard bool

	Caps Caps
}

// AlertMeaning is the meaning of the state that makes a binary rule fire. It is
// empty for a numeric class.
func (s Spec) AlertMeaning() Meaning {
	if s.Value != ValueBinary {
		return ""
	}
	if s.AlertState == "off" {
		return s.Off
	}
	return s.On
}

// SupportsUnit reports whether unit is one of the class's documented units.
func (s Spec) SupportsUnit(unit string) bool {
	for _, u := range s.Units {
		if u == unit {
			return true
		}
	}
	return false
}

// SingleUnit returns the only unit of the class and true when there is exactly
// one, in which case a legacy rule can be bound to it without ambiguity.
func (s Spec) SingleUnit() (string, bool) {
	if len(s.Units) == 1 {
		return s.Units[0], true
	}
	return "", false
}

const (
	stateRuleCaps   = CapState | CapStateDuration
	hazardRuleCaps  = CapState
	numericRuleCaps = CapThreshold | CapThresholdDuration | CapHysteresis
)

var percent = &Range{Min: 0, Max: 100}

// registry lists every supported class. Order is the display order. Extending
// monitoring to a new class is one row here plus its tests; the invariants test
// (registry_test.go) refuses a row that is incomplete or inconsistent.
//
// The binary meanings follow Home Assistant's documented contract in
// homeassistant/components/binary_sensor/const.py: door, window, garage_door and
// opening: on = open; motion: on = motion detected; moisture: on = wet; battery:
// on = low. Numeric classes follow sensor/const.py (temperature: °C, °F or K;
// humidity and battery: %).
var registry = []Spec{
	{Category: CategoryTemperature, Domain: DomainSensor, DeviceClass: "temperature", Label: "Temperature", Noun: "Temperature sensor",
		Value: ValueNumeric, Units: []string{"°C", "°F", "K"}, Cooldown: 15 * time.Minute, Caps: numericRuleCaps},
	{Category: CategoryHumidity, Domain: DomainSensor, DeviceClass: "humidity", Label: "Humidity", Noun: "Humidity sensor",
		Value: ValueNumeric, Range: percent, Units: []string{"%"}, Cooldown: 15 * time.Minute, Caps: numericRuleCaps},
	{Category: CategoryBattery, Domain: DomainSensor, DeviceClass: "battery", Label: "Battery", Noun: "Battery sensor",
		Value: ValueNumeric, Range: percent, Units: []string{"%"}, Cooldown: 6 * time.Hour, Caps: numericRuleCaps},
	{Category: CategoryBatteryLow, Domain: DomainBinarySensor, DeviceClass: "battery", Label: "Battery low", Noun: "Battery sensor",
		Phrase: "when its battery is low", Alert: "reports a low battery", Held: "has reported a low battery for", Value: ValueBinary, AlertState: "on", On: MeaningLow, Off: MeaningNormal, Cooldown: 6 * time.Hour, Caps: stateRuleCaps},
	{Category: CategoryMotion, Domain: DomainBinarySensor, DeviceClass: "motion", Label: "Motion", Noun: "Motion sensor",
		Phrase: "when motion is detected", Alert: "detected motion", Held: "has been detecting motion for", Value: ValueBinary, AlertState: "on", On: MeaningDetected, Off: MeaningClear, Cooldown: 5 * time.Minute, Caps: stateRuleCaps},
	{Category: CategoryDoor, Domain: DomainBinarySensor, DeviceClass: "door", Label: "Door", Noun: "Door sensor",
		Phrase: "when it opens", Alert: "is now open", Held: "has been open for", Value: ValueBinary, AlertState: "on", On: MeaningOpen, Off: MeaningClosed, Cooldown: time.Minute, Caps: stateRuleCaps},
	{Category: CategoryWindow, Domain: DomainBinarySensor, DeviceClass: "window", Label: "Window", Noun: "Window sensor",
		Phrase: "when it opens", Alert: "is now open", Held: "has been open for", Value: ValueBinary, AlertState: "on", On: MeaningOpen, Off: MeaningClosed, Cooldown: time.Minute, Caps: stateRuleCaps},
	{Category: CategoryGarageDoor, Domain: DomainBinarySensor, DeviceClass: "garage_door", Label: "Garage door", Noun: "Garage door sensor",
		Phrase: "when it opens", Alert: "is now open", Held: "has been open for", Value: ValueBinary, AlertState: "on", On: MeaningOpen, Off: MeaningClosed, Cooldown: time.Minute, Caps: stateRuleCaps},
	{Category: CategoryOpening, Domain: DomainBinarySensor, DeviceClass: "opening", Label: "Opening", Noun: "Opening sensor",
		Phrase: "when it opens", Alert: "is now open", Held: "has been open for", Value: ValueBinary, AlertState: "on", On: MeaningOpen, Off: MeaningClosed, Cooldown: time.Minute, Caps: stateRuleCaps},
	{Category: CategoryLeak, Domain: DomainBinarySensor, DeviceClass: "moisture", Label: "Water leak", Noun: "Leak sensor",
		Phrase: "when it gets wet", Alert: "is wet (water detected)", Held: "has been wet for", Value: ValueBinary, AlertState: "on", On: MeaningWet, Off: MeaningDry, Cooldown: 15 * time.Minute, Hazard: true, Caps: hazardRuleCaps},
}

type classKey struct{ domain, deviceClass string }

var (
	byClass    = map[classKey]Spec{}
	byCategory = map[Category]Spec{}
)

func init() {
	for _, s := range registry {
		byClass[classKey{s.Domain, s.DeviceClass}] = s
		byCategory[s.Category] = s
	}
}

// Lookup returns the spec for a Home Assistant domain and device class. An
// unknown pair is not supported: there is no generic fallback.
func Lookup(domain, deviceClass string) (Spec, bool) {
	s, ok := byClass[classKey{domain, deviceClass}]
	return s, ok
}

// SpecFor returns the spec of a category.
func SpecFor(c Category) (Spec, bool) {
	s, ok := byCategory[c]
	return s, ok
}

// Specs returns a copy of the registry in display order.
func Specs() []Spec {
	out := make([]Spec, len(registry))
	copy(out, registry)
	return out
}
