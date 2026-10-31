package monitor

// Category is what a sensor measures or detects, derived from Home Assistant
// metadata only (domain and device_class), never from the entity ID text.
type Category string

const (
	CategoryTemperature Category = "temperature"
	CategoryHumidity    Category = "humidity"
	CategoryBattery     Category = "battery"     // numeric charge level
	CategoryBatteryLow  Category = "battery_low" // binary: on means low
	CategoryMotion      Category = "motion"
	CategoryDoor        Category = "door"
	CategoryWindow      Category = "window"
	CategoryLeak        Category = "leak" // device_class moisture
	CategoryGarageDoor  Category = "garage_door"
	CategoryOpening     Category = "opening"
)

// Domains NeptHass monitors.
const (
	DomainSensor       = "sensor"
	DomainBinarySensor = "binary_sensor"
)

// Classify returns the category for a domain and device class, and false when
// NeptHass does not monitor that kind of sensor. The registry is the only table.
func Classify(domain, deviceClass string) (Category, bool) {
	s, ok := Lookup(domain, deviceClass)
	return s.Category, ok
}

// Monitored reports whether a domain can contain monitored sensors at all.
func Monitored(domain string) bool {
	return domain == DomainSensor || domain == DomainBinarySensor
}

// Numeric reports whether the category carries a number rather than on/off.
func (c Category) Numeric() bool {
	s, ok := SpecFor(c)
	return ok && s.Value == ValueNumeric
}

// Hazard reports whether the category is a critical hazard: immediate-only
// alerts on the critical lane that are announced even at start-up.
func (c Category) Hazard() bool {
	s, ok := SpecFor(c)
	return ok && s.Hazard
}

// Label is a short human-readable name.
func (c Category) Label() string {
	if s, ok := SpecFor(c); ok {
		return s.Label
	}
	return string(c)
}
